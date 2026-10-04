package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func TestGitHubInstallations(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()

	if _, err := st.InstallationForRepo(ctx, "acme/web"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("before install: %v, want ErrNotFound", err)
	}
	if err := st.UpsertInstallation(ctx, store.Installation{ID: 42, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetInstallationRepos(ctx, 42, []store.InstallationRepo{
		{RepoID: 1, FullName: "acme/web"}, {RepoID: 2, FullName: "acme/api", Private: true},
	}); err != nil {
		t.Fatal(err)
	}
	if id, err := st.InstallationForRepo(ctx, "ACME/Web"); err != nil || id != 42 {
		t.Fatalf("lookup is case-insensitive: %d, %v", id, err)
	}

	// Unclaimed installations offer nothing for import.
	team, err := st.CreateTeam(ctx, "web", "Web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if repos, _ := st.ImportableRepos(ctx, []int64{team.ID}); len(repos) != 0 {
		t.Fatalf("unclaimed: %+v", repos)
	}
	if err := st.ClaimInstallation(ctx, 42, team.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAppInTeam(ctx, team.ID, "web", "acme/web", "main"); err != nil {
		t.Fatal(err)
	}
	repos, err := st.ImportableRepos(ctx, []int64{team.ID})
	if err != nil || len(repos) != 2 || repos[0].FullName != "acme/api" || repos[1].App != "web" || repos[0].AccountLogin != "acme" {
		t.Fatalf("importable: %+v, %v", repos, err)
	}

	// installation_repositories events, suspend, uninstall.
	st.AddInstallationRepos(ctx, 42, []store.InstallationRepo{{RepoID: 3, FullName: "acme/docs"}})
	st.RemoveInstallationRepos(ctx, 42, []int64{1})
	if repos, _ := st.ImportableRepos(ctx, []int64{team.ID}); len(repos) != 2 || repos[1].FullName != "acme/docs" {
		t.Fatalf("after add/remove: %+v", repos)
	}
	st.SetInstallationSuspended(ctx, 42, true)
	if _, err := st.InstallationForRepo(ctx, "acme/api"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("suspended: %v", err)
	}
	// Seen again (e.g. a resync): the claim survives.
	st.UpsertInstallation(ctx, store.Installation{ID: 42, AccountLogin: "acme", AccountType: "Organization"})
	if in, _ := st.GetInstallation(ctx, 42); in.TeamID == nil || *in.TeamID != team.ID || in.Suspended {
		t.Fatalf("after upsert: %+v", in)
	}
	if err := st.DeleteInstallation(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetInstallation(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := st.ClaimInstallation(ctx, 42, team.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim missing: %v", err)
	}
}
