package deploy

// Faz 22: add-ons (managed Postgres and Redis) in the app's namespace:
//
//	Secret      addon-<n>             passwords: admin-password, password[, next-password]
//	Secret      addon-<n>-branches    role password of each branch database (Postgres)
//	ConfigMap   addon-<n>-init        creates the app role and database on first start (Postgres)
//	PVC         addon-<n>-data        data; addon-<n>-backups: dumps (Postgres)
//	StatefulSet addon-<n>             1 replica, official image, non-root, read-only root, probes
//	Service     addon-<n>             ClusterIP :5432 / :6379
//	CronJob     addon-<n>-backup      daily pg_dump into the backup volume (Postgres, backup_keep > 0)
//	Job         addon-<n>-<op>-…      branch copies and drops, rotation, backups, restores
//
// Server pods carry paas/addon=<n>: the NetworkPolicy paas-addons lets only
// pods of the same namespace reach them, on their port (objects.go). Job
// pods carry paas/addon-of=<n> instead, so neither the Service nor that
// policy selects them. Everything is applied idempotently from the
// database by the reconcile loop (internal/addons); the volumes are
// standalone PVCs rather than volumeClaimTemplates, so a plan change can
// grow them and deleting the add-on deletes them explicitly.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// Labels and keys of add-on objects.
const (
	LabelAddon     = "paas/addon"      // server pods and every add-on object
	LabelAddonKind = "paas/addon-kind" // postgres or redis
	LabelAddonOf   = "paas/addon-of"   // Job pods: the add-on they work on
	LabelAddonJob  = "paas/addon-job"  // Jobs: copy, drop, rotate, backup, restore

	AnnotSecretsVersion = "paas/secrets-version" // rolls the Redis pod after a rotation

	KeyAdminPassword = "admin-password"
	KeyPassword      = "password"
	KeyNextPassword  = "next-password"
)

// Job operations (LabelAddonJob).
const (
	OpCopy    = "copy"
	OpDrop    = "drop"
	OpRotate  = "rotate"
	OpBackup  = "backup"
	OpRestore = "restore"
)

// Add-on defaults (Config).
const (
	DefaultStorageClass   = "local-path" // k3s
	DefaultPostgresImage  = "postgres:16-alpine"
	DefaultRedisImage     = "redis:7-alpine"
	DefaultBackupSchedule = "0 3 * * *"
)

// Resources of a Job pod; every Postgres add-on reserves room for
// addonJobSlots of them in the quota.
const (
	jobCPURequest = "50m"
	jobCPULimit   = "1"
	jobMemory     = "256Mi"
	addonJobSlots = 2
)

// Deadlines of the Jobs.
const (
	shortJobDeadline  = 5 * time.Minute
	backupJobDeadline = time.Hour
	jobTTL            = 24 * time.Hour // finished Jobs the loop forgot
)

// UIDs of the official images' users.
const (
	postgresUID = 70
	redisUID    = 999
	redisGID    = 1000
)

// AddonSource lists an app's add-ons (store.Store); the namespace quota
// grows with them. New takes it from the EnvSource when that implements it.
type AddonSource interface {
	AppAddons(ctx context.Context, app string) ([]store.Addon, error)
}

// AddonSpec is everything ApplyAddon needs about one add-on.
type AddonSpec struct {
	Addon   store.Addon
	Secrets store.AddonSecrets
	// Next is the password of a pending rotation.
	Next *store.AddonSecrets
	// BranchPasswords by database name (Postgres).
	BranchPasswords map[string]string
}

// AddonState is what the cluster says about an add-on's server.
type AddonState struct {
	Ready bool
	// Message explains why it is not ready, or a problem that does not
	// stop it (a volume that could not grow).
	Message string
}

// JobResult is the state of one add-on Job.
type JobResult struct {
	Name      string
	Exists    bool
	Active    bool
	Succeeded bool
	Failed    bool
	// Manual: started by the platform, not by the backup CronJob.
	Manual bool
	// Message is the termination message of its last pod: a JSON report on
	// success, the last log lines on failure.
	Message           string
	Started, Finished *time.Time
}

// ---- names ----

func addonNS(a store.Addon) string { return naming.Namespace(a.AppName) }

// AddonSecretName is the Secret with the add-on's passwords.
func AddonSecretName(a store.Addon) string { return a.Object() }

// AddonBranchSecretName is the Secret with the branch roles' passwords.
func AddonBranchSecretName(a store.Addon) string { return a.Object() + "-branches" }

