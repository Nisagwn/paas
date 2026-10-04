package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 15: the GitHub App's installation events. GitHub sends every event of
// the App to its one webhook URL, so these arrive on /webhooks/github next to
// push and pull_request.

// InstallationStore is the part of *store.Store the installation events
// write to.
type InstallationStore interface {
	UpsertInstallation(ctx context.Context, in store.Installation) error
	DeleteInstallation(ctx context.Context, id int64) error
	SetInstallationSuspended(ctx context.Context, id int64, suspended bool) error
	SetInstallationRepos(ctx context.Context, id int64, repos []store.InstallationRepo) error
	AddInstallationRepos(ctx context.Context, id int64, repos []store.InstallationRepo) error
	RemoveInstallationRepos(ctx context.Context, id int64, repoIDs []int64) error
}

// maxBody bounds a delivery; an installation on many repositories lists
// them all, GitHub caps payloads at 25 MB.
const maxBody = 25 << 20

type installationPayload struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	SuspendedAt *string `json:"suspended_at"`
}

type repoPayload struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// InstallationEvent holds the fields of "installation" and
// "installation_repositories" payloads that paas needs.
type InstallationEvent struct {
	Action       string              `json:"action"`
	Installation installationPayload `json:"installation"`
	// installation (created): the repositories granted at install time.
	Repositories []repoPayload `json:"repositories"`
	// installation_repositories (added / removed).
	RepositoriesAdded   []repoPayload `json:"repositories_added"`
	RepositoriesRemoved []repoPayload `json:"repositories_removed"`
}

// errInvalidPayload marks a delivery that cannot be applied as sent (400).
var errInvalidPayload = errors.New("invalid installation payload")

// IsInstallationEvent reports whether HandleInstallationEvent handles event
// (the X-GitHub-Event header).
func IsInstallationEvent(event string) bool {
	return event == "installation" || event == "installation_repositories"
}

// HandleInstallationEvent applies an installation or
// installation_repositories delivery to the store. It returns a short
// description of what it did; ErrIgnored for actions paas does not track.
func HandleInstallationEvent(ctx context.Context, st InstallationStore, event string, body []byte) (string, error) {
	var ev InstallationEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", fmt.Errorf("%w: %v", errInvalidPayload, err)
	}
	in := ev.Installation
	if in.ID == 0 {
		return "", fmt.Errorf("%w: missing installation id", errInvalidPayload)
	}
	if ev.Action == "deleted" && event == "installation" {
		return "installation deleted", st.DeleteInstallation(ctx, in.ID)
	}
	if in.Account.Type != "User" && in.Account.Type != "Organization" {
		return "", fmt.Errorf("%w: installation %d on a %q account", ErrIgnored, in.ID, in.Account.Type)
	}
	// Every other action carries the full installation; upserting it first
	// also repairs a missed "created" delivery.
	row := store.Installation{
		ID: in.ID, AccountLogin: in.Account.Login, AccountType: in.Account.Type,
		Suspended: in.SuspendedAt != nil,
	}

	switch event + "." + ev.Action {
	case "installation.created":
		if err := st.UpsertInstallation(ctx, row); err != nil {
			return "", err
		}
		return fmt.Sprintf("installation created (%d repos)", len(ev.Repositories)),
			st.SetInstallationRepos(ctx, in.ID, repoRows(in.ID, ev.Repositories))
	case "installation.new_permissions_accepted":
		return "installation updated", st.UpsertInstallation(ctx, row)
	case "installation.suspend", "installation.unsuspend":
		row.Suspended = ev.Action == "suspend"
		err := st.SetInstallationSuspended(ctx, in.ID, row.Suspended)
		if errors.Is(err, store.ErrNotFound) {
			err = st.UpsertInstallation(ctx, row)
		}
		return "installation " + ev.Action + "ed", err
	case "installation_repositories.added", "installation_repositories.removed":
		if err := st.UpsertInstallation(ctx, row); err != nil {
			return "", err
		}
		if ev.Action == "added" {
			return fmt.Sprintf("%d repos added", len(ev.RepositoriesAdded)),
				st.AddInstallationRepos(ctx, in.ID, repoRows(in.ID, ev.RepositoriesAdded))
		}
		ids := make([]int64, 0, len(ev.RepositoriesRemoved))
		for _, r := range ev.RepositoriesRemoved {
			ids = append(ids, r.ID)
		}
		return fmt.Sprintf("%d repos removed", len(ids)), st.RemoveInstallationRepos(ctx, in.ID, ids)
	}
	return "", fmt.Errorf("%w: %s action %q", ErrIgnored, event, ev.Action)
}

func repoRows(id int64, repos []repoPayload) []store.InstallationRepo {
	out := make([]store.InstallationRepo, 0, len(repos))
	for _, r := range repos {
		out = append(out, store.InstallationRepo{InstallationID: id, RepoID: r.ID, FullName: r.FullName, Private: r.Private})
	}
	return out
}

// Installations serves the installation events of POST /webhooks/github
// and passes every other request to Next (the API, which handles push,
// pull_request and ping). Signatures are checked as for every delivery.
type Installations struct {
	Secret string
	Store  InstallationStore
	Log    *slog.Logger
	Next   http.Handler
}

func (h *Installations) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	if r.Method != http.MethodPost || r.URL.Path != "/webhooks/github" || !IsInstallationEvent(event) {
		h.Next.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "payload too large"})
		return
	}
	if err := VerifySignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	result, err := HandleInstallationEvent(r.Context(), h.Store, event, body)
	switch {
	case errors.Is(err, ErrIgnored):
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored", "reason": err.Error()})
	case errors.Is(err, errInvalidPayload):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case err != nil:
		h.log().Error("github app: installation event", "event", event, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	default:
		h.log().Info("github app: "+result, "event", event)
		writeJSON(w, http.StatusOK, map[string]string{"result": result})
	}
}

func (h *Installations) log() *slog.Logger {
	if h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
