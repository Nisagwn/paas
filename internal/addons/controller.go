// Package addons runs managed databases (Faz 22): Postgres and Redis
// add-ons in an app's namespace, a copy of the production database for
// every preview branch, daily backups and restores.
//
// The Controller is a reconcile loop like the rollout and process loops:
// every Interval it claims the add-ons that are due (store.ClaimDueAddons,
// FOR UPDATE SKIP LOCKED, so several control plane replicas share them)
// and, for each one,
//
//	deleting      → delete its objects and volumes, then the row
//	otherwise     → apply its objects (deploy.ApplyAddon repairs drift),
//	                provisioning → ready once the server pod is ready
//	                (failed when it is not within ProvisionTimeout)
//	ready         → a pending password rotation; branch databases
//	                (pending → copy Job, copying → result, deleting →
//	                drop Job, no live deployment left → deleting);
//	                backups (manual requests, CronJob runs, restores)
//
// Every Job has a deterministic name, so a step repeated after a crash or
// by another replica finds the Job of the first attempt instead of
// starting a second one; every state change is a compare-and-set on the
// row (status, generation). Without a cluster (dry run) the same states
// are passed through at once.
package addons

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nisagwn/paas/internal/deploy"
	"github.com/nisagwn/paas/internal/store"
)

// Cluster runs add-ons (deploy.Kubernetes).
type Cluster interface {
	ApplyAddon(ctx context.Context, spec deploy.AddonSpec) (deploy.AddonState, error)
	DeleteAddon(ctx context.Context, a store.Addon) (bool, error)
	ApplyQuota(ctx context.Context, app string) error
	StartCopy(ctx context.Context, a store.Addon, b store.AddonBranch, o deploy.CopyOptions) error
	StartDrop(ctx context.Context, a store.Addon, b store.AddonBranch) error
	StartRotate(ctx context.Context, a store.Addon) error
	StartBackup(ctx context.Context, a store.Addon, name string) error
	StartRestore(ctx context.Context, a store.Addon, b store.AddonBackup) error
	AddonJob(ctx context.Context, a store.Addon, name string) (deploy.JobResult, error)
	AddonJobPods(ctx context.Context, a store.Addon, name string) (bool, error)
	DeleteAddonJob(ctx context.Context, a store.Addon, name string) error
	BackupRuns(ctx context.Context, a store.Addon) ([]deploy.JobResult, error)
}

var _ Cluster = (*deploy.Kubernetes)(nil)

// Defaults.
const (
	DefaultInterval         = 10 * time.Second
	DefaultCopyTimeout      = 10 * time.Minute
	DefaultProvisionTimeout = 10 * time.Minute
	DefaultCopyMaxBytes     = 5 << 30
	// maxParallelCopies per add-on: copies share the server's disk and CPU
	// with production.
	maxParallelCopies = 2
	// maxCopyAttempts: a copy whose Job keeps disappearing fails.
	maxCopyAttempts = 3
	// branchGrace keeps a branch database that changed recently from
	// being dropped before its deployment is queued.
	branchGrace = 2 * time.Minute
	// kickGap: a kicked app's add-ons are reconciled at most this often.
	kickGap = 2 * time.Second
)

// Controller reconciles add-ons; it also implements worker.Databases
// (prepare.go).
type Controller struct {
	Store *store.Store
	// Cluster is nil without Kubernetes (dry run): add-ons become ready and
	// copies, backups and restores succeed at once, without data.
	Cluster Cluster
	Log     *slog.Logger

	Interval         time.Duration
	ProvisionTimeout time.Duration
	CopyTimeout      time.Duration
	CopyMaxBytes     int64
	// WaitPoll is how often a waiting deployment looks at its databases.
	WaitPoll time.Duration

	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time

	kicks chan int64
}