func addonInitName(a store.Addon) string   { return a.Object() + "-init" }
func addonDataPVC(a store.Addon) string    { return a.Object() + "-data" }
func addonBackupPVC(a store.Addon) string  { return a.Object() + "-backups" }
func addonBackupCron(a store.Addon) string { return a.Object() + "-backup" }

// CopyJobName is the Job that copies (or creates) a branch database.
func CopyJobName(a store.Addon, b store.AddonBranch) string {
	return fmt.Sprintf("%s-copy-%d-%d", a.Object(), b.ID, b.Generation)
}

// DropJobName is the Job that drops a branch database.
func DropJobName(a store.Addon, b store.AddonBranch) string {
	return fmt.Sprintf("%s-drop-%d-%d", a.Object(), b.ID, b.Generation)
}

// RotateJobName is the Job that applies the rotation after secrets version v.
func RotateJobName(a store.Addon) string {
	return fmt.Sprintf("%s-rotate-%d", a.Object(), a.SecretsVersion)
}

// RestoreJobName is the Job that restores a backup into production.
func RestoreJobName(a store.Addon, b store.AddonBackup) string {
	return fmt.Sprintf("%s-restore-%d", a.Object(), b.ID)
}

func addonLabels(a store.Addon) map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelApp: a.AppName, LabelAddon: a.Name, LabelAddonKind: a.Kind}
}

func addonSelector(a store.Addon) map[string]string {
	return map[string]string{LabelApp: a.AppName, LabelAddon: a.Name}
}

func addonJobLabels(a store.Addon, op string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelApp: a.AppName, LabelAddonOf: a.Name, LabelAddonJob: op}
}

// ---- quota ----

// addonReservation adds what add-on a needs to the namespace quota: its
// server pod with the plan's resources and volumes, and for Postgres room
// for its Jobs.
func addonReservation(hard corev1.ResourceList, a store.Addon) {
	plan, ok := store.GetAddonPlan(a.Plan)
	if !ok {
		return
	}
	add := func(name corev1.ResourceName, q resource.Quantity) {
		cur := hard[name]
		cur.Add(q)
		hard[name] = cur
	}
	mem, cpu, storage := resource.MustParse(plan.Memory), resource.MustParse(plan.CPURequest), resource.MustParse(plan.Storage)
	pods, pvcs := int64(1), int64(1)
	if a.Kind == store.AddonPostgres {
		jobMem, jobCPU := resource.MustParse(jobMemory), resource.MustParse(jobCPURequest)
		for i := 0; i < addonJobSlots; i++ {
			mem.Add(jobMem)
			cpu.Add(jobCPU)
		}
		storage.Add(resource.MustParse(plan.Storage)) // the backup volume
		pods += addonJobSlots
		pvcs++
	}
	add(corev1.ResourcePods, *resource.NewQuantity(pods, resource.DecimalSI))
	add(corev1.ResourceRequestsCPU, cpu)
	add(corev1.ResourceRequestsMemory, mem)
	add(corev1.ResourceLimitsMemory, mem)
	add(corev1.ResourceRequestsStorage, storage)
	add(corev1.ResourcePersistentVolumeClaims, *resource.NewQuantity(pvcs, resource.DecimalSI))
}

// quotaHard is the namespace quota of app: the configured base for its
// deployments plus every add-on's reservation. Without add-ons no volume
// can be claimed in the namespace at all.
func (k *Kubernetes) quotaHard(ctx context.Context, app string) (corev1.ResourceList, error) {
	hard := corev1.ResourceList{
		corev1.ResourcePods:                   *resource.NewQuantity(int64(k.cfg.QuotaPods), resource.DecimalSI),
		corev1.ResourceRequestsCPU:            k.q.quotaCPU.DeepCopy(),
		corev1.ResourceLimitsMemory:           k.q.quotaMem.DeepCopy(),
		corev1.ResourceRequestsMemory:         k.q.quotaMem.DeepCopy(),
		corev1.ResourceRequestsStorage:        *resource.NewQuantity(0, resource.BinarySI),
		corev1.ResourcePersistentVolumeClaims: *resource.NewQuantity(0, resource.DecimalSI),
	}
	if k.Addons == nil {
		return hard, nil
	}
	addons, err := k.Addons.AppAddons(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("add-ons: %w", err)
	}
	for _, a := range addons {
		addonReservation(hard, a)
	}
	return hard, nil
}

// ---- apply ----

