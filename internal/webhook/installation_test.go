package webhook_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
	"github.com/nisagwn/paas/internal/webhook"
)

const secret = "s3cret"

type harness struct {
	t      *testing.T
	st     *store.Store
	h      http.Handler
	passed []string // events handed to Next
}

func newHarness(t *testing.T) *harness {
	hs := &harness{t: t, st: testdb.Open(t)}
	hs.h = &webhook.Installations{Secret: secret, Store: hs.st,
		Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hs.passed = append(hs.passed, r.Header.Get("X-GitHub-Event"))
			w.WriteHeader(http.StatusTeapot)
		})}
	return hs
}

func (hs *harness) deliver(event, body string) *httptest.ResponseRecorder {
	return hs.deliverSigned(event, body, webhook.Sign(secret, []byte(body)))
}

func (hs *harness) deliverSigned(event, body, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func (hs *harness) expect(rec *httptest.ResponseRecorder, code int) {
	hs.t.Helper()
	if rec.Code != code {
		hs.t.Fatalf("status %d, want %d: %s", rec.Code, code, rec.Body)
	}
}

func (hs *harness) installationFor(repo string) (int64, error) {
	return hs.st.InstallationForRepo(context.Background(), repo)
}

const installation = `"installation": {"id": 42, "account": {"login": "acme", "type": "Organization"}, "suspended_at": null}`

func TestInstallationEvents(t *testing.T) {
	hs := newHarness(t)
	ctx := context.Background()

	// created: the installation and the repositories granted at install time.
	hs.expect(hs.deliver("installation", `{"action": "created", `+installation+`,
		"repositories": [{"id": 1, "full_name": "acme/web", "private": false},
		                 {"id": 2, "full_name": "acme/api", "private": true}]}`), http.StatusOK)
	in, err := hs.st.GetInstallation(ctx, 42)
	if err != nil || in.AccountLogin != "acme" || in.AccountType != "Organization" || in.Suspended {
		t.Fatalf("installation = %+v, %v", in, err)
	}
	if id, err := hs.installationFor("acme/api"); err != nil || id != 42 {
		t.Fatalf("acme/api: %d, %v", id, err)
	}

	// A team claims it (setup URL); no event may drop the claim.
	team, err := hs.st.CreateTeam(ctx, "web", "Web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.st.ClaimInstallation(ctx, 42, team.ID); err != nil {
		t.Fatal(err)
	}

	// installation_repositories: added / removed.
	hs.expect(hs.deliver("installation_repositories", `{"action": "added", `+installation+`,
		"repository_selection": "selected",
		"repositories_added": [{"id": 3, "full_name": "acme/docs", "private": false}],
		"repositories_removed": []}`), http.StatusOK)
	hs.expect(hs.deliver("installation_repositories", `{"action": "removed", `+installation+`,
		"repository_selection": "selected", "repositories_added": [],
		"repositories_removed": [{"id": 1, "full_name": "acme/web"}]}`), http.StatusOK)
	repos, err := hs.st.ImportableRepos(ctx, []int64{team.ID})
	if err != nil || len(repos) != 2 || repos[0].FullName != "acme/api" || repos[1].FullName != "acme/docs" {
		t.Fatalf("repos = %+v, %v", repos, err)
	}

	// suspend / unsuspend: a suspended installation grants nothing.
	suspended := strings.Replace(installation, `"suspended_at": null`, `"suspended_at": "2026-10-04T10:00:00Z"`, 1)
	hs.expect(hs.deliver("installation", `{"action": "suspend", `+suspended+`}`), http.StatusOK)
	if _, err := hs.installationFor("acme/api"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("suspended: %v, want ErrNotFound", err)
	}
	hs.expect(hs.deliver("installation", `{"action": "unsuspend", `+installation+`}`), http.StatusOK)
	if id, err := hs.installationFor("acme/api"); err != nil || id != 42 {
		t.Fatalf("unsuspended: %d, %v", id, err)
	}

	// new_permissions_accepted refreshes the account; the claim stays.
	renamed := strings.Replace(installation, `"login": "acme"`, `"login": "acme-inc"`, 1)
	hs.expect(hs.deliver("installation", `{"action": "new_permissions_accepted", `+renamed+`}`), http.StatusOK)
	if in, _ := hs.st.GetInstallation(ctx, 42); in.AccountLogin != "acme-inc" || in.TeamID == nil || *in.TeamID != team.ID {
		t.Fatalf("after new_permissions_accepted: %+v", in)
	}

	// deleted: installation and repositories are gone.
	hs.expect(hs.deliver("installation", `{"action": "deleted", `+installation+`}`), http.StatusOK)
	if _, err := hs.st.GetInstallation(ctx, 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := hs.installationFor("acme/api"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repo of a deleted installation: %v", err)
	}
	if len(hs.passed) != 0 {
		t.Fatalf("installation events reached the API: %v", hs.passed)
	}
}

// Deliveries can be missed or reordered; events that carry the installation
// recreate it.
func TestInstallationEventsRepairMissedCreate(t *testing.T) {
	hs := newHarness(t)
	hs.expect(hs.deliver("installation_repositories", `{"action": "added", `+installation+`,
		"repositories_added": [{"id": 3, "full_name": "acme/docs"}]}`), http.StatusOK)
	if id, err := hs.installationFor("acme/docs"); err != nil || id != 42 {
		t.Fatalf("added before created: %d, %v", id, err)
	}
	other := `"installation": {"id": 7, "account": {"login": "nisa", "type": "User"}, "suspended_at": "2026-10-04T10:00:00Z"}`
	hs.expect(hs.deliver("installation", `{"action": "suspend", `+other+`}`), http.StatusOK)
	if in, err := hs.st.GetInstallation(context.Background(), 7); err != nil || !in.Suspended || in.AccountType != "User" {
		t.Fatalf("suspend of an unknown installation: %+v, %v", in, err)
	}
}

func TestInstallationEventsRejectedOrIgnored(t *testing.T) {
	hs := newHarness(t)
	body := `{"action": "created", ` + installation + `, "repositories": []}`

	hs.expect(hs.deliverSigned("installation", body, webhook.Sign("wrong", []byte(body))), http.StatusUnauthorized)
	hs.expect(hs.deliverSigned("installation", body, ""), http.StatusUnauthorized)
	if _, err := hs.st.GetInstallation(context.Background(), 42); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unsigned delivery applied: %v", err)
	}

	hs.expect(hs.deliver("installation", `{"action": "created"}`), http.StatusBadRequest)
	hs.expect(hs.deliver("installation", `not json`), http.StatusBadRequest)
	hs.expect(hs.deliver("installation", `{"action": "renamed", `+installation+`}`), http.StatusAccepted)
	enterprise := strings.Replace(installation, `"Organization"`, `"Enterprise"`, 1)
	hs.expect(hs.deliver("installation", `{"action": "created", `+enterprise+`}`), http.StatusAccepted)

	// Everything else goes to the API untouched.
	hs.expect(hs.deliver("push", `{}`), http.StatusTeapot)
	hs.expect(hs.deliver("ping", `{}`), http.StatusTeapot)
	hs.expect(hs.deliver("pull_request", `{}`), http.StatusTeapot)
	req := httptest.NewRequest(http.MethodGet, "/webhooks/github", nil)
	req.Header.Set("X-GitHub-Event", "installation")
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	hs.expect(rec, http.StatusTeapot)
	if strings.Join(hs.passed, ",") != "push,ping,pull_request,installation" {
		t.Fatalf("passed through: %v", hs.passed)
	}
}
