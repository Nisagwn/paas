package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
)

// policyName names the per-namespace ResourceQuota and LimitRange.
const policyName = "paas"

func ptr[T any](v T) *T { return &v }

// resourceClient is the subset of every typed client that apply needs.
type resourceClient[T any] interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (T, error)
	Create(ctx context.Context, obj T, opts metav1.CreateOptions) (T, error)
	Update(ctx context.Context, obj T, opts metav1.UpdateOptions) (T, error)
}

// apply creates want or, if the object exists, lets merge copy the fields
// paas owns onto the live object and updates it when merge reports a
// change. Re-running a deployment after a crash, or two deployments of the
// same app racing on the namespace, therefore never fails.
func apply[T any](ctx context.Context, c resourceClient[T], name string, want T, merge func(have T) bool) (T, error) {
	var zero T
	for attempt := 1; ; attempt++ {
		have, err := c.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			got, err := c.Create(ctx, want, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) && attempt < 3 {
				continue // lost a race; update instead
			}
			return got, err
		}
		if err != nil {
			return zero, err
		}
		if !merge(have) {
			return have, nil
		}
		got, err := c.Update(ctx, have, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) && attempt < 3 {
			continue
		}
		return got, err
	}
}

// mergeLabels adds want to *have and reports whether anything changed.
func mergeLabels(have *map[string]string, want map[string]string) bool {
	changed := false
	for k, v := range want {
		if (*have)[k] != v {
			if *have == nil {
				*have = map[string]string{}
			}
			(*have)[k] = v
			changed = true
		}
	}
	return changed
}

// ---- per app ----

func (k *Kubernetes) ensureNamespace(ctx context.Context, app string) error {
	ns := naming.Namespace(app)
	lbl := map[string]string{LabelManagedBy: ManagedBy, LabelApp: app}

	nsc := k.client.CoreV1().Namespaces()
	if _, err := apply(ctx, nsc, ns, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: lbl},
	}, func(have *corev1.Namespace) bool { return mergeLabels(&have.Labels, lbl) }); err != nil {
		return err
	}

	// Faz 22: the add-ons' pods, memory and volumes come on top of the
	// deployments' share (addons.go), so a database never eats it.
	hard, err := k.quotaHard(ctx, app)
	if err != nil {
		return fmt.Errorf("resource quota: %w", err)
	}
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns, Labels: lbl},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
	}
	if _, err := apply(ctx, k.client.CoreV1().ResourceQuotas(ns), policyName, quota,
		func(have *corev1.ResourceQuota) bool {
			if equality.Semantic.DeepEqual(have.Spec.Hard, quota.Spec.Hard) {
				return false
			}
			have.Spec.Hard = quota.Spec.Hard
			return true
		}); err != nil {
		return fmt.Errorf("resource quota: %w", err)
	}

	// Defaults for containers that do not set their own (paas always
	// does; this covers anything else run in the namespace).
	limits := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns, Labels: lbl},
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type:           corev1.LimitTypeContainer,
			Default:        corev1.ResourceList{corev1.ResourceCPU: k.q.cpuLim, corev1.ResourceMemory: k.q.memLim},
			DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: k.q.cpuReq, corev1.ResourceMemory: k.q.memReq},
		}}},
	}
	if _, err := apply(ctx, k.client.CoreV1().LimitRanges(ns), policyName, limits,
		func(have *corev1.LimitRange) bool {
			if equality.Semantic.DeepEqual(have.Spec, limits.Spec) {
				return false
			}
			have.Spec = limits.Spec
			return true
		}); err != nil {
		return fmt.Errorf("limit range: %w", err)
	}

	// Deny ingress except from kube-system, where Traefik runs: apps cannot
	// reach each other, only through their public URLs. Add-on servers
	// (Faz 22) are left out here: paas-addons below admits only pods of
	// the namespace, on the add-on ports.
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: ns, Labels: lbl},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: LabelAddon, Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
				}}},
			}},
		},
	}
	// Faz 11: the activator (control plane) proxies woken requests to pods.
	if a := k.cfg.ActivatorNamespace; a != "" {
		policy.Spec.Ingress[0].From = append(policy.Spec.Ingress[0].From, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": a}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "paas"}},
		})
	}
	if err := k.applyPolicy(ctx, policy); err != nil {
		return err
	}
	return k.applyPolicy(ctx, addonPolicy(ns, lbl))
}

