package addons

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/testdb"
)

// fakeCluster plays Kubernetes for the controller: add-ons become ready
// when ready is set, Jobs run until finish is called.
type fakeCluster struct {
	mu      sync.Mutex
	ready   bool
	applied []deploy.AddonSpec
	jobs    map[string]deploy.JobResult
	pods    map[string]bool // Job pods still terminating
	copies  map[string]deploy.CopyOptions
	deleted int // DeleteAddon calls
	gone    bool
	quota   int
	backups []deploy.JobResult
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{jobs: map[string]deploy.JobResult{}, pods: map[string]bool{}, copies: map[string]deploy.CopyOptions{}}
}

func (f *fakeCluster) ApplyAddon(_ context.Context, spec deploy.AddonSpec) (deploy.AddonState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, spec)
	if !f.ready {
		return deploy.AddonState{Message: "pod addon-db-0: Pending (ContainerCreating)"}, nil
	}
	return deploy.AddonState{Ready: true}, nil
}

func (f *fakeCluster) DeleteAddon(context.Context, store.Addon) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted++
	return f.gone, nil
}

func (f *fakeCluster) ApplyQuota(context.Context, string) error { f.quota++; return nil }

func (f *fakeCluster) start(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[name]; !ok {
		f.jobs[name] = deploy.JobResult{Name: name, Exists: true, Active: true}
	}
	return nil
}

func (f *fakeCluster) StartCopy(_ context.Context, a store.Addon, b store.AddonBranch, o deploy.CopyOptions) error {
	f.mu.Lock()
	f.copies[deploy.CopyJobName(a, b)] = o
	f.mu.Unlock()
	return f.start(deploy.CopyJobName(a, b))
}
func (f *fakeCluster) StartDrop(_ context.Context, a store.Addon, b store.AddonBranch) error {
	return f.start(deploy.DropJobName(a, b))
}
func (f *fakeCluster) StartRotate(_ context.Context, a store.Addon) error {
	return f.start(deploy.RotateJobName(a))
}
func (f *fakeCluster) StartBackup(_ context.Context, a store.Addon, name string) error {
	return f.start(name)
}
func (f *fakeCluster) StartRestore(_ context.Context, a store.Addon, b store.AddonBackup) error {
	return f.start(deploy.RestoreJobName(a, b))
}

func (f *fakeCluster) AddonJob(_ context.Context, _ store.Addon, name string) (deploy.JobResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[name]; ok {
		return j, nil
	}
	return deploy.JobResult{Name: name}, nil
}

func (f *fakeCluster) AddonJobPods(_ context.Context, _ store.Addon, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pods[name], nil
}

func (f *fakeCluster) DeleteAddonJob(_ context.Context, _ store.Addon, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, name)
	return nil
}

func (f *fakeCluster) BackupRuns(context.Context, store.Addon) ([]deploy.JobResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]deploy.JobResult(nil), f.backups...)
	for name, j := range f.jobs {
		if strings.Contains(name, "-backup-") {
			j.Manual = true
			out = append(out, j)
		}
	}
	return out, nil
}

// finish ends a Job.
func (f *fakeCluster) finish(t *testing.T, name string, ok bool, msg string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	j, exists := f.jobs[name]
	if !exists {
		t.Fatalf("job %s was not started; jobs: %v", name, f.jobs)
	}
	now := time.Now()
	j.Active, j.Succeeded, j.Failed, j.Message, j.Finished = false, ok, !ok, msg, &now
	f.jobs[name] = j
}

func (f *fakeCluster) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.jobs[name]
	return ok
}

type fixture struct {
	t   *testing.T
	st  *store.Store
	c   *Controller
	fc  *fakeCluster
	app store.App
	ctx context.Context
}

func setup(t *testing.T, cluster bool) *fixture {
	st := testdb.Open(t)
	ctx := context.Background()
	app, err := st.CreateApp(ctx, "blog", "nisagwn/blog", "main")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, app: app, ctx: ctx, fc: newFakeCluster()}
	var c Cluster
	if cluster {
		c = f.fc
	}
	f.c = New(st, c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.c.WaitPoll = 10 * time.Millisecond
	return f
}

