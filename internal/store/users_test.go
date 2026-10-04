package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// Apps that existed before migration 007 move to the "default" team.
func TestMigrationAssignsDefaultTeam(t *testing.T) {
	testdb.Open(t) // only for the skip check and a clean schema
	url := os.Getenv("PAAS_TEST_DATABASE_URL")
	ctx := context.Background()
	raw, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.MigrateUntil(ctx, "006_zzz"); err != nil {
		t.Fatal(err)
	}
	if err := st.Exec(ctx, `INSERT INTO apps (name, repo_full_name) VALUES ('old', 'nisagwn/old')`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	def, err := st.GetTeamBySlug(ctx, store.DefaultTeam)
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.GetAppByName(ctx, "old")
	if err != nil || app.TeamID != def.ID {
		t.Fatalf("old app team = %d (%v), want default team %d", app.TeamID, err, def.ID)
	}
	// CreateApp without a team also lands in "default".
	if a, err := st.CreateApp(ctx, "new", "nisagwn/new", "main"); err != nil || a.TeamID != def.ID {
		t.Fatalf("CreateApp team = %d (%v)", a.TeamID, err)
	}
}

func TestUsersAndRename(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	u, err := st.UpsertUser(ctx, 1, "Alice", "Alice A", "https://a/1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.UpsertUser(ctx, 1, "alice2", "Alice B", "")
	if err != nil || again.ID != u.ID || again.Login != "alice2" {
		t.Fatalf("re-login: %+v %v", again, err)
	}
	// Another account claims the freed name later; then the first renames back.
	bob, _ := st.UpsertUser(ctx, 2, "bob", "", "")
	if _, err := st.UpsertUser(ctx, 1, "BOB", "", ""); err != nil {
		t.Fatalf("login taken by a stale row must not block: %v", err)
	}
	if got, _ := st.UserByLogin(ctx, "bob"); got.ID != u.ID {
		t.Fatalf("bob now = %+v", got)
	}
	if got, _ := st.GetUser(ctx, bob.ID); got.Login == "bob" {
		t.Fatal("stale login kept")
	}
	if _, err := st.UserByLogin(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown login: %v", err)
	}
}

func TestTeamsMembersAndApps(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	bob, _ := st.UpsertUser(ctx, 2, "bob", "", "")

	team, err := st.CreateTeam(ctx, "web", "Web", alice.ID)
	if err != nil || team.Role != store.RoleOwner {
		t.Fatalf("create team: %+v %v", team, err)
	}
	if _, err := st.CreateTeam(ctx, "web", "Again", alice.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate slug: %v", err)
	}
	if ok, _ := st.HasTeams(ctx, bob.ID); ok {
		t.Fatal("bob has no team yet")
	}
	if err := st.SetMember(ctx, team.ID, bob.ID, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if role, _ := st.TeamRole(ctx, bob.ID, team.ID); role != store.RoleViewer {
		t.Fatalf("bob role = %q", role)
	}
	if err := st.SetMember(ctx, team.ID, bob.ID, "admin"); err == nil {
		t.Fatal("invalid role accepted")
	}

	// The last owner can be neither demoted nor removed.
	if err := st.SetMember(ctx, team.ID, alice.ID, store.RoleMember); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("demote last owner: %v", err)
	}
	if err := st.RemoveMember(ctx, team.ID, alice.ID); !errors.Is(err, store.ErrLastOwner) {
		t.Fatalf("remove last owner: %v", err)
	}
	st.SetMember(ctx, team.ID, bob.ID, store.RoleOwner)
	if err := st.RemoveMember(ctx, team.ID, alice.ID); err != nil {
		t.Fatalf("remove one of two owners: %v", err)
	}
	if err := st.RemoveMember(ctx, team.ID, alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remove non-member: %v", err)
	}
	members, _ := st.TeamMembers(ctx, team.ID)
	if len(members) != 1 || members[0].Login != "bob" || members[0].Role != store.RoleOwner {
		t.Fatalf("members = %+v", members)
	}

	// Apps are listed per user's teams.
	app, err := st.CreateAppInTeam(ctx, team.ID, "site", "nisagwn/site", "main")
	if err != nil || app.TeamID != team.ID {
		t.Fatalf("create app in team: %+v %v", app, err)
	}
	st.CreateApp(ctx, "other", "nisagwn/other", "main") // default team
	if apps, _ := st.AppsForUser(ctx, bob.ID); len(apps) != 1 || apps[0].Name != "site" {
		t.Fatalf("bob's apps = %+v", apps)
	}
	if apps, _ := st.AppsForUser(ctx, alice.ID); len(apps) != 0 {
		t.Fatalf("alice left the team but sees %+v", apps)
	}
	if teams, _ := st.TeamsForUser(ctx, bob.ID); len(teams) != 1 || teams[0].Slug != "web" {
		t.Fatalf("bob's teams = %+v", teams)
	}
	if all, _ := st.ListTeams(ctx); len(all) != 2 {
		t.Fatalf("all teams = %+v", all)
	}
}

func TestRoleAllows(t *testing.T) {
	for _, c := range []struct {
		have, need string
		ok         bool
	}{
		{"owner", "member", true}, {"member", "member", true}, {"viewer", "member", false},
		{"viewer", "viewer", true}, {"", "viewer", false}, {"member", "owner", false},
	} {
		if got := store.RoleAllows(c.have, c.need); got != c.ok {
			t.Errorf("RoleAllows(%q, %q) = %v", c.have, c.need, got)
		}
	}
}

func TestAPITokens(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	alice, _ := st.UpsertUser(ctx, 1, "alice", "", "")
	bob, _ := st.UpsertUser(ctx, 2, "bob", "", "")
	hash := func(c byte) string {
		b := make([]byte, 64)
		for i := range b {
			b[i] = c
		}
		return string(b)
	}

	tok, err := st.CreateAPIToken(ctx, alice.ID, " ci ", hash('a'), "paas_abcdef", nil)
	if err != nil || tok.Name != "ci" {
		t.Fatalf("create: %+v %v", tok, err)
	}
	if _, err := st.CreateAPIToken(ctx, alice.ID, "dup", hash('a'), "paas_abcdef", nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate hash: %v", err)
	}
	u, err := st.UserByTokenHash(ctx, hash('a'))
	if err != nil || u.ID != alice.ID {
		t.Fatalf("lookup: %+v %v", u, err)
	}
	if list, _ := st.APITokens(ctx, alice.ID); len(list) != 1 || list[0].LastUsedAt == nil {
		t.Fatalf("last_used_at not recorded: %+v", list)
	}

	past := time.Now().Add(-time.Minute)
	st.CreateAPIToken(ctx, alice.ID, "old", hash('b'), "paas_old", &past)
	if _, err := st.UserByTokenHash(ctx, hash('b')); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("expired token: %v", err)
	}
	if _, err := st.UserByTokenHash(ctx, hash('c')); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}

	// Revocation is per user: bob cannot revoke alice's token.
	if err := st.RevokeAPIToken(ctx, bob.ID, tok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke someone else's: %v", err)
	}
	if err := st.RevokeAPIToken(ctx, alice.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserByTokenHash(ctx, hash('a')); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked token still works: %v", err)
	}
}
