package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

func TestAuditLog(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateApp(ctx, "shop", "nisagwn/shop", "main")
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := st.EnqueueDeployment(ctx, app.ID, sha(1), "main", "first")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.UpsertUser(ctx, 42, "ayse", "Ayşe", "")
	if err != nil {
		t.Fatal(err)
	}

	id1, err := st.StartAudit(ctx, store.AuditEntry{Action: store.AuditTerminal, AppID: app.ID, DeploymentID: &d.ID,
		UserID: &u.ID, Actor: "ayse", Process: "web", Pod: "d-0000001-abc"})
	if err != nil {
		t.Fatal(err)
	}
	// The admin principal has no user row.
	zero := int64(0)
	id2, err := st.StartAudit(ctx, store.AuditEntry{Action: store.AuditTerminal, AppID: app.ID, DeploymentID: &d.ID,
		UserID: &zero, Actor: "admin", Process: "worker", Pod: "d-0000001-w-worker-xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartAudit(ctx, store.AuditEntry{Action: store.AuditTerminal, AppID: other.ID, Actor: "x"}); err != nil {
		t.Fatal(err)
	}

	code := 3
	if err := st.EndAudit(ctx, id1, &code, "exit"); err != nil {
		t.Fatal(err)
	}
	// A second end (e.g. shutdown after a normal exit) keeps the first.
	if err := st.EndAudit(ctx, id1, nil, "shutdown"); err != nil {
		t.Fatal(err)
	}
	if err := st.EndAudit(ctx, 99999, nil, "exit"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("end of a missing entry: %v", err)
	}

	list, err := st.ListAudit(ctx, app.ID, store.AuditTerminal, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d entries, want 2 (other apps excluded)", len(list))
	}
	byID := map[int64]store.AuditEntry{}
	for _, e := range list {
		byID[e.ID] = e
	}
	e1, e2 := byID[id1], byID[id2]
	if e1.Actor != "ayse" || e1.UserID == nil || *e1.UserID != u.ID || e1.Pod != "d-0000001-abc" || e1.Process != "web" ||
		e1.EndedAt == nil || e1.ExitCode == nil || *e1.ExitCode != 3 || e1.EndReason != "exit" || e1.CommitSHA != sha(1) ||
		e1.DeploymentID == nil || *e1.DeploymentID != d.ID {
		t.Fatalf("entry 1: %+v", e1)
	}
	if e2.UserID != nil || e2.Actor != "admin" || e2.EndedAt != nil || e2.ExitCode != nil {
		t.Fatalf("entry 2 (open, admin): %+v", e2)
	}
	if list[0].ID != id2 && !list[0].StartedAt.After(list[1].StartedAt) {
		t.Fatalf("not newest first: %+v", list)
	}

	// Deleting the deployment keeps the record.
	if err := st.Exec(ctx, `DELETE FROM deployments WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	list, err = st.ListAudit(ctx, app.ID, "", 10)
	if err != nil || len(list) != 2 || list[0].DeploymentID != nil {
		t.Fatalf("after deleting the deployment: %v %+v", err, list)
	}
}