// pass reconciles every add-on of the app once.
func (f *fixture) pass() {
	f.t.Helper()
	list, err := f.st.ListAddons(f.ctx, f.app.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, a := range list {
		if err := f.c.Reconcile(f.ctx, a); err != nil {
			f.t.Fatalf("reconcile %s: %v", a.Name, err)
		}
	}
}

func (f *fixture) addon(name string) store.Addon {
	f.t.Helper()
	a, err := f.st.GetAddon(f.ctx, f.app.ID, name)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fixture) branch(a store.Addon, branch string) store.AddonBranch {
	f.t.Helper()
	b, err := f.st.GetAddonBranch(f.ctx, a.ID, branch)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) create(kind, name, mode string) store.Addon {
	f.t.Helper()
	a, err := f.st.CreateAddon(f.ctx, f.app.ID, store.NewAddon{Kind: kind, Name: name, Plan: "hobby", PreviewMode: mode})
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestProvisioning(t *testing.T) {
	f := setup(t, true)
	f.create(store.AddonPostgres, "db", store.PreviewCopy)
	f.pass()
	a := f.addon("db")
	if a.Status != store.AddonProvisioning || !strings.Contains(a.Message, "ContainerCreating") {
		t.Fatalf("provisioning: %+v", a)
	}
	spec := f.fc.applied[0]
	if spec.Secrets.Password == "" || spec.Secrets.Admin == "" || spec.Next != nil {
		t.Fatalf("spec: %+v", spec)
	}

	// Not ready within the timeout: failed; ready later: recovers.
	f.c.Now = func() time.Time { return time.Now().Add(time.Hour) }
	f.pass()
	if a = f.addon("db"); a.Status != store.AddonFailed || !strings.HasPrefix(a.Message, "not ready after 10m0s") {
		t.Fatalf("timeout: %+v", a)
	}
	f.fc.ready = true
	f.pass()
	if a = f.addon("db"); a.Status != store.AddonReady || a.Message != "" {
		t.Fatalf("ready: %+v", a)
	}
	// A crash later keeps it ready (deployments keep their variables).
	f.fc.ready = false
	f.pass()
	if a = f.addon("db"); a.Status != store.AddonReady || a.Message == "" {
		t.Fatalf("crash: %+v", a)
	}

	// Deletion: objects first, then the row and the quota.
	f.fc.ready = true
	if _, err := f.st.DeleteAddon(f.ctx, a); err != nil {
		t.Fatal(err)
	}
	f.pass()
	if f.fc.deleted != 1 || f.fc.quota != 0 {
		t.Fatal("deleted before the objects were gone")
	}
	f.fc.gone = true
	f.pass()
	if _, err := f.st.GetAddon(f.ctx, f.app.ID, "db"); err == nil || f.fc.quota != 1 {
		t.Fatalf("row left after deletion: %v, quota %d", err, f.fc.quota)
	}
}

func TestBranchCopyFlow(t *testing.T) {
	f := setup(t, true)
	f.fc.ready = true
	a := f.create(store.AddonPostgres, "db", store.PreviewCopy)
	rules := []string{"users.email: email", "*.phone: null"}
	sql := "UPDATE orders SET note = NULL;"
	a, _ = f.st.UpdateAddon(f.ctx, a, store.AddonChange{Anonymize: &rules, AnonymizeSQL: &sql})
	f.pass()

	// A preview deployment asks for its database before the build.
	d, _, _ := f.st.EnqueueDeployment(f.ctx, f.app.ID, fmt.Sprintf("%040x", 1), "feature/x", "")
	var log logLines
	if err := f.c.Request(f.ctx, d, log.add); err != nil {
		t.Fatal(err)
	}
	b := f.branch(a, "feature/x")
	if b.Status != store.BranchPending || !strings.Contains(log.String(), "==> veritabanı kopyası: db → "+b.Database+" (production'dan kopyalanıyor, anonimleştirilecek)") {
		t.Fatalf("request: %+v\n%s", b, log.String())
	}

	// The next pass applies the branch role's password, then starts the copy.
	f.pass()
	last := f.fc.applied[len(f.fc.applied)-1]
	if last.BranchPasswords[b.Database] == "" {
		t.Fatalf("branch password not applied: %+v", last.BranchPasswords)
	}
	job := deploy.CopyJobName(a, b)
	if b = f.branch(a, "feature/x"); b.Status != store.BranchCopying || !f.fc.has(job) {
		t.Fatalf("copy not started: %+v", b)
	}
	o := f.fc.copies[job]
	if o.Mode != "copy" || o.UserSQL != sql || !strings.Contains(o.AnonymizeSQL, `UPDATE "public"."users" SET "email"`) ||
		!strings.Contains(o.AnonymizeSQL, "column_name = 'phone'") || o.MaxBytes != DefaultCopyMaxBytes {
		t.Fatalf("copy options: %+v", o)
	}

	// The deployment waits; the Job reports a fallback to an empty database.
	done := make(chan error, 1)
	var wlog logLines
	go func() { done <- f.c.Wait(f.ctx, d, wlog.add) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Wait returned before the copy: %v", err)
	default:
	}
	f.fc.finish(t, job, true, `{"mode":"empty","warning":"size","snapshot":"2026-10-07T05:27:04Z","size":7765015,"source_size":6442450944}`)
	f.pass()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b = f.branch(a, "feature/x")
	if b.Status != store.BranchReady || b.Mode != store.PreviewEmpty || *b.SizeBytes != 7765015 || f.fc.has(job) ||
		!strings.Contains(b.Warning, "above the copy limit of 5.0 GiB") || b.SnapshotAt == nil {
		t.Fatalf("finished: %+v", b)
	}
	for _, want := range []string{"kopyalanıyor…", "==> veritabanı kopyası: db hazır (7.4 MiB, boş veritabanı)",
		"UYARI: production 6.0 GiB, kopya sınırı 5.0 GiB"} {
		if !strings.Contains(wlog.String(), want) {
			t.Errorf("wait log lacks %q:\n%s", want, wlog.String())
		}
	}

	// "Kopyayı yenile": a new generation, a new Job.
	if _, err := f.st.ResetAddonBranch(f.ctx, a, "feature/x"); err != nil {
		t.Fatal(err)
	}
	f.pass()
	b = f.branch(a, "feature/x")
	job2 := deploy.CopyJobName(a, b)
	if b.Generation != 1 || job2 == job || !f.fc.has(job2) {
		t.Fatalf("reset: %+v", b)
	}
	// A failed copy fails the waiting deployment with the Job's error.
	f.fc.finish(t, job2, false, "==> anonymization rules\nERROR:  relation \"public.users\" does not exist")
	f.pass()
	err := f.c.Wait(f.ctx, d, (&logLines{}).add)
	if err == nil || !strings.Contains(err.Error(), `veritabanı kopyası başarısız (db → `+b.Database+`): ERROR:  relation "public.users" does not exist`) {
		t.Fatalf("failed copy: %v", err)
	}
	// The next deployment retries it.
	if err := f.c.Request(f.ctx, d, (&logLines{}).add); err != nil {
		t.Fatal(err)
	}
	if b = f.branch(a, "feature/x"); b.Status != store.BranchPending || b.Generation != 2 {
		t.Fatalf("retry: %+v", b)
	}

	// Branch deleted mid-copy: the copy Job is stopped, its pod must be
	// gone, then a drop Job runs, then the row goes.
	f.pass()
	b = f.branch(a, "feature/x")
	copyJob := deploy.CopyJobName(a, b)
	if b.Status != store.BranchCopying || !f.fc.has(copyJob) {
		t.Fatalf("copying: %+v", b)
	}
	if _, err := f.st.DeleteBranch(f.ctx, f.app.ID, "feature/x", "branch deleted"); err != nil {
		t.Fatal(err)
	}
	f.fc.pods[copyJob] = true
	f.pass()
	if f.fc.has(copyJob) || f.fc.has(deploy.DropJobName(a, b)) {
		t.Fatal("copy not stopped first")
	}
	f.pass() // the copy's pod is still terminating
	if f.fc.has(deploy.DropJobName(a, b)) {
		t.Fatal("drop started while the copy pod lived")
	}
	f.fc.pods[copyJob] = false
	f.pass()
	drop := deploy.DropJobName(a, b)
	if !f.fc.has(drop) {
		t.Fatal("drop not started")
	}
	f.fc.finish(t, drop, true, "")
	f.pass()
	if _, err := f.st.GetAddonBranch(f.ctx, a.ID, "feature/x"); err == nil || f.fc.has(drop) {
		t.Fatal("branch row left after the drop")
	}
}

func TestSharedAndProduction(t *testing.T) {
	f := setup(t, true)
	f.fc.ready = true
	f.create(store.AddonPostgres, "db", store.PreviewShared)
	f.create(store.AddonRedis, "cache", store.PreviewShared)
	f.pass()
	prev, _, _ := f.st.EnqueueDeployment(f.ctx, f.app.ID, fmt.Sprintf("%040x", 1), "dev", "")
	var log logLines
	if err := f.c.Request(f.ctx, prev, log.add); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Wait(f.ctx, prev, log.add); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "bu önizleme production veritabanını kullanıyor") {
		t.Errorf("log: %s", log.String())
	}
	if rows, _ := f.st.ListAddonBranches(f.ctx, f.addon("db").ID); len(rows) != 0 {
		t.Fatalf("shared mode created branches: %+v", rows)
	}

	// A provisioning add-on is waited for, a failed one fails the deployment.
	f.create(store.AddonPostgres, "analytics", store.PreviewShared)
	prod, _, _ := f.st.EnqueueDeployment(f.ctx, f.app.ID, fmt.Sprintf("%040x", 2), "main", "")
	ctx, cancel := context.WithTimeout(f.ctx, 50*time.Millisecond)
	defer cancel()
	log = logLines{}
	if err := f.c.Wait(ctx, prod, log.add); err == nil || !strings.Contains(log.String(), "veritabanı hazırlanıyor: analytics") {
		t.Fatalf("provisioning wait: %v\n%s", err, log.String())
	}
	f.st.SetAddonState(f.ctx, f.addon("analytics").ID, store.AddonFailed, "not ready after 10m0s: ImagePullBackOff")
	if err := f.c.Wait(f.ctx, prod, log.add); err == nil || !strings.Contains(err.Error(), "add-on analytics is not working: not ready after 10m0s: ImagePullBackOff") {
		t.Fatalf("failed add-on: %v", err)
	}
}

func TestRotation(t *testing.T) {
	f := setup(t, true)
	f.fc.ready = true
	a := f.create(store.AddonPostgres, "db", store.PreviewCopy)
	f.pass()
	d, _, _ := f.st.EnqueueDeployment(f.ctx, f.app.ID, fmt.Sprintf("%040x", 1), "main", "")
	f.st.MarkReady(f.ctx, d, []store.AliasSpec{{Hostname: "blog.x", Kind: store.AliasProduction, Branch: "main"}})

	a, _ = f.st.GetAddonByID(f.ctx, a.ID)
	if _, err := f.st.RequestAddonRotation(f.ctx, a, true); err != nil {
		t.Fatal(err)
	}
	f.pass()
	spec := f.fc.applied[len(f.fc.applied)-1]
	job := deploy.RotateJobName(a)
	if spec.Next == nil || spec.Next.Password == spec.Secrets.Password || !f.fc.has(job) {
		t.Fatalf("rotation not started: %+v", spec)
	}
	f.pass() // still running
	if a = f.addon("db"); !a.Rotating {
		t.Fatal("completed before the job")
	}
	f.fc.finish(t, job, true, "")
	f.pass()
	a = f.addon("db")
	cur, next, _ := f.st.AddonSecretsOf(f.ctx, a)
	if a.Rotating || next != nil || cur.Password != spec.Next.Password || a.SecretsVersion != 2 || f.fc.has(job) {
		t.Fatalf("after rotation: %+v", a)
	}
	// What the aliases pointed at is redeployed with the new password.
	deps, _ := f.st.ListDeployments(f.ctx, f.app.ID, 10)
	if len(deps) != 2 || deps[0].Origin != store.OriginRedeploy || deps[0].SourceDeploymentID == nil || *deps[0].SourceDeploymentID != d.ID {
		t.Fatalf("redeploy: %+v", deps)
	}

	// A failed rotation keeps the old password and says why.
	f.st.RequestAddonRotation(f.ctx, a, false)
	f.pass()
	f.fc.finish(t, deploy.RotateJobName(a), false, "FATAL: password authentication failed")
	f.pass()
	a = f.addon("db")
	if c2, n, _ := f.st.AddonSecretsOf(f.ctx, a); a.Rotating || n != nil || c2 != cur || !strings.Contains(a.Message, "password rotation failed") {
		t.Fatalf("failed rotation: %+v", a)
	}
}

func TestBackupsAndRestore(t *testing.T) {
	f := setup(t, true)
	f.fc.ready = true
	a := f.create(store.AddonPostgres, "db", store.PreviewCopy)
	f.pass()
	a = f.addon("db")

	// A scheduled run of the CronJob is imported; pruned files expire.
	t1 := time.Now().Add(-2 * time.Hour)
	f.st.RecordBackupRun(f.ctx, a.ID, store.BackupRun{Job: "addon-db-backup-old", Trigger: store.BackupScheduled,
		Status: store.BackupSucceeded, FinishedAt: &t1})
	t2 := time.Now().Add(-time.Hour)
	f.fc.backups = []deploy.JobResult{{Name: "addon-db-backup-29000000", Exists: true, Succeeded: true, Finished: &t2,
		Message: `{"file":"addon-db-backup-29000000.dump","size":35034,"kept":["addon-db-backup-29000000.dump"]}`}}
	m, err := f.st.CreateManualBackup(f.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	f.pass()
	list, _ := f.st.ListAddonBackups(f.ctx, a.ID, 10)
	byJob := map[string]store.AddonBackup{}
	for _, b := range list {
		byJob[b.Job] = b
	}
	if s := byJob["addon-db-backup-29000000"]; s.Status != store.BackupSucceeded || *s.SizeBytes != 35034 || s.Trigger != store.BackupScheduled {
		t.Fatalf("scheduled: %+v", s)
	}
	if byJob["addon-db-backup-old"].Status != store.BackupExpired {
		t.Fatalf("pruned: %+v", byJob["addon-db-backup-old"])
	}
	if byJob[m.Job].Status != store.BackupRunning || !f.fc.has(m.Job) {
		t.Fatalf("manual: %+v", byJob[m.Job])
	}
	f.fc.finish(t, m.Job, true, `{"file":"`+m.Job+`.dump","size":1,"kept":["`+m.Job+`.dump","addon-db-backup-29000000.dump"]}`)
	f.pass()
	if b, _ := f.st.GetAddonBackup(f.ctx, a.ID, m.ID); b.Status != store.BackupSucceeded || b.Trigger != store.BackupManual {
		t.Fatalf("manual finished: %+v", b)
	}

	// Restore.
	if _, err := f.st.RequestRestore(f.ctx, a, m.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	f.pass()
	b, _ := f.st.GetAddonBackup(f.ctx, a.ID, m.ID)
	job := deploy.RestoreJobName(a, b)
	if b.RestoreStatus != store.BackupRunning || !f.fc.has(job) {
		t.Fatalf("restore not started: %+v", b)
	}
	f.fc.finish(t, job, false, "backup file x.dump is no longer on the backup volume")
	f.pass()
	if b, _ = f.st.GetAddonBackup(f.ctx, a.ID, m.ID); b.RestoreStatus != store.BackupFailed || !strings.Contains(b.RestoreError, "no longer on the backup volume") {
		t.Fatalf("restore failed: %+v", b)
	}
}

func TestDryRun(t *testing.T) {
	f := setup(t, false)
	a := f.create(store.AddonPostgres, "db", store.PreviewCopy)
	f.create(store.AddonRedis, "cache", store.PreviewShared)
	d, _, _ := f.st.EnqueueDeployment(f.ctx, f.app.ID, fmt.Sprintf("%040x", 1), "dev", "")
	var log logLines
	if err := f.c.Request(f.ctx, d, log.add); err != nil {
		t.Fatal(err)
	}
	// Run passes the add-ons through on kicks while Wait polls.
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	go f.c.Run(ctx)
	wctx, wcancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer wcancel()
	if err := f.c.Wait(wctx, d, log.add); err != nil {
		t.Fatalf("dry-run wait: %v\n%s", err, log.String())
	}
	if f.addon("db").Status != store.AddonReady || f.branch(a, "dev").Status != store.BranchReady {
		t.Fatal("dry run did not pass the states through")
	}
	env, err := f.st.DeploymentEnv(f.ctx, d)
	if err != nil || env["PGDATABASE"] != "preview_dev" || env["REDIS_URL"] == "" {
		t.Fatalf("env: %v %v", env, err)
	}
	for _, want := range []string{"veritabanı hazırlanıyor: db", "veritabanı kopyası: db hazır"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
