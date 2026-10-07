package deploy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nisagwn/paas/internal/store"
)

// fakeAddons is an AddonSource.
type fakeAddons []store.Addon

func (f fakeAddons) AppAddons(context.Context, string) ([]store.Addon, error) { return f, nil }

var (
	pgAddon = store.Addon{ID: 3, AppID: 7, AppName: "blog", Kind: store.AddonPostgres, Name: "db", Plan: "hobby",
		Status: store.AddonProvisioning, PreviewMode: store.PreviewCopy, BackupKeep: 7, SecretsVersion: 1}
	redisAddon = store.Addon{ID: 4, AppID: 7, AppName: "blog", Kind: store.AddonRedis, Name: "cache", Plan: "hobby",
		Status: store.AddonProvisioning, PreviewMode: store.PreviewShared, SecretsVersion: 2}
	pgSpec = AddonSpec{Addon: pgAddon, Secrets: store.AddonSecrets{Admin: "adminpw", Password: "apppw"},
		BranchPasswords: map[string]string{"preview_dev": "branchpw"}}
)

func envNamed(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func secretRef(c corev1.Container, name string) string {
	e := envNamed(c, name)
	if e == nil || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
		return ""
	}
	return e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
}

func checkHardened(t *testing.T, what string, pod corev1.PodSpec, uid int64) {
	t.Helper()
	ps, cs := pod.SecurityContext, pod.Containers[0].SecurityContext
	if ps == nil || ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot || *ps.RunAsUser != uid ||
		ps.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("%s: pod security context %+v", what, ps)
	}
	if cs == nil || *cs.AllowPrivilegeEscalation || !*cs.ReadOnlyRootFilesystem || cs.Capabilities.Drop[0] != "ALL" {
		t.Errorf("%s: container security context %+v", what, cs)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("%s: service account token mounted", what)
	}
}