// ApplyAddon creates or repairs the add-on's objects and reports whether
// its server is ready. It is idempotent: the reconcile loop calls it for
// every add-on on every pass.
func (k *Kubernetes) ApplyAddon(ctx context.Context, spec AddonSpec) (AddonState, error) {
	a := spec.Addon
	plan, ok := store.GetAddonPlan(a.Plan)
	if !ok {
		return AddonState{}, fmt.Errorf("unknown plan %q", a.Plan)
	}
	if err := k.ensureNamespace(ctx, a.AppName); err != nil {
		return AddonState{}, fmt.Errorf("namespace: %w", err)
	}
	if err := k.applyAddonSecrets(ctx, spec); err != nil {
		return AddonState{}, err
	}
	var problems []string
	pvcs := []string{addonDataPVC(a)}
	if a.Kind == store.AddonPostgres {
		pvcs = append(pvcs, addonBackupPVC(a))
		if err := k.applyInitConfig(ctx, a); err != nil {
			return AddonState{}, err
		}
	}
	for _, name := range pvcs {
		msg, err := k.applyPVC(ctx, a, name, plan.Storage)
		if err != nil {
			return AddonState{}, err
		}
		if msg != "" {
			problems = append(problems, msg)
		}
	}
	if err := k.applyAddonService(ctx, a); err != nil {
		return AddonState{}, err
	}
	sts, err := k.applyStatefulSet(ctx, k.statefulSetObject(a, plan))
	if err != nil {
		return AddonState{}, fmt.Errorf("statefulset %s/%s: %w", addonNS(a), a.Object(), err)
	}
	if a.Kind == store.AddonPostgres {
		if err := k.applyBackupCron(ctx, a); err != nil {
			return AddonState{}, err
		}
	}
	state := AddonState{Ready: sts.Status.ReadyReplicas >= 1 && sts.Status.ObservedGeneration >= sts.Generation}
	if !state.Ready {
		state.Message = k.addonPodProblem(ctx, a)
	}
	if len(problems) > 0 {
		state.Message = strings.TrimPrefix(state.Message+"; "+strings.Join(problems, "; "), "; ")
	}
	return state, nil
}

func (k *Kubernetes) applySecret(ctx context.Context, want *corev1.Secret) error {
	_, err := apply(ctx, k.client.CoreV1().Secrets(want.Namespace), want.Name, want,
		func(have *corev1.Secret) bool {
			if sameData(have.Data, want.Data) && !mergeLabels(&have.Labels, want.Labels) {
				return false
			}
			have.Data = want.Data
			return true
		})
	if err != nil {
		return fmt.Errorf("secret %s/%s: %w", want.Namespace, want.Name, err)
	}
	return nil
}

func (k *Kubernetes) applyAddonSecrets(ctx context.Context, spec AddonSpec) error {
	a := spec.Addon
	data := map[string][]byte{KeyPassword: []byte(spec.Secrets.Password)}
	if spec.Secrets.Admin != "" {
		data[KeyAdminPassword] = []byte(spec.Secrets.Admin)
	}
	if spec.Next != nil {
		data[KeyNextPassword] = []byte(spec.Next.Password)
	}
	if err := k.applySecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AddonSecretName(a), Namespace: addonNS(a), Labels: addonLabels(a)},
		Type:       corev1.SecretTypeOpaque, Data: data,
	}); err != nil {
		return err
	}
	if a.Kind != store.AddonPostgres {
		return nil
	}
	branches := make(map[string][]byte, len(spec.BranchPasswords))
	for db, pw := range spec.BranchPasswords {
		branches[db] = []byte(pw)
	}
	return k.applySecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AddonBranchSecretName(a), Namespace: addonNS(a), Labels: addonLabels(a)},
		Type:       corev1.SecretTypeOpaque, Data: branches,
	})
}

func (k *Kubernetes) applyInitConfig(ctx context.Context, a store.Addon) error {
	want := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: addonInitName(a), Namespace: addonNS(a), Labels: addonLabels(a)},
		Data:       map[string]string{"10-paas.sh": postgresInitScript},
	}
	_, err := apply(ctx, k.client.CoreV1().ConfigMaps(want.Namespace), want.Name, want,
		func(have *corev1.ConfigMap) bool {
			if maps.Equal(have.Data, want.Data) {
				return false
			}
			have.Data = want.Data
			return true
		})
	if err != nil {
		return fmt.Errorf("configmap %s/%s: %w", want.Namespace, want.Name, err)
	}
	return nil
}

// storageClass is the class of add-on volumes; nil uses the cluster's
// default class.
func (k *Kubernetes) storageClass() *string {
	if k.cfg.AddonStorageClass == "default" {
		return nil
	}
	return ptr(k.cfg.AddonStorageClass)
}

