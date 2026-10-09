package addons

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/notify"
	"github.com/nisagwn/paas/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recorder) Emit(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

// TestFailureNotifications: a provisioning timeout and a failed restore
// emit addon.failed once each (Faz 26).
func TestFailureNotifications(t *testing.T) {
	f := setup(t, true)
	rec := &recorder{}
	f.c.Notify = rec
	f.create(store.AddonPostgres, "db", store.PreviewCopy)
	f.pass()
	if len(rec.events) != 0 {
		t.Fatalf("provisioning reported: %+v", rec.events)
	}
	f.c.Now = func() time.Time { return time.Now().Add(time.Hour) }
	f.pass()
	f.pass() // still failed: not reported again
	if len(rec.events) != 1 {
		t.Fatalf("events: %+v", rec.events)
	}
	e := rec.events[0]
	if e.Type != notify.AddonFailed || e.AppID != f.app.ID || !strings.Contains(e.Title, `"db" veritabanı hazırlanamadı`) ||
		!strings.Contains(e.Text, "not ready after") || e.Link != "/apps/blog/databases" {
		t.Fatalf("event: %+v", e)
	}

	// A failed restore.
	f.c.Now = time.Now
	f.fc.ready = true
	f.pass()
	a := f.addon("db")
	m, err := f.st.CreateManualBackup(f.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	f.pass()
	f.fc.finish(t, m.Job, true, `{"file":"`+m.Job+`.dump","size":1,"kept":["`+m.Job+`.dump"]}`)
	f.pass()
	if _, err := f.st.RequestRestore(f.ctx, a, m.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	f.pass()
	b, _ := f.st.GetAddonBackup(f.ctx, a.ID, m.ID)
	f.fc.finish(t, deploy.RestoreJobName(a, b), false, "pg_restore: error: could not connect")
	rec.events = nil
	f.pass()
	if len(rec.events) != 1 || !strings.Contains(rec.events[0].Title, "yedekten geri yüklenemedi") ||
		!strings.Contains(rec.events[0].Text, "could not connect") {
		t.Fatalf("restore events: %+v", rec.events)
	}
}
