package github_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nisagwn/paas/internal/github"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func TestInstallationSync(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	f := newFakeApp(t, st)

	// The mirror before: installation 1 claimed by a team with a repo that
	// was since removed on GitHub; installation 99 was uninstalled.
	team, err := st.CreateTeam(ctx, "web", "Web", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []store.Installation{
		{ID: 1, AccountLogin: "acme-old", AccountType: "Organization"},
		{ID: 99, AccountLogin: "gone", AccountType: "User"},
	} {
		if err := st.UpsertInstallation(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ClaimInstallation(ctx, 1, team.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetInstallationRepos(ctx, 1, []store.InstallationRepo{{RepoID: 5, FullName: "acme/removed"}}); err != nil {
		t.Fatal(err)
	}

	f.installs = []map[string]any{
		{"id": 1, "account": map[string]string{"login": "acme", "type": "Organization"}},
		{"id": 2, "account": map[string]string{"login": "nisa", "type": "User"}, "suspended_at": "2026-10-01T00:00:00Z"},
		{"id": 3, "account": map[string]string{"login": "corp", "type": "Enterprise"}},
	}
	f.repos[1] = []github.Repository{{ID: 10, FullName: "acme/web"}, {ID: 11, FullName: "acme/api", Private: true}}
	f.repos[2] = []github.Repository{{ID: 20, FullName: "nisa/blog"}}

	s := &github.InstallationSyncer{App: f.app, Store: st}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	in, err := st.GetInstallation(ctx, 1)
	if err != nil || in.AccountLogin != "acme" || in.TeamID == nil || *in.TeamID != team.ID {
		t.Fatalf("installation 1 = %+v, %v (team claim must survive)", in, err)
	}
	repos, err := st.ImportableRepos(ctx, []int64{team.ID})
	if err != nil || len(repos) != 2 || repos[0].FullName != "acme/api" || !repos[0].Private || repos[1].FullName != "acme/web" {
		t.Fatalf("repos of installation 1 = %+v, %v", repos, err)
	}
	if in, err := st.GetInstallation(ctx, 2); err != nil || !in.Suspended {
		t.Fatalf("installation 2 = %+v, %v, want suspended", in, err)
	}
	if f.minted(2) != 0 {
		t.Fatal("token minted for a suspended installation")
	}
	for _, id := range []int64{3, 99} {
		if _, err := st.GetInstallation(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("installation %d: %v, want ErrNotFound", id, err)
		}
	}

	// RepoToken finds the installation through the store.
	if tok, err := f.app.RepoToken(ctx, "acme/web"); err != nil || tok != "inst-1-1" || f.lookups != 0 {
		t.Fatalf("RepoToken = %q, %v (%d GitHub lookups)", tok, err, f.lookups)
	}

	// A failed listing deletes nothing.
	f.installs = nil
	f.rejectsExpected = true
	f.app.Key = otherKey(t)
	if err := s.Sync(ctx); err == nil {
		t.Fatal("sync with a rejected JWT succeeded")
	}
	if _, err := st.GetInstallation(ctx, 1); err != nil {
		t.Fatalf("installation 1 after a failed sync: %v", err)
	}
}