// applyPVC creates a volume or grows it to size. A volume is never shrunk;
// a class that cannot expand volumes (local-path) answers with an error
// that is returned as a message, not a failure: the add-on keeps running
// on its current volume.
func (k *Kubernetes) applyPVC(ctx context.Context, a store.Addon, name, size string) (string, error) {
	want := resource.MustParse(size)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS(a), Labels: addonLabels(a)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: k.storageClass(),
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: want}},
		},
	}
	pc := k.client.CoreV1().PersistentVolumeClaims(pvc.Namespace)
	have, err := pc.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := pc.Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("volume %s/%s: %w", pvc.Namespace, name, err)
		}
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("volume %s/%s: %w", pvc.Namespace, name, err)
	}
	cur := have.Spec.Resources.Requests[corev1.ResourceStorage]
	if cur.Cmp(want) >= 0 {
		return "", nil
	}
	have.Spec.Resources.Requests[corev1.ResourceStorage] = want
	if _, err := pc.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
		return fmt.Sprintf("volume %s could not grow from %s to %s: %v", name, cur.String(), size, err), nil
	}
	return "", nil
}

func (k *Kubernetes) applyAddonService(ctx context.Context, a store.Addon) error {
	port := int32(a.Port())
	want := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: a.Object(), Namespace: addonNS(a), Labels: addonLabels(a)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: addonSelector(a),
			Ports: []corev1.ServicePort{{
				Name: a.Kind, Port: port, TargetPort: intstr.FromString(a.Kind), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	_, err := apply(ctx, k.client.CoreV1().Services(want.Namespace), want.Name, want,
		func(have *corev1.Service) bool {
			if equality.Semantic.DeepEqual(have.Spec.Selector, want.Spec.Selector) &&
				equality.Semantic.DeepEqual(have.Spec.Ports, want.Spec.Ports) {
				return false
			}
			have.Spec.Selector, have.Spec.Ports = want.Spec.Selector, want.Spec.Ports
			return true
		})
	if err != nil {
		return fmt.Errorf("service %s/%s: %w", want.Namespace, want.Name, err)
	}
	return nil
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
	}}}
}

func emptyDir(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func pvcVolume(name, claim string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
	}}
}

// hardened is the container security context of add-on pods: no
// privilege escalation, no capabilities, read-only root file system.
func hardened() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func podSecurity(uid, gid int64) *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr(true), RunAsUser: ptr(uid), RunAsGroup: ptr(gid), FSGroup: ptr(gid),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func execProbe(cmd []string, initial, period, failures int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: cmd}},
		InitialDelaySeconds: initial, PeriodSeconds: period, TimeoutSeconds: 5, FailureThreshold: failures,
	}
}

func (k *Kubernetes) statefulSetObject(a store.Addon, plan store.AddonPlan) *appsv1.StatefulSet {
	lbl := addonLabels(a)
	mem := resource.MustParse(plan.Memory)
	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(plan.CPURequest), corev1.ResourceMemory: mem},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(plan.CPULimit), corev1.ResourceMemory: mem},
	}
	var pod corev1.PodSpec
	if a.Kind == store.AddonRedis {
		pod = k.redisPod(a, plan, res)
	} else {
		pod = k.postgresPod(a, plan, res)
	}
	pod.AutomountServiceAccountToken = ptr(false)
	pod.EnableServiceLinks = ptr(false)
	pod.TerminationGracePeriodSeconds = ptr(int64(60))
	annot := map[string]string{AnnotSecretsVersion: strconv.Itoa(a.SecretsVersion)}
	if a.Kind == store.AddonPostgres {
		annot = nil // Postgres reads its password from the database, not at start
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: a.Object(), Namespace: addonNS(a), Labels: lbl},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr(int32(1)),
			ServiceName: a.Object(),
			Selector:    &metav1.LabelSelector{MatchLabels: addonSelector(a)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl, Annotations: annot},
				Spec:       pod,
			},
		},
	}
}