func (c *Controller) defaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.ProvisionTimeout <= 0 {
		c.ProvisionTimeout = DefaultProvisionTimeout
	}
	if c.CopyTimeout <= 0 {
		c.CopyTimeout = DefaultCopyTimeout
	}
	if c.CopyMaxBytes <= 0 {
		c.CopyMaxBytes = DefaultCopyMaxBytes
	}
	if c.WaitPoll <= 0 {
		c.WaitPoll = time.Second
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// New returns a controller ready to receive kicks before Run starts.
// cluster nil runs add-ons in dry-run mode.
func New(st *store.Store, cluster Cluster, log *slog.Logger) *Controller {
	return &Controller{Store: st, Cluster: cluster, Log: log, kicks: make(chan int64, 64)}
}

// Kick asks Run to reconcile the app's add-ons soon (a deployment waits
// for them). It never blocks.
func (c *Controller) Kick(appID int64) {
	if c.kicks == nil {
		return
	}
	select {
	case c.kicks <- appID:
	default:
	}
}

// Run reconciles until ctx ends: due add-ons every Interval, a kicked
// app's add-ons at once.
func (c *Controller) Run(ctx context.Context) {
	c.defaults()
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case app := <-c.kicks:
			c.reconcileAll(ctx, func() ([]store.Addon, error) { return c.Store.ClaimAppAddons(ctx, app, kickGap) })
		}
	}
}

// Tick reconciles the add-ons that are due and returns how many.
func (c *Controller) Tick(ctx context.Context) int {
	c.defaults()
	// A little under the interval, so a replica's own next tick finds the
	// add-on due again while another replica's tick in between does not.
	return c.reconcileAll(ctx, func() ([]store.Addon, error) { return c.Store.ClaimDueAddons(ctx, c.Interval*4/5) })
}

func (c *Controller) reconcileAll(ctx context.Context, claim func() ([]store.Addon, error)) int {
	due, err := claim()
	if err != nil {
		c.Log.Error("addons: claim", "err", err)
		return 0
	}
	for _, a := range due {
		if err := c.Reconcile(ctx, a); err != nil && ctx.Err() == nil {
			c.Log.Error("addons: reconcile", "app", a.AppName, "addon", a.Name, "err", err)
		}
	}
	return len(due)
}

// Reconcile brings one add-on a step closer to its desired state.
func (c *Controller) Reconcile(ctx context.Context, a store.Addon) error {
	c.defaults()
	if a.Status == store.AddonDeleting {
		return c.remove(ctx, a)
	}
	cur, next, err := c.Store.AddonSecretsOf(ctx, a)
	if err != nil {
		return err
	}
	if c.Cluster == nil {
		return c.dryRun(ctx, a, next)
	}
	var branchPWs map[string]string
	if a.Kind == store.AddonPostgres {
		if branchPWs, err = c.Store.AddonBranchPasswords(ctx, a); err != nil {
			return err
		}
	}
	state, err := c.Cluster.ApplyAddon(ctx, deploy.AddonSpec{Addon: a, Secrets: cur, Next: next, BranchPasswords: branchPWs})
	if err != nil {
		c.setState(ctx, a, c.statusFor(a, false), err.Error())
		return err
	}
	status, msg := c.statusFor(a, state.Ready), state.Message
	if status == store.AddonFailed {
		msg = fmt.Sprintf("not ready after %s: %s", c.ProvisionTimeout, msg)
	}
	c.setState(ctx, a, status, msg)
	if status != store.AddonReady {
		return nil
	}
	if a.Status != store.AddonReady {
		c.Log.Info("addon ready", "app", a.AppName, "addon", a.Name, "kind", a.Kind)
	}
	a.Status = store.AddonReady

	if next != nil {
		done, err := c.rotate(ctx, a)
		if err != nil || done {
			return err // a completed rotation is applied on the next pass
		}
	}
	if a.Kind != store.AddonPostgres {
		return nil
	}
	return errors.Join(c.branches(ctx, a, branchPWs), c.backups(ctx, a))
}

// statusFor is the status after a pass: ready once the server was ready
// (a later crash keeps it ready, so deployments keep their variables; the
// message says what is wrong); provisioning until ProvisionTimeout, then
// failed; a failed add-on that becomes ready recovers.
func (c *Controller) statusFor(a store.Addon, ready bool) string {
	switch {
	case ready || a.Status == store.AddonReady:
		return store.AddonReady
	case a.Status == store.AddonFailed || c.Now().Sub(a.CreatedAt) > c.ProvisionTimeout:
		return store.AddonFailed
	}
	return store.AddonProvisioning
}