// addonPolicyName is the NetworkPolicy of the add-on servers (Faz 22).
const addonPolicyName = "paas-addons"

// addonPolicy admits traffic to add-on servers only from pods of the same
// namespace (the app's deployments and the add-on Jobs) and only on the
// Postgres and Redis ports; nothing outside the namespace, Traefik and the
// control plane included, reaches them.
func addonPolicy(ns string, lbl map[string]string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: addonPolicyName, Namespace: ns, Labels: lbl},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: LabelAddon, Operator: metav1.LabelSelectorOpExists},
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				// A pod selector without a namespace selector: this namespace only.
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: ptr(intstr.FromInt32(store.PostgresPort))},
					{Protocol: &tcp, Port: ptr(intstr.FromInt32(store.RedisPort))},
				},
			}},
		},
	}
}

func (k *Kubernetes) applyPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) error {
	if _, err := apply(ctx, k.client.NetworkingV1().NetworkPolicies(policy.Namespace), policy.Name, policy,
		func(have *networkingv1.NetworkPolicy) bool {
			if equality.Semantic.DeepEqual(have.Spec, policy.Spec) {
				return false
			}
			have.Spec = policy.Spec
			return true
		}); err != nil {
		return fmt.Errorf("network policy %s: %w", policy.Name, err)
	}
	return nil
}

// ---- per deployment ----

func labels(d store.Deployment) map[string]string {
	branch := naming.Slug(d.Branch)
	if len(branch) > 63 {
		branch = strings.TrimRight(branch[:63], "-")
	}
	if branch == "" {
		branch = "branch"
	}
	return map[string]string{
		LabelManagedBy:    ManagedBy,
		LabelApp:          d.AppName,
		LabelDeploymentID: fmt.Sprint(d.ID),
		LabelCommit:       d.CommitSHA,
		LabelBranch:       branch,
	}
}

// selector picks the pods of one deployment. It is immutable once created.
func selector(d store.Deployment) map[string]string {
	return map[string]string{LabelApp: d.AppName, LabelDeploymentID: fmt.Sprint(d.ID)}
}

func secretName(d store.Deployment) string { return d.ObjectName() + "-env" }

// envHash identifies an env snapshot, so a changed snapshot rolls the pods.
func envHash(env map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(h, "%s=%s\x00", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ensureSecret snapshots env into an immutable Secret. Immutable data cannot
// be updated, so if a retried deployment sees different values the old
// snapshot is replaced.
func (k *Kubernetes) ensureSecret(ctx context.Context, d store.Deployment, env map[string]string) (*corev1.Secret, error) {
	ns, name := naming.Namespace(d.AppName), secretName(d)
	data := make(map[string][]byte, len(env))
	for key, v := range env {
		data[key] = []byte(v)
	}
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels(d)},
		Type:       corev1.SecretTypeOpaque,
		Immutable:  ptr(true),
		Data:       data,
	}
	sc := k.client.CoreV1().Secrets(ns)
	for attempt := 1; ; attempt++ {
		have, err := sc.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if sameData(have.Data, data) {
				return have, nil
			}
			err = sc.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &have.UID}})
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
		got, err := sc.Create(ctx, want, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) && attempt < 3 {
			continue
		}
		return got, err
	}
}

func sameData(a, b map[string][]byte) bool {
	return maps.EqualFunc(a, b, bytes.Equal)
}

