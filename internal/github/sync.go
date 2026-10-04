package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nisagwn/paas/internal/store"
)

// DefaultSyncInterval is how often installations are resynced with GitHub.
const DefaultSyncInterval = 30 * time.Minute

// InstallationSyncer mirrors the App's installations and their repositories
// into the store. Webhooks keep the mirror current; the periodic resync
// repairs missed deliveries and installations made before the platform knew
// the App. Team claims are never touched.
type InstallationSyncer struct {
	App      *App
	Store    *store.Store
	Interval time.Duration // DefaultSyncInterval when zero
	Log      *slog.Logger
}

// Run syncs once right away, then every Interval until ctx ends. Failures
// are logged; the mirror keeps its last state.
func (s *InstallationSyncer) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSyncInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			s.log().Error("github app: installation resync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync rewrites the mirror from GitHub: every installation is upserted, its
// repositories replaced, and installations GitHub no longer reports are
// deleted. Deleting happens only after the full list was read, so a failed
// listing never forgets an installation.
func (s *InstallationSyncer) Sync(ctx context.Context) error {
	installs, err := s.App.ListInstallations(ctx)
	if err != nil {
		return fmt.Errorf("list installations: %w", err)
	}
	seen := map[int64]bool{}
	var errs []error
	repos := 0
	for _, in := range installs {
		row, ok := installationRow(in)
		if !ok {
			s.log().Warn("github app: installation on unsupported account type skipped",
				"installation", in.ID, "account", in.Account.Login, "type", in.Account.Type)
			continue
		}
		seen[in.ID] = true
		if err := s.Store.UpsertInstallation(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("installation %d: %w", in.ID, err))
			continue
		}
		// A suspended installation cannot mint tokens; its repositories
		// stay as they were until it is unsuspended.
		if row.Suspended {
			continue
		}
		list, err := s.App.ListInstallationRepos(ctx, in.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("installation %d: list repositories: %w", in.ID, err))
			continue
		}
		if err := s.Store.SetInstallationRepos(ctx, in.ID, installationRepos(in.ID, list)); err != nil {
			errs = append(errs, fmt.Errorf("installation %d: %w", in.ID, err))
			continue
		}
		repos += len(list)
	}

	known, err := s.Store.ListInstallations(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	removed := 0
	for _, in := range known {
		if seen[in.ID] {
			continue
		}
		if err := s.Store.DeleteInstallation(ctx, in.ID); err != nil {
			errs = append(errs, fmt.Errorf("installation %d: %w", in.ID, err))
			continue
		}
		removed++
	}
	s.log().Info("github app: installations synced", "installations", len(seen), "repos", repos, "removed", removed)
	return errors.Join(errs...)
}

func (s *InstallationSyncer) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// installationRow converts GitHub's installation to a store row. Accounts
// other than users and organizations (enterprise installations) are not
// supported.
func installationRow(in AppInstallation) (store.Installation, bool) {
	if in.Account.Type != "User" && in.Account.Type != "Organization" {
		return store.Installation{}, false
	}
	return store.Installation{
		ID: in.ID, AccountLogin: in.Account.Login, AccountType: in.Account.Type,
		Suspended: in.SuspendedAt != nil,
	}, true
}

func installationRepos(id int64, list []Repository) []store.InstallationRepo {
	out := make([]store.InstallationRepo, 0, len(list))
	for _, r := range list {
		out = append(out, store.InstallationRepo{InstallationID: id, RepoID: r.ID, FullName: r.FullName, Private: r.Private})
	}
	return out
}