func TestApplyAddonPostgres(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	k.Addons = fakeAddons{pgAddon}
	ctx := context.Background()
	st, err := k.ApplyAddon(ctx, pgSpec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Ready || !strings.Contains(st.Message, "waiting for the pod") {
		t.Fatalf("state = %+v", st)
	}

	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, "addon-db", metav1.GetOptions{})
	if err != nil || string(sec.Data[KeyAdminPassword]) != "adminpw" || string(sec.Data[KeyPassword]) != "apppw" ||
		sec.Data[KeyNextPassword] != nil || sec.Labels[LabelAddon] != "db" {
		t.Fatalf("secret: %+v %v", sec, err)
	}
	if b, err := cs.CoreV1().Secrets(ns).Get(ctx, "addon-db-branches", metav1.GetOptions{}); err != nil || string(b.Data["preview_dev"]) != "branchpw" {
		t.Fatalf("branch secret: %+v %v", b, err)
	}
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "addon-db-init", metav1.GetOptions{})
	if err != nil || !strings.Contains(cm.Data["10-paas.sh"], "CREATE DATABASE app OWNER app") {
		t.Fatalf("init configmap: %+v %v", cm, err)
	}
	for _, name := range []string{"addon-db-data", "addon-db-backups"} {
		pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil || *pvc.Spec.StorageClassName != "local-path" || pvc.Spec.Resources.Requests.Storage().String() != "1Gi" {
			t.Fatalf("pvc %s: %+v %v", name, pvc, err)
		}
	}
	svc, err := cs.CoreV1().Services(ns).Get(ctx, "addon-db", metav1.GetOptions{})
	if err != nil || svc.Spec.Ports[0].Port != 5432 || svc.Spec.Selector[LabelAddon] != "db" || svc.Spec.Selector[LabelAddonOf] != "" {
		t.Fatalf("service: %+v %v", svc, err)
	}

	sts, err := cs.AppsV1().StatefulSets(ns).Get(ctx, "addon-db", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := sts.Spec.Template.Spec
	c := pod.Containers[0]
	checkHardened(t, "postgres", pod, postgresUID)
	if c.Image != DefaultPostgresImage || *sts.Spec.Replicas != 1 || sts.Spec.ServiceName != "addon-db" {
		t.Errorf("statefulset: image %s replicas %d", c.Image, *sts.Spec.Replicas)
	}
	if secretRef(c, "POSTGRES_PASSWORD") != "addon-db/admin-password" || secretRef(c, "APP_PASSWORD") != "addon-db/password" ||
		envNamed(c, "PGDATA").Value != "/var/lib/postgresql/data/pgdata" {
		t.Errorf("env: %+v", c.Env)
	}
	if !slices.Contains(c.Args, "shared_buffers=64MB") || c.Resources.Limits.Memory().String() != "256Mi" ||
		c.Resources.Requests.Memory().String() != "256Mi" {
		t.Errorf("args/resources: %v %+v", c.Args, c.Resources)
	}
	if got := strings.Join(c.ReadinessProbe.Exec.Command, " "); got != "pg_isready -U postgres -h 127.0.0.1 -p 5432" || c.LivenessProbe == nil {
		t.Errorf("probes: %q", got)
	}
	if sts.Spec.Template.Labels[LabelAddon] != "db" || sts.Spec.Template.Annotations[AnnotSecretsVersion] != "" {
		t.Errorf("pod labels %v annotations %v", sts.Spec.Template.Labels, sts.Spec.Template.Annotations)
	}

	cj, err := cs.BatchV1().CronJobs(ns).Get(ctx, "addon-db-backup", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	jc := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if cj.Spec.Schedule != DefaultBackupSchedule || *cj.Spec.TimeZone != "Etc/UTC" || cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent ||
		envNamed(jc, "KEEP").Value != "7" || envNamed(jc, "JOB_NAME").ValueFrom.FieldRef.FieldPath != "metadata.labels['job-name']" ||
		cj.Spec.JobTemplate.Labels[LabelAddonOf] != "db" || cj.Spec.JobTemplate.Labels[LabelAddon] != "" {
		t.Errorf("backup cronjob: %+v", cj.Spec)
	}
	checkHardened(t, "backup job", cj.Spec.JobTemplate.Spec.Template.Spec, postgresUID)

	// The quota grows by the add-on: server pod + 2 job slots, plan memory +
	// 2 × job memory, data + backup volume.
	q, err := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "paas", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hard := q.Spec.Hard
	wantMem := resource.MustParse("4Gi")
	wantMem.Add(resource.MustParse("768Mi"))
	if hard.Pods().Value() != 23 || hard.Name(corev1.ResourceRequestsStorage, "").String() != "2Gi" ||
		hard.Name(corev1.ResourcePersistentVolumeClaims, "").Value() != 2 ||
		hard.Name(corev1.ResourceLimitsMemory, "").Cmp(wantMem) != 0 || hard.Name(corev1.ResourceRequestsCPU, "").String() != "1150m" {
		t.Errorf("quota: %v", hard)
	}

	// Network policies: app pods keep the kube-system rule but it no longer
	// selects add-on servers; those admit only this namespace on their ports.
	np, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, "paas", metav1.GetOptions{})
	if err != nil || np.Spec.PodSelector.MatchExpressions[0].Key != LabelAddon ||
		np.Spec.PodSelector.MatchExpressions[0].Operator != metav1.LabelSelectorOpDoesNotExist {
		t.Fatalf("paas policy: %+v %v", np, err)
	}
	ap, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, "paas-addons", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	from := ap.Spec.Ingress[0].From
	if ap.Spec.PodSelector.MatchExpressions[0].Operator != metav1.LabelSelectorOpExists || len(from) != 1 ||
		from[0].NamespaceSelector != nil || from[0].PodSelector == nil || len(ap.Spec.Ingress[0].Ports) != 2 ||
		ap.Spec.Ingress[0].Ports[0].Port.IntValue() != 5432 || ap.Spec.Ingress[0].Ports[1].Port.IntValue() != 6379 {
		t.Errorf("addon policy: %+v", ap.Spec)
	}

	// Ready once the StatefulSet reports a ready replica; applying again
	// changes nothing.
	sts.Status = appsv1.StatefulSetStatus{ReadyReplicas: 1, Replicas: 1, ObservedGeneration: sts.Generation}
	if _, err := cs.AppsV1().StatefulSets(ns).UpdateStatus(ctx, sts, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs.ClearActions()
	if st, err = k.ApplyAddon(ctx, pgSpec); err != nil || !st.Ready || st.Message != "" {
		t.Fatalf("second apply: %+v %v", st, err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "create" {
			t.Errorf("idempotent apply did %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}

	// A rotation puts the next password into the Secret; backups off
	// removes the CronJob.
	spec := pgSpec
	spec.Next = &store.AddonSecrets{Admin: "adminpw", Password: "newpw"}
	spec.Addon.BackupKeep = 0
	if _, err := k.ApplyAddon(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if sec, _ := cs.CoreV1().Secrets(ns).Get(ctx, "addon-db", metav1.GetOptions{}); string(sec.Data[KeyNextPassword]) != "newpw" {
		t.Errorf("next password: %v", sec.Data)
	}
	if _, err := cs.BatchV1().CronJobs(ns).Get(ctx, "addon-db-backup", metav1.GetOptions{}); err == nil {
		t.Error("backup cronjob kept with backup_keep 0")
	}
}

func TestApplyAddonRedis(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	k.Addons = fakeAddons{redisAddon}
	ctx := context.Background()
	if _, err := k.ApplyAddon(ctx, AddonSpec{Addon: redisAddon, Secrets: store.AddonSecrets{Password: "redispw"}}); err != nil {
		t.Fatal(err)
	}
	sts, err := cs.AppsV1().StatefulSets(ns).Get(ctx, "addon-cache", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	checkHardened(t, "redis", sts.Spec.Template.Spec, redisUID)
	args := strings.Join(c.Args, " ")
	for _, want := range []string{"--appendonly yes", "--requirepass $(REDIS_PASSWORD)", "--maxmemory 204mb", "--maxmemory-policy noeviction"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if secretRef(c, "REDIS_PASSWORD") != "addon-cache/password" || secretRef(c, "REDISCLI_AUTH") != "addon-cache/password" ||
		sts.Spec.Template.Annotations[AnnotSecretsVersion] != "2" || c.Ports[0].ContainerPort != 6379 {
		t.Errorf("redis pod: %+v %v", c, sts.Spec.Template.Annotations)
	}
	for _, name := range []string{"addon-cache-backups", "addon-cache-branches"} {
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			t.Errorf("redis has %s", name)
		}
	}
	if _, err := cs.BatchV1().CronJobs(ns).Get(ctx, "addon-cache-backup", metav1.GetOptions{}); err == nil {
		t.Error("redis has a backup cronjob")
	}
	q, _ := cs.CoreV1().ResourceQuotas(ns).Get(ctx, "paas", metav1.GetOptions{})
	if q.Spec.Hard.Pods().Value() != 21 || q.Spec.Hard.Name(corev1.ResourcePersistentVolumeClaims, "").Value() != 1 {
		t.Errorf("quota: %v", q.Spec.Hard)
	}

	// A rotation (new secrets version) rolls the pod.
	rotated := redisAddon
	rotated.SecretsVersion = 3
	if _, err := k.ApplyAddon(ctx, AddonSpec{Addon: rotated, Secrets: store.AddonSecrets{Password: "new"}}); err != nil {
		t.Fatal(err)
	}
	sts, _ = cs.AppsV1().StatefulSets(ns).Get(ctx, "addon-cache", metav1.GetOptions{})
	if sts.Spec.Template.Annotations[AnnotSecretsVersion] != "3" {
		t.Errorf("annotation after rotation: %v", sts.Spec.Template.Annotations)
	}
}

func TestQuotaWithoutAddons(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	if err := k.ensureNamespace(context.Background(), "blog"); err != nil {
		t.Fatal(err)
	}
	q, _ := cs.CoreV1().ResourceQuotas(ns).Get(context.Background(), "paas", metav1.GetOptions{})
	if q.Spec.Hard.Name(corev1.ResourceRequestsStorage, "").Value() != 0 ||
		q.Spec.Hard.Name(corev1.ResourcePersistentVolumeClaims, "").Value() != 0 || q.Spec.Hard.Pods().Value() != 20 {
		t.Errorf("quota without add-ons: %v", q.Spec.Hard)
	}
}

func TestApplyAddonGrowsVolume(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	ctx := context.Background()
	if _, err := k.ApplyAddon(ctx, pgSpec); err != nil {
		t.Fatal(err)
	}
	bigger := pgSpec
	bigger.Addon.Plan = "standard"
	st, err := k.ApplyAddon(ctx, bigger)
	if err != nil || st.Message != "waiting for the pod to be created" {
		t.Fatalf("grow: %+v %v", st, err)
	}
	if pvc, _ := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "addon-db-data", metav1.GetOptions{}); pvc.Spec.Resources.Requests.Storage().String() != "5Gi" {
		t.Errorf("volume not grown: %v", pvc.Spec.Resources.Requests)
	}
	sts, _ := cs.AppsV1().StatefulSets(ns).Get(ctx, "addon-db", metav1.GetOptions{})
	if sts.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String() != "512Mi" {
		t.Errorf("plan resources not applied")
	}

	// A storage class that cannot expand: the add-on keeps running, the
	// message says why the volume did not grow.
	cs.PrependReactor("update", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("only dynamically provisioned pvc can be resized")
	})
	pro := pgSpec
	pro.Addon.Plan = "pro"
	st, err = k.ApplyAddon(ctx, pro)
	if err != nil || !strings.Contains(st.Message, "volume addon-db-data could not grow from 5Gi to 20Gi") {
		t.Fatalf("no expansion: %+v %v", st, err)
	}
}

// finishJob plays the Job controller: the Job completes (or fails) and its
// pod ends with message.
func finishJob(t *testing.T, cs *fake.Clientset, name string, ok bool, message string) {
	t.Helper()
	ctx := context.Background()
	j, err := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cond := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	if !ok {
		cond = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}
	}
	now := metav1.Now()
	j.Status = batchv1.JobStatus{StartTime: &now, CompletionTime: &now, Conditions: []batchv1.JobCondition{cond}}
	if _, err := cs.BatchV1().Jobs(ns).UpdateStatus(ctx, j, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-x1", Namespace: ns, Labels: map[string]string{"job-name": name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message, ExitCode: map[bool]int32{true: 0, false: 1}[ok]}},
		}}},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestAddonJobs(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	ctx := context.Background()
	b := store.AddonBranch{ID: 11, Branch: "feature/x", Database: "preview_feature_x_1a2b3c4d", Generation: 2}
	opts := CopyOptions{Mode: store.PreviewCopy, AnonymizeSQL: "SET x;", UserSQL: "UPDATE t SET a = 1;", MaxBytes: 5 << 30}
	if err := k.StartCopy(ctx, pgAddon, b, opts); err != nil {
		t.Fatal(err)
	}
	if err := k.StartCopy(ctx, pgAddon, b, opts); err != nil { // a repeated step finds the same Job
		t.Fatalf("second start: %v", err)
	}
	name := CopyJobName(pgAddon, b)
	if name != "addon-db-copy-11-2" {
		t.Fatalf("job name %q", name)
	}
	j, err := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := j.Spec.Template.Spec.Containers[0]
	checkHardened(t, "copy job", j.Spec.Template.Spec, postgresUID)
	for k, want := range map[string]string{
		"TARGET_DB": b.Database, "SOURCE_DB": "app", "MODE": "copy", "MAX_BYTES": "5368709120",
		"CAPACITY_BYTES": "1073741824", "ANON_SQL": "SET x;", "USER_SQL": "UPDATE t SET a = 1;", "PGHOST": "addon-db.app-blog.svc",
	} {
		if e := envNamed(c, k); e == nil || e.Value != want {
			t.Errorf("env %s = %+v, want %q", k, e, want)
		}
	}
	if secretRef(c, "BRANCH_PASSWORD") != "addon-db-branches/"+b.Database || secretRef(c, "ADMIN_PASSWORD") != "addon-db/admin-password" {
		t.Errorf("secret refs: %+v", c.Env)
	}
	if j.Labels[LabelAddonOf] != "db" || j.Labels[LabelAddonJob] != OpCopy || j.Spec.Template.Labels[LabelAddon] != "" ||
		*j.Spec.BackoffLimit != 0 || *j.Spec.ActiveDeadlineSeconds != 600 ||
		c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError || c.Command[2] != copyScript {
		t.Errorf("job: %+v", j)
	}

	res, err := k.AddonJob(ctx, pgAddon, name)
	if err != nil || !res.Exists || !res.Active {
		t.Fatalf("running job: %+v %v", res, err)
	}
	finishJob(t, cs, name, true, `{"mode":"empty","warning":"size","snapshot":"2026-10-07T05:27:04Z","size":7765015,"source_size":6442450944}`)
	res, _ = k.AddonJob(ctx, pgAddon, name)
	rep, ok := ParseCopyReport(res.Message)
	if !res.Succeeded || !ok || rep.Mode != "empty" || rep.Warning != "size" || rep.SourceSize != 6<<30 {
		t.Fatalf("finished job: %+v %+v", res, rep)
	}
	if err := k.DeleteAddonJob(ctx, pgAddon, name); err != nil {
		t.Fatal(err)
	}
	if res, _ := k.AddonJob(ctx, pgAddon, name); res.Exists {
		t.Error("job not deleted")
	}

	// A failed Job reports its last log lines.
	if err := k.StartDrop(ctx, pgAddon, b); err != nil {
		t.Fatal(err)
	}
	finishJob(t, cs, DropJobName(pgAddon, b), false, "ERROR:  database is being accessed by other users")
	if res, _ := k.AddonJob(ctx, pgAddon, DropJobName(pgAddon, b)); !res.Failed || !strings.Contains(res.Message, "being accessed") {
		t.Errorf("failed job: %+v", res)
	}

	// Rotation reads the next password; restore and manual backups mount
	// the backup volume.
	if err := k.StartRotate(ctx, pgAddon); err != nil {
		t.Fatal(err)
	}
	rj, _ := cs.BatchV1().Jobs(ns).Get(ctx, "addon-db-rotate-1", metav1.GetOptions{})
	if secretRef(rj.Spec.Template.Spec.Containers[0], "NEW_PASSWORD") != "addon-db/next-password" {
		t.Errorf("rotate job env: %+v", rj.Spec.Template.Spec.Containers[0].Env)
	}
	backup := store.AddonBackup{ID: 9, Job: "addon-db-backup-m9"}
	if err := k.StartRestore(ctx, pgAddon, backup); err != nil {
		t.Fatal(err)
	}
	rs, _ := cs.BatchV1().Jobs(ns).Get(ctx, "addon-db-restore-9", metav1.GetOptions{})
	if envNamed(rs.Spec.Template.Spec.Containers[0], "BACKUP_FILE").Value != "addon-db-backup-m9.dump" ||
		rs.Spec.Template.Spec.Volumes[1].PersistentVolumeClaim.ClaimName != "addon-db-backups" {
		t.Errorf("restore job: %+v", rs.Spec.Template.Spec)
	}
	if err := k.StartBackup(ctx, pgAddon, "addon-db-backup-m10"); err != nil {
		t.Fatal(err)
	}
	finishJob(t, cs, "addon-db-backup-m10", true, `{"file":"addon-db-backup-m10.dump","size":35034,"kept":["addon-db-backup-m10.dump"]}`)
	runs, err := k.BackupRuns(ctx, pgAddon)
	if err != nil || len(runs) != 1 || !runs[0].Manual || !runs[0].Succeeded {
		t.Fatalf("backup runs: %+v %v", runs, err)
	}
	if rep, ok := ParseBackupReport(runs[0].Message); !ok || rep.Size != 35034 || rep.Kept[0] != "addon-db-backup-m10.dump" {
		t.Errorf("backup report: %+v", rep)
	}
}

func TestDeleteAddon(t *testing.T) {
	k, cs := newDeployer(t, nil, time.Second)
	ctx := context.Background()
	if _, err := k.ApplyAddon(ctx, pgSpec); err != nil {
		t.Fatal(err)
	}
	if err := k.StartCopy(ctx, pgAddon, store.AddonBranch{ID: 1, Database: "preview_x"}, CopyOptions{Mode: "copy"}); err != nil {
		t.Fatal(err)
	}
	// A server pod still terminating keeps the add-on "not gone".
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "addon-db-0", Namespace: ns, Labels: addonSelector(pgAddon)}}
	cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	gone, err := k.DeleteAddon(ctx, pgAddon)
	if err != nil || gone {
		t.Fatalf("delete with a pod left: %v %v", gone, err)
	}
	cs.CoreV1().Pods(ns).Delete(ctx, "addon-db-0", metav1.DeleteOptions{})
	if gone, err = k.DeleteAddon(ctx, pgAddon); err != nil || !gone {
		t.Fatalf("delete: %v %v", gone, err)
	}
	for _, check := range []func() error{
		func() error {
			_, err := cs.AppsV1().StatefulSets(ns).Get(ctx, "addon-db", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "addon-db-data", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "addon-db-backups", metav1.GetOptions{})
			return err
		},
		func() error { _, err := cs.CoreV1().Secrets(ns).Get(ctx, "addon-db", metav1.GetOptions{}); return err },
		func() error {
			_, err := cs.BatchV1().CronJobs(ns).Get(ctx, "addon-db-backup", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.BatchV1().Jobs(ns).Get(ctx, "addon-db-copy-1-0", metav1.GetOptions{})
			return err
		},
	} {
		if err := check(); err == nil {
			t.Error("an object of the add-on is left")
		}
	}
}