func (k *Kubernetes) postgresPod(a store.Addon, plan store.AddonPlan, res corev1.ResourceRequirements) corev1.PodSpec {
	secret := AddonSecretName(a)
	ready := []string{"pg_isready", "-U", store.PostgresAdminUser, "-h", "127.0.0.1", "-p", strconv.Itoa(store.PostgresPort)}
	return corev1.PodSpec{
		SecurityContext: podSecurity(postgresUID, postgresUID),
		Containers: []corev1.Container{{
			Name:  "postgres",
			Image: k.cfg.PostgresImage,
			Args: []string{"-c", fmt.Sprintf("shared_buffers=%dMB", plan.MemoryMB/4),
				"-c", "max_connections=100", "-c", "password_encryption=scram-sha-256"},
			Env: []corev1.EnvVar{
				{Name: "POSTGRES_USER", Value: store.PostgresAdminUser},
				secretEnv("POSTGRES_PASSWORD", secret, KeyAdminPassword),
				secretEnv("APP_PASSWORD", secret, KeyPassword),
				// A subdirectory: a volume's root may hold lost+found.
				{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
			},
			Ports:     []corev1.ContainerPort{{Name: store.AddonPostgres, ContainerPort: store.PostgresPort, Protocol: corev1.ProtocolTCP}},
			Resources: res,
			VolumeMounts: []corev1.VolumeMount{
				{Name: "data", MountPath: "/var/lib/postgresql/data"},
				{Name: "init", MountPath: "/docker-entrypoint-initdb.d", ReadOnly: true},
				{Name: "run", MountPath: "/var/run/postgresql"},
				{Name: "tmp", MountPath: "/tmp"},
			},
			// TCP on 127.0.0.1: during the first start the init scripts run on
			// a server that only listens on its socket, so the pod becomes
			// ready only once the app role and database exist.
			ReadinessProbe:  execProbe(ready, 5, 5, 3),
			LivenessProbe:   execProbe(ready, 60, 10, 6),
			SecurityContext: hardened(),
		}},
		Volumes: []corev1.Volume{
			pvcVolume("data", addonDataPVC(a)),
			{Name: "init", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: addonInitName(a)},
				DefaultMode:          ptr(int32(0o555)),
			}}},
			emptyDir("run"), emptyDir("tmp"),
		},
	}
}

func (k *Kubernetes) redisPod(a store.Addon, plan store.AddonPlan, res corev1.ResourceRequirements) corev1.PodSpec {
	secret := AddonSecretName(a)
	ping := []string{"sh", "-c", "redis-cli ping | grep -q PONG"}
	return corev1.PodSpec{
		SecurityContext: podSecurity(redisUID, redisGID),
		Containers: []corev1.Container{{
			Name:    "redis",
			Image:   k.cfg.RedisImage,
			Command: []string{"redis-server"},
			// noeviction: Redis also backs job queues (Faz 20 workers), where
			// silently evicted keys are lost jobs; maxmemory leaves room
			// below the container limit so writes fail instead of the pod
			// being OOM-killed.
			Args: []string{"--appendonly", "yes", "--dir", "/data", "--requirepass", "$(REDIS_PASSWORD)",
				"--maxmemory", fmt.Sprintf("%dmb", plan.MemoryMB*8/10), "--maxmemory-policy", "noeviction"},
			Env: []corev1.EnvVar{
				secretEnv("REDIS_PASSWORD", secret, KeyPassword),
				secretEnv("REDISCLI_AUTH", secret, KeyPassword),
			},
			Ports:           []corev1.ContainerPort{{Name: store.AddonRedis, ContainerPort: store.RedisPort, Protocol: corev1.ProtocolTCP}},
			Resources:       res,
			VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}, {Name: "tmp", MountPath: "/tmp"}},
			ReadinessProbe:  execProbe(ping, 2, 5, 3),
			LivenessProbe:   execProbe(ping, 30, 10, 6),
			SecurityContext: hardened(),
		}},
		Volumes: []corev1.Volume{pvcVolume("data", addonDataPVC(a)), emptyDir("tmp")},
	}
}

func (k *Kubernetes) applyStatefulSet(ctx context.Context, want *appsv1.StatefulSet) (*appsv1.StatefulSet, error) {
	return apply(ctx, k.client.AppsV1().StatefulSets(want.Namespace), want.Name, want,
		func(have *appsv1.StatefulSet) bool {
			if equality.Semantic.DeepDerivative(want.Spec.Template, have.Spec.Template) &&
				equality.Semantic.DeepEqual(have.Spec.Replicas, want.Spec.Replicas) {
				return false // the API server's defaults are not a change
			}
			have.Labels = want.Labels
			have.Spec.Replicas = want.Spec.Replicas
			have.Spec.Template = want.Spec.Template // selector and serviceName are immutable and unchanged
			return true
		})
}