// adoptSecret makes the Deployment own the Secret, so deleting a deployment
// (Faz 7 cleanup) garbage-collects its env snapshot. Metadata of an
// immutable Secret can still change.
func (k *Kubernetes) adoptSecret(ctx context.Context, s *corev1.Secret, dep *appsv1.Deployment) error {
	ref := ownerRef(dep)
	sc := k.client.CoreV1().Secrets(s.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := sc.Get(ctx, s.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, r := range cur.OwnerReferences {
			if r.UID == ref.UID && r.Name == ref.Name {
				return nil
			}
		}
		cur.OwnerReferences = append(cur.OwnerReferences, ref)
		_, err = sc.Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

func ownerRef(dep *appsv1.Deployment) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "Deployment",
		Name: dep.Name, UID: dep.UID,
		// No BlockOwnerDeletion: it would need update on deployments/finalizers
		// where OwnerReferencesPermissionEnforcement is enabled.
		Controller: ptr(true),
	}
}

// deploymentObject is the web Deployment; exec, when set, replaces the
// image's command (Faz 20: processes.web of a Dockerfile project).
func (k *Kubernetes) deploymentObject(d store.Deployment, image, envHash string, exec []string) *appsv1.Deployment {
	ns, name := naming.Namespace(d.AppName), d.ObjectName()
	lbl := labels(d)
	annot := map[string]string{AnnotBranch: d.Branch, AnnotEnvHash: envHash}

	podSec := &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if k.cfg.RunAsNonRoot {
		// No fixed runAsUser: the image's own USER is used.
		podSec.RunAsNonRoot = ptr(true)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbl, Annotations: annot},
		Spec: appsv1.DeploymentSpec{
			Replicas:             ptr(int32(1)),
			RevisionHistoryLimit: ptr(int32(1)),
			Selector:             &metav1.LabelSelector{MatchLabels: selector(d)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbl, Annotations: annot},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr(false),
					// Many d-* Services live in one namespace; their
					// docker-link style env vars would only be noise.
					EnableServiceLinks: ptr(false),
					SecurityContext:    podSec,
					Containers: []corev1.Container{{
						Name:    "app",
						Image:   image,
						Command: exec,
						Ports:   []corev1.ContainerPort{{Name: "http", ContainerPort: Port, Protocol: corev1.ProtocolTCP}},
						Env:     envVars(d),
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secretName(d)}},
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: k.q.cpuReq, corev1.ResourceMemory: k.q.memReq},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: k.q.cpuLim, corev1.ResourceMemory: k.q.memLim},
						},
						// TCP rather than HTTP: an app without a "/" route is still up.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("http")}},
							PeriodSeconds:    2,
							TimeoutSeconds:   2,
							FailureThreshold: 3,
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
}

func (k *Kubernetes) ensureDeployment(ctx context.Context, d store.Deployment, image, envHash string, exec []string) (*appsv1.Deployment, error) {
	want := k.deploymentObject(d, image, envHash, exec)
	return apply(ctx, k.client.AppsV1().Deployments(want.Namespace), want.Name, want,
		func(have *appsv1.Deployment) bool {
			c := have.Spec.Template.Spec.Containers
			asleep := have.Spec.Replicas != nil && *have.Spec.Replicas == 0
			if len(c) == 1 && c[0].Image == image && have.Spec.Template.Annotations[AnnotEnvHash] == envHash && !asleep {
				return false // a retry of the same deployment: leave the rollout alone
			}
			have.Labels, have.Annotations = want.Labels, want.Annotations
			have.Spec.Replicas = want.Spec.Replicas
			have.Spec.Template = want.Spec.Template // the selector is immutable and unchanged
			return true
		})
}

func (k *Kubernetes) ensureService(ctx context.Context, d store.Deployment, dep *appsv1.Deployment) (*corev1.Service, error) {
	want := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: dep.Name, Namespace: dep.Namespace, Labels: labels(d),
			OwnerReferences: []metav1.OwnerReference{ownerRef(dep)},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector(d),
			Ports: []corev1.ServicePort{{
				Name: "http", Port: 80, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	return apply(ctx, k.client.CoreV1().Services(dep.Namespace), want.Name, want,
		func(have *corev1.Service) bool {
			if equality.Semantic.DeepEqual(have.Spec.Selector, want.Spec.Selector) &&
				equality.Semantic.DeepEqual(have.Spec.Ports, want.Spec.Ports) {
				return false
			}
			// Keep the allocated ClusterIP: only the fields paas owns change.
			have.Spec.Selector, have.Spec.Ports = want.Spec.Selector, want.Spec.Ports
			return true
		})
}