func (c *Controller) setState(ctx context.Context, a store.Addon, status, msg string) {
	if err := c.Store.SetAddonState(ctx, a.ID, status, msg); err != nil {
		c.Log.Error("addons: state", "addon", a.Name, "err", err)
	}
}

// remove deletes a deleting add-on's objects, then its row.
func (c *Controller) remove(ctx context.Context, a store.Addon) error {
	if c.Cluster != nil {
		gone, err := c.Cluster.DeleteAddon(ctx, a)
		if err != nil || !gone {
			return err // the next pass checks again
		}
	}
	if err := c.Store.RemoveAddon(ctx, a.ID); err != nil {
		return err
	}
	c.Log.Info("addon deleted", "app", a.AppName, "addon", a.Name)
	if c.Cluster != nil {
		return c.Cluster.ApplyQuota(ctx, a.AppName)
	}
	return nil
}

// ---- rotation ----

// rotate applies a pending password. Postgres: a Job runs ALTER ROLE with
// the next password (the add-on Secret's next-password); Redis: nothing to
// run, the pod restarts with the new Secret on the next pass. It reports
// whether the rotation completed.
func (c *Controller) rotate(ctx context.Context, a store.Addon) (bool, error) {
	if a.Kind == store.AddonPostgres {
		name := deploy.RotateJobName(a)
		job, err := c.Cluster.AddonJob(ctx, a, name)
		if err != nil {
			return false, err
		}
		switch {
		case !job.Exists:
			return false, c.Cluster.StartRotate(ctx, a)
		case job.Active:
			return false, nil
		case job.Failed:
			msg := "password rotation failed: " + lastLines(job.Message)
			c.Log.Warn("addon rotation failed", "app", a.AppName, "addon", a.Name, "err", job.Message)
			if err := c.Store.CancelAddonRotation(ctx, a, msg); err != nil {
				return false, err
			}
			return true, c.Cluster.DeleteAddonJob(ctx, a, name)
		}
		if err := c.Cluster.DeleteAddonJob(ctx, a, name); err != nil {
			return false, err
		}
	}
	return true, c.completeRotation(ctx, a)
}

func (c *Controller) completeRotation(ctx context.Context, a store.Addon) error {
	redeploy, err := c.Store.CompleteAddonRotation(ctx, a)
	if err != nil {
		return err
	}
	c.Log.Info("addon password rotated", "app", a.AppName, "addon", a.Name, "redeploy", redeploy)
	if !redeploy {
		return nil
	}
	targets, err := c.Store.AliasTargets(ctx, a.AppID)
	if err != nil {
		return err
	}
	for _, d := range targets {
		nd, err := c.Store.CopyDeployment(ctx, d, store.CopyOptions{
			Origin: store.OriginRedeploy, Target: d.Target, ReuseImage: true,
		})
		if err != nil {
			c.Log.Error("addons: redeploy after rotation", "deployment", d.ID, "err", err)
			continue
		}
		c.Store.AppendLog(ctx, nd.ID, fmt.Sprintf("==> redeploy of deployment #%d: the password of add-on %s was rotated", d.ID, a.Name))
	}
	return nil
}

// ---- branch databases ----