// applyBackupCron keeps the daily backup CronJob of a Postgres add-on, or
// deletes it when backups are off (backup_keep 0).
func (k *Kubernetes) applyBackupCron(ctx context.Context, a store.Addon) error {
	cc := k.client.BatchV1().CronJobs(addonNS(a))
	name := addonBackupCron(a)
	if a.BackupKeep <= 0 {
		err := cc.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("cronjob %s/%s: %w", addonNS(a), name, err)
		}
		return nil
	}
	job := k.addonJobSpec(a, OpBackup, backupScript, backupJobDeadline, true,
		corev1.EnvVar{Name: "KEEP", Value: strconv.Itoa(a.BackupKeep)},
		corev1.EnvVar{Name: "JOB_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			FieldPath: "metadata.labels['job-name']",
		}}})
	job.Spec.BackoffLimit = ptr(int32(1))
	job.Spec.TTLSecondsAfterFinished = nil // the CronJob's history limits clean up
	want := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: addonNS(a), Labels: addonJobLabels(a, OpBackup)},
		Spec: batchv1.CronJobSpec{
			Schedule:                   k.cfg.BackupSchedule,
			TimeZone:                   ptr("Etc/UTC"),
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			StartingDeadlineSeconds:    ptr(int64(3600)),
			SuccessfulJobsHistoryLimit: ptr(int32(cronHistory)),
			FailedJobsHistoryLimit:     ptr(int32(cronHistory)),
			JobTemplate:                batchv1.JobTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: job.Labels}, Spec: job.Spec},
		},
	}
	_, err := apply(ctx, cc, name, want, func(have *batchv1.CronJob) bool {
		if have.Spec.Schedule == want.Spec.Schedule &&
			equality.Semantic.DeepDerivative(want.Spec.JobTemplate, have.Spec.JobTemplate) {
			return false
		}
		have.Labels = want.Labels
		have.Spec = want.Spec
		return true
	})
	if err != nil {
		return fmt.Errorf("cronjob %s/%s: %w", addonNS(a), name, err)
	}
	return nil
}

// addonPodProblem summarizes why the add-on's server is not ready.
func (k *Kubernetes) addonPodProblem(ctx context.Context, a store.Addon) string {
	pods, err := k.client.CoreV1().Pods(addonNS(a)).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(addonSelector(a)).String(),
	})
	if err != nil {
		return "reading pods: " + err.Error()
	}
	if len(pods.Items) == 0 {
		return "waiting for the pod to be created"
	}
	return "pod " + pods.Items[0].Name + ": " + podState(&pods.Items[0])
}

// ---- jobs ----

// addonJobSpec is a Job of a Postgres add-on: the postgres image running
// script as the image's postgres user, with the admin password, the
// server's address and extra env; backups mounts the backup volume.
func (k *Kubernetes) addonJobSpec(a store.Addon, op, script string, deadline time.Duration, backups bool, env ...corev1.EnvVar) *batchv1.Job {
	lbl := addonJobLabels(a, op)
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	volumes := []corev1.Volume{emptyDir("tmp")}
	if backups {
		mounts = append(mounts, corev1.VolumeMount{Name: "backups", MountPath: "/backups"})
		volumes = append(volumes, pvcVolume("backups", addonBackupPVC(a)))
	}
	base := []corev1.EnvVar{
		secretEnv("ADMIN_PASSWORD", AddonSecretName(a), KeyAdminPassword),
		{Name: "PGHOST", Value: a.Host()},
		{Name: "PGPORT", Value: strconv.Itoa(store.PostgresPort)},
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: addonNS(a), Labels: lbl},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr(int32(0)),
			ActiveDeadlineSeconds:   ptr(int64(deadline.Seconds())),
			TTLSecondsAfterFinished: ptr(int32(jobTTL.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr(false),
					EnableServiceLinks:           ptr(false),
					SecurityContext:              podSecurity(postgresUID, postgresUID),
					Containers: []corev1.Container{{
						Name:    op,
						Image:   k.cfg.PostgresImage,
						Command: []string{"sh", "-c", script},
						Env:     append(base, env...),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(jobCPURequest), corev1.ResourceMemory: resource.MustParse(jobMemory)},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(jobCPULimit), corev1.ResourceMemory: resource.MustParse(jobMemory)},
						},
						VolumeMounts:             mounts,
						SecurityContext:          hardened(),
						TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

// startJob creates job under name; an existing Job of that name (a retry
// of the reconcile loop, or another replica) is not an error.
func (k *Kubernetes) startJob(ctx context.Context, job *batchv1.Job, name string) error {
	job.Name = name
	_, err := k.client.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("job %s/%s: %w", job.Namespace, name, err)
	}
	return nil
}

// CopyOptions describe one branch copy.
type CopyOptions struct {
	// Mode: store.PreviewCopy or store.PreviewEmpty.
	Mode string
	// AnonymizeSQL is the compiled column rules (internal/addons), run as
	// the superuser; UserSQL the member's statements, run as the branch role.
	AnonymizeSQL, UserSQL string
	// MaxBytes: a production database above it is not copied (empty instead).
	MaxBytes int64
	Deadline time.Duration
}

// StartCopy starts the Job that (re)creates branch database b.
func (k *Kubernetes) StartCopy(ctx context.Context, a store.Addon, b store.AddonBranch, o CopyOptions) error {
	plan, _ := store.GetAddonPlan(a.Plan)
	capacity := resource.MustParse(plan.Storage)
	deadline := o.Deadline
	if deadline <= 0 {
		deadline = 10 * time.Minute
	}
	job := k.addonJobSpec(a, OpCopy, copyScript, deadline, false,
		secretEnv("BRANCH_PASSWORD", AddonBranchSecretName(a), b.Database),
		corev1.EnvVar{Name: "SOURCE_DB", Value: store.PostgresAppDatabase},
		corev1.EnvVar{Name: "TARGET_DB", Value: b.Database},
		corev1.EnvVar{Name: "MODE", Value: o.Mode},
		corev1.EnvVar{Name: "MAX_BYTES", Value: strconv.FormatInt(o.MaxBytes, 10)},
		corev1.EnvVar{Name: "CAPACITY_BYTES", Value: strconv.FormatInt(capacity.Value(), 10)},
		corev1.EnvVar{Name: "ANON_SQL", Value: o.AnonymizeSQL},
		corev1.EnvVar{Name: "USER_SQL", Value: o.UserSQL},
	)
	job.Annotations = map[string]string{AnnotBranch: b.Branch}
	return k.startJob(ctx, job, CopyJobName(a, b))
}

// StartDrop starts the Job that drops branch database b and its role.
func (k *Kubernetes) StartDrop(ctx context.Context, a store.Addon, b store.AddonBranch) error {
	job := k.addonJobSpec(a, OpDrop, dropScript, shortJobDeadline, false,
		corev1.EnvVar{Name: "TARGET_DB", Value: b.Database})
	job.Annotations = map[string]string{AnnotBranch: b.Branch}
	return k.startJob(ctx, job, DropJobName(a, b))
}

// StartRotate starts the Job that gives the app role the pending password
// (the add-on Secret's next-password, ApplyAddon).
func (k *Kubernetes) StartRotate(ctx context.Context, a store.Addon) error {
	job := k.addonJobSpec(a, OpRotate, rotateScript, shortJobDeadline, false,
		secretEnv("NEW_PASSWORD", AddonSecretName(a), KeyNextPassword))
	return k.startJob(ctx, job, RotateJobName(a))
}

// StartBackup starts a manual backup Job named name.
func (k *Kubernetes) StartBackup(ctx context.Context, a store.Addon, name string) error {
	keep := a.BackupKeep
	if keep <= 0 {
		keep = 1 // backups are off: keep only this one
	}
	job := k.addonJobSpec(a, OpBackup, backupScript, backupJobDeadline, true,
		corev1.EnvVar{Name: "KEEP", Value: strconv.Itoa(keep)},
		corev1.EnvVar{Name: "JOB_NAME", Value: name})
	job.Annotations = map[string]string{annotManualJob: "manual"}
	return k.startJob(ctx, job, name)
}

// StartRestore starts the Job that restores backup b into production.
func (k *Kubernetes) StartRestore(ctx context.Context, a store.Addon, b store.AddonBackup) error {
	job := k.addonJobSpec(a, OpRestore, restoreScript, backupJobDeadline, true,
		corev1.EnvVar{Name: "BACKUP_FILE", Value: b.File()})
	return k.startJob(ctx, job, RestoreJobName(a, b))
}

// AddonJob reports the state of the add-on's Job name (Exists false when
// there is none).
func (k *Kubernetes) AddonJob(ctx context.Context, a store.Addon, name string) (JobResult, error) {
	j, err := k.client.BatchV1().Jobs(addonNS(a)).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return JobResult{Name: name}, nil
	}
	if err != nil {
		return JobResult{Name: name}, err
	}
	return k.jobResult(ctx, j), nil
}

func (k *Kubernetes) jobResult(ctx context.Context, j *batchv1.Job) JobResult {
	js := jobStatus(j)
	r := JobResult{Name: j.Name, Exists: true, Manual: j.Annotations[annotManualJob] == "manual",
		Started: js.Started, Finished: js.Finished}
	switch js.Status {
	case "succeeded":
		r.Succeeded = true
	case "failed":
		r.Failed = true
		for _, c := range j.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason == "DeadlineExceeded" {
				r.Message = "the job ran out of time: " + c.Message
			}
		}
	default:
		r.Active = true
	}
	if r.Succeeded || r.Failed {
		if msg := k.jobMessage(ctx, j); msg != "" {
			r.Message = strings.TrimSpace(strings.TrimPrefix(r.Message+"\n"+msg, "\n"))
		}
	}
	return r
}