func (c *Controller) branches(ctx context.Context, a store.Addon, passwords map[string]string) error {
	var err error
	if a.HasBranches() {
		_, err = c.Store.ExpireAddonBranches(ctx, a, branchGrace)
	} else {
		err = c.Store.DeleteAddonBranches(ctx, a) // previews share production now
	}
	if err != nil {
		return err
	}
	rows, err := c.Store.ListAddonBranches(ctx, a.ID)
	if err != nil {
		return err
	}
	copying := 0
	for _, b := range rows {
		if b.Status == store.BranchCopying {
			copying++
		}
	}
	var errs []error
	for _, b := range rows {
		switch b.Status {
		case store.BranchDeleting:
			errs = append(errs, c.drop(ctx, a, b))
		case store.BranchCopying:
			errs = append(errs, c.checkCopy(ctx, a, b))
		case store.BranchPending:
			// The branch role's password must be in the Secret the Job reads,
			// i.e. in what this pass applied.
			if _, ok := passwords[b.Database]; !ok || copying >= maxParallelCopies {
				continue
			}
			started, err := c.startCopy(ctx, a, b)
			if started {
				copying++
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) startCopy(ctx context.Context, a store.Addon, b store.AddonBranch) (bool, error) {
	rules, err := ParseRules(a.Anonymize)
	if err != nil {
		// Rules are validated when saved; this only happens after a change
		// of the rule syntax.
		_, ferr := c.Store.FailAddonBranch(ctx, b.ID, b.Generation, "anonymization rules: "+err.Error())
		return false, ferr
	}
	ok, err := c.Store.StartAddonBranch(ctx, b.ID, b.Generation)
	if err != nil || !ok {
		return false, err
	}
	err = c.Cluster.StartCopy(ctx, a, b, deploy.CopyOptions{
		Mode: b.Mode, AnonymizeSQL: CompileRules(rules), UserSQL: a.AnonymizeSQL,
		MaxBytes: c.CopyMaxBytes, Deadline: c.CopyTimeout,
	})
	if err == nil {
		c.Log.Info("addon branch copy started", "app", a.AppName, "addon", a.Name, "branch", b.Branch, "mode", b.Mode)
	}
	return true, err // a Job that was not created is started again (checkCopy)
}

func (c *Controller) checkCopy(ctx context.Context, a store.Addon, b store.AddonBranch) error {
	name := deploy.CopyJobName(a, b)
	job, err := c.Cluster.AddonJob(ctx, a, name)
	if err != nil {
		return err
	}
	switch {
	case !job.Exists:
		if b.Attempts >= maxCopyAttempts {
			_, err := c.Store.FailAddonBranch(ctx, b.ID, b.Generation, "the copy job disappeared repeatedly")
			return err
		}
		return c.Store.RequeueAddonBranch(ctx, b.ID, b.Generation)
	case job.Active:
		return nil
	case job.Failed:
		reason := lastLines(job.Message)
		if reason == "" {
			reason = "the copy job failed"
		}
		c.Log.Warn("addon branch copy failed", "app", a.AppName, "addon", a.Name, "branch", b.Branch, "err", reason)
		if _, err := c.Store.FailAddonBranch(ctx, b.ID, b.Generation, reason); err != nil {
			return err
		}
		return c.Cluster.DeleteAddonJob(ctx, a, name)
	}
	r := store.BranchResult{Mode: b.Mode}
	if rep, ok := deploy.ParseCopyReport(job.Message); ok {
		r.Mode = rep.Mode
		r.SizeBytes = &rep.Size
		if t, err := time.Parse(time.RFC3339, rep.Snapshot); err == nil {
			r.SnapshotAt = &t
		}
		r.Warning = FallbackWarning(rep.Warning, rep.SourceSize, c.CopyMaxBytes)
	}
	if _, err := c.Store.FinishAddonBranch(ctx, b.ID, b.Generation, r); err != nil {
		return err
	}
	c.Log.Info("addon branch ready", "app", a.AppName, "addon", a.Name, "branch", b.Branch, "mode", r.Mode)
	return c.Cluster.DeleteAddonJob(ctx, a, name)
}

// drop removes a branch database: a copy still running is stopped first
// (its pod must be gone, or it could create the database again after the
// drop), then a Job drops the database and its role, then the row goes.
func (c *Controller) drop(ctx context.Context, a store.Addon, b store.AddonBranch) error {
	copyJob := deploy.CopyJobName(a, b)
	cj, err := c.Cluster.AddonJob(ctx, a, copyJob)
	if err != nil {
		return err
	}
	if cj.Exists {
		return c.Cluster.DeleteAddonJob(ctx, a, copyJob)
	}
	if busy, err := c.Cluster.AddonJobPods(ctx, a, copyJob); err != nil || busy {
		return err
	}
	name := deploy.DropJobName(a, b)
	job, err := c.Cluster.AddonJob(ctx, a, name)
	if err != nil {
		return err
	}
	switch {
	case !job.Exists:
		return c.Cluster.StartDrop(ctx, a, b)
	case job.Active:
		return nil
	case job.Failed:
		reason := "dropping the database failed (retried): " + lastLines(job.Message)
		if err := c.Store.NoteAddonBranchError(ctx, b.ID, reason); err != nil {
			return err
		}
		return c.Cluster.DeleteAddonJob(ctx, a, name)
	}
	if err := c.Store.RemoveAddonBranch(ctx, b.ID); err != nil {
		return err
	}
	c.Log.Info("addon branch dropped", "app", a.AppName, "addon", a.Name, "branch", b.Branch)
	return c.Cluster.DeleteAddonJob(ctx, a, name)
}

// ---- backups ----

func (c *Controller) backups(ctx context.Context, a store.Addon) error {
	runs, err := c.Cluster.BackupRuns(ctx, a)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var latest *deploy.BackupReport
	var latestAt time.Time
	for _, r := range runs {
		seen[r.Name] = true
		run := store.BackupRun{Job: r.Name, Trigger: store.BackupScheduled, Status: store.BackupRunning,
			StartedAt: r.Started, FinishedAt: r.Finished}
		if r.Manual {
			run.Trigger = store.BackupManual
		}
		switch {
		case r.Succeeded:
			run.Status = store.BackupSucceeded
			if rep, ok := deploy.ParseBackupReport(r.Message); ok {
				run.SizeBytes = &rep.Size
				if r.Finished != nil && r.Finished.After(latestAt) {
					rep := rep
					latest, latestAt = &rep, *r.Finished
				}
			}
		case r.Failed:
			run.Status, run.Error = store.BackupFailed, lastLines(r.Message)
		}
		if err := c.Store.RecordBackupRun(ctx, a.ID, run); err != nil {
			return err
		}
	}
	if latest != nil {
		if err := c.Store.ExpireAddonBackups(ctx, a.ID, latest.Kept, latestAt); err != nil {
			return err
		}
	}

	rows, err := c.Store.ListAddonBackups(ctx, a.ID, 100)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range rows {
		switch {
		case b.Status == store.BackupPending:
			if err := c.Cluster.StartBackup(ctx, a, b.Job); err != nil {
				errs = append(errs, err)
				continue
			}
			errs = append(errs, c.Store.SetBackupStatus(ctx, b.ID, store.BackupRunning, ""))
		case b.Status == store.BackupRunning && !seen[b.Job] && b.StartedAt != nil &&
			c.Now().Sub(*b.StartedAt) > 2*time.Hour:
			errs = append(errs, c.Store.SetBackupStatus(ctx, b.ID, store.BackupFailed, "the backup job disappeared"))
		}
		if b.RestoreStatus == store.BackupPending || b.RestoreStatus == store.BackupRunning {
			errs = append(errs, c.restore(ctx, a, b))
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) restore(ctx context.Context, a store.Addon, b store.AddonBackup) error {
	name := deploy.RestoreJobName(a, b)
	job, err := c.Cluster.AddonJob(ctx, a, name)
	if err != nil {
		return err
	}
	switch {
	case !job.Exists && b.RestoreStatus == store.BackupPending:
		if err := c.Cluster.StartRestore(ctx, a, b); err != nil {
			return err
		}
		c.Log.Info("addon restore started", "app", a.AppName, "addon", a.Name, "backup", b.ID, "by", b.RestoreBy)
		return c.Store.SetRestoreStatus(ctx, b.ID, store.BackupRunning, "")
	case !job.Exists:
		return c.Store.SetRestoreStatus(ctx, b.ID, store.BackupFailed, "the restore job disappeared")
	case job.Active:
		return nil
	case job.Failed:
		c.Log.Warn("addon restore failed", "app", a.AppName, "addon", a.Name, "backup", b.ID, "err", job.Message)
		if err := c.Store.SetRestoreStatus(ctx, b.ID, store.BackupFailed, lastLines(job.Message)); err != nil {
			return err
		}
	default:
		c.Log.Info("addon restored", "app", a.AppName, "addon", a.Name, "backup", b.ID)
		if err := c.Store.SetRestoreStatus(ctx, b.ID, store.BackupSucceeded, ""); err != nil {
			return err
		}
	}
	return c.Cluster.DeleteAddonJob(ctx, a, name)
}

// ---- dry run ----

// dryRun passes every state through at once (PAAS_DEPLOYER=dryrun): the
// add-on is ready, copies, backups and restores succeed without data.
func (c *Controller) dryRun(ctx context.Context, a store.Addon, next *store.AddonSecrets) error {
	if a.Status != store.AddonReady {
		c.setState(ctx, a, store.AddonReady, "")
	}
	if next != nil {
		if err := c.completeRotation(ctx, a); err != nil {
			return err
		}
	}
	if a.Kind != store.AddonPostgres {
		return nil
	}
	a.Status = store.AddonReady
	if a.HasBranches() {
		if _, err := c.Store.ExpireAddonBranches(ctx, a, branchGrace); err != nil {
			return err
		}
	} else if err := c.Store.DeleteAddonBranches(ctx, a); err != nil {
		return err
	}
	rows, err := c.Store.ListAddonBranches(ctx, a.ID)
	if err != nil {
		return err
	}
	now := c.Now().UTC()
	zero := int64(0)
	for _, b := range rows {
		switch b.Status {
		case store.BranchPending:
			if _, err := c.Store.StartAddonBranch(ctx, b.ID, b.Generation); err != nil {
				return err
			}
			if _, err := c.Store.FinishAddonBranch(ctx, b.ID, b.Generation,
				store.BranchResult{Mode: b.Mode, SnapshotAt: &now, SizeBytes: &zero}); err != nil {
				return err
			}
		case store.BranchDeleting:
			if err := c.Store.RemoveAddonBranch(ctx, b.ID); err != nil {
				return err
			}
		}
	}
	backups, err := c.Store.ListAddonBackups(ctx, a.ID, 100)
	if err != nil {
		return err
	}
	for _, b := range backups {
		if b.Status == store.BackupPending {
			if err := c.Store.RecordBackupRun(ctx, a.ID, store.BackupRun{Job: b.Job, Trigger: b.Trigger,
				Status: store.BackupSucceeded, SizeBytes: &zero, StartedAt: &now, FinishedAt: &now}); err != nil {
				return err
			}
		}
		if b.RestoreStatus == store.BackupPending {
			if err := c.Store.SetRestoreStatus(ctx, b.ID, store.BackupSucceeded, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- messages ----

// lastLines keeps the end of a Job's log for an error message: the last
// lines name the failure (psql, pg_restore and the scripts print errors
// last).
func lastLines(msg string) string {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < 4; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "==>") {
			keep = append([]string{l}, keep...)
		}
	}
	out := strings.Join(keep, " ⏎ ")
	if len(out) > 1000 {
		out = out[len(out)-1000:]
	}
	return out
}

// FallbackWarning explains why a copy became an empty database ("" when
// it did not).
func FallbackWarning(code string, sourceSize, limit int64) string {
	switch code {
	case "size":
		return fmt.Sprintf("production is %s, above the copy limit of %s: an empty database was created instead",
			HumanBytes(sourceSize), HumanBytes(limit))
	case "disk":
		return fmt.Sprintf("a copy of production (%s) would fill the add-on's volume beyond 80 %%: an empty database was created instead",
			HumanBytes(sourceSize))
	}
	return ""
}

var (
	sizeWarningRe = regexp.MustCompile(`^production is (.+), above the copy limit of (.+): an empty database was created instead$`)
	diskWarningRe = regexp.MustCompile(`^a copy of production \((.+)\) would fill the add-on's volume beyond 80 %: an empty database was created instead$`)
)

// WarningTR is FallbackWarning in Turkish (deployment log, web UI); other
// text passes unchanged.
func WarningTR(w string) string {
	if m := sizeWarningRe.FindStringSubmatch(w); m != nil {
		return fmt.Sprintf("production %s, kopya sınırı %s: kopya yerine boş bir veritabanı oluşturuldu", m[1], m[2])
	}
	if m := diskWarningRe.FindStringSubmatch(w); m != nil {
		return fmt.Sprintf("production'ın kopyası (%s) eklentinin diskini %%80'in üstüne doldururdu: kopya yerine boş bir veritabanı oluşturuldu", m[1])
	}
	return w
}

// HumanBytes formats n with binary units: "512 B", "12.3 MiB", "6.0 GiB".
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