// jobMessage is the termination message of the Job's newest pod.
func (k *Kubernetes) jobMessage(ctx context.Context, j *batchv1.Job) string {
	pods, err := k.client.CoreV1().Pods(j.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(map[string]string{"job-name": j.Name}).String(),
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	sort.Slice(pods.Items, func(x, y int) bool {
		return pods.Items[y].CreationTimestamp.Before(&pods.Items[x].CreationTimestamp)
	})
	for _, cs := range pods.Items[0].Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			if t.Message != "" {
				return t.Message
			}
			return fmt.Sprintf("%s (exit code %d)", t.Reason, t.ExitCode)
		}
	}
	return ""
}

// DeleteAddonJob deletes a Job and its pods.
func (k *Kubernetes) DeleteAddonJob(ctx context.Context, a store.Addon, name string) error {
	err := k.client.BatchV1().Jobs(addonNS(a)).Delete(ctx, name,
		metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// AddonJobPods reports whether pods of the Job name still exist (a deleted
// Job's pods terminate in the background).
func (k *Kubernetes) AddonJobPods(ctx context.Context, a store.Addon, name string) (bool, error) {
	pods, err := k.client.CoreV1().Pods(addonNS(a)).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(map[string]string{"job-name": name}).String(),
	})
	if err != nil {
		return false, err
	}
	return len(pods.Items) > 0, nil
}

// BackupRuns returns the add-on's backup Jobs, scheduled and manual.
func (k *Kubernetes) BackupRuns(ctx context.Context, a store.Addon) ([]JobResult, error) {
	jobs, err := k.client.BatchV1().Jobs(addonNS(a)).List(ctx, metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(map[string]string{LabelAddonOf: a.Name, LabelAddonJob: OpBackup}).String(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]JobResult, 0, len(jobs.Items))
	for i := range jobs.Items {
		out = append(out, k.jobResult(ctx, &jobs.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---- delete ----

// DeleteAddon deletes every object of the add-on, volumes included, and
// reports whether they are all gone (pods terminate and volumes are
// released in the background; the reconcile loop calls again).
func (k *Kubernetes) DeleteAddon(ctx context.Context, a store.Addon) (bool, error) {
	ns := addonNS(a)
	bg := metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)}
	ignore := func(err error) error {
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	errs := []error{
		ignore(k.client.AppsV1().StatefulSets(ns).Delete(ctx, a.Object(), bg)),
		ignore(k.client.BatchV1().CronJobs(ns).Delete(ctx, addonBackupCron(a), bg)),
		ignore(k.client.CoreV1().Services(ns).Delete(ctx, a.Object(), metav1.DeleteOptions{})),
		ignore(k.client.CoreV1().ConfigMaps(ns).Delete(ctx, addonInitName(a), metav1.DeleteOptions{})),
		ignore(k.client.CoreV1().Secrets(ns).Delete(ctx, AddonSecretName(a), metav1.DeleteOptions{})),
		ignore(k.client.CoreV1().Secrets(ns).Delete(ctx, AddonBranchSecretName(a), metav1.DeleteOptions{})),
		ignore(k.client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, addonDataPVC(a), metav1.DeleteOptions{})),
		ignore(k.client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, addonBackupPVC(a), metav1.DeleteOptions{})),
	}
	jobs := k8slabels.SelectorFromSet(map[string]string{LabelAddonOf: a.Name}).String()
	list, err := k.client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: jobs})
	errs = append(errs, err)
	if list != nil {
		for _, j := range list.Items {
			errs = append(errs, ignore(k.client.BatchV1().Jobs(ns).Delete(ctx, j.Name, bg)))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}

	// Gone means: no server or Job pod and no volume left.
	for _, sel := range []string{k8slabels.SelectorFromSet(addonSelector(a)).String(), jobs} {
		pods, err := k.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return false, err
		}
		if len(pods.Items) > 0 {
			return false, nil
		}
	}
	for _, name := range []string{addonDataPVC(a), addonBackupPVC(a)} {
		_, err := k.client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return false, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	// The quota shrinks once the row is gone (the loop applies it again).
	return true, nil
}

// ApplyQuota brings the app namespace's quota in line with its add-ons
// (after one was deleted).
func (k *Kubernetes) ApplyQuota(ctx context.Context, app string) error {
	return k.ensureNamespace(ctx, app)
}
