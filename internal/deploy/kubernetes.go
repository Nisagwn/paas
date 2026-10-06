// Package deploy runs built images on Kubernetes:
//
//	namespace app-<app> (+ ResourceQuota, LimitRange)
//	  └─ Secret d-<sha7>-env → Deployment d-<sha7> → Service d-<sha7> (:80 → :8080)
//
//	  └─ Ingress d-<sha7>       <sha7>-<app>.<domain>   (owned by the Deployment)
//	  └─ Ingress alias-<label>  <app>.<domain>, <branch>-<app>.<domain>
//	  └─ Ingress domain-<host>  custom domains → production (domains.go)
//
// Every commit gets its own Deployment, Service and Ingress, so deployments
// are immutable and a rollback only moves an alias Ingress (ApplyAliases).
package deploy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/nisagwn/paas/internal/naming"
	"github.com/nisagwn/paas/internal/store"
	"github.com/nisagwn/paas/internal/worker"
)

// Port is the platform contract (build.Port): every app listens on $PORT.
const Port = 8080

// Labels and annotations set on every object paas creates.
const (
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	LabelApp          = "app"
	LabelDeploymentID = "paas/deployment-id"
	LabelCommit       = "paas/commit-sha"
	LabelBranch       = "paas/branch"
	AnnotBranch       = "paas/branch"   // the unslugged branch name
	AnnotEnvHash      = "paas/env-hash" // rolls pods when the env snapshot changes
	ManagedBy         = "paas"
)

// EnvSource provides the environment variables a deployment runs with
// (store.Store): those of its environment, production or preview (Faz 17).
type EnvSource interface {
	DeploymentEnv(ctx context.Context, d store.Deployment) (map[string]string, error)
}

// Config holds the tunables. Quantities use Kubernetes syntax ("250m", "256Mi").
type Config struct {
	// Domain is the platform domain every hostname lives under.
	Domain string
	// IngressClass selects the ingress controller ("traefik" on k3s).
	// Empty uses the cluster's default class.
	IngressClass string
	// TLS serves routes on the websecure entry point with the default
	// (wildcard) certificate; false uses plain HTTP, e.g. on a laptop cluster.
	TLS bool
	// CustomDomainIssuer is the cert-manager ClusterIssuer for custom
	// domains (Faz 12). Empty means DefaultCustomDomainIssuer.
	CustomDomainIssuer string
	// CertIssuer, when set, gives every route its own certificate from this
	// cert-manager ClusterIssuer instead of the wildcard default, e.g. on a
	// DuckDNS name where a wildcard (DNS-01) is not possible.
	CertIssuer string
	// Per container.
	CPURequest, CPULimit       string
	MemoryRequest, MemoryLimit string
	// Per app namespace (ResourceQuota): requests.cpu, limits.memory, pods.
	QuotaCPU, QuotaMemory string
	QuotaPods             int
	// RunAsNonRoot makes the kubelet refuse images that run as root. It also
	// refuses images whose USER is a name instead of a numeric UID.
	RunAsNonRoot bool
	// RolloutTimeout bounds the wait for the Deployment to become available.
	RolloutTimeout time.Duration
	// PollInterval between rollout checks. Zero means 2s.
	PollInterval time.Duration

	// Faz 11 (sleep.go, metrics.go). ActivatorIP:ActivatorPort is where the
	// ingress controller reaches the activator; empty disables Sleep.
	ActivatorIP   string
	ActivatorPort int32
	// ActivatorNamespace, if set, lets pods of that namespace labelled
	// app.kubernetes.io/name=paas (the control plane) reach app pods, so the
	// activator can proxy a woken request straight to the pod.
	ActivatorNamespace string
	// Where Traefik's metrics are read. Defaults: kube-system,
	// app.kubernetes.io/name=traefik, port 9100.
	TraefikNamespace, TraefikSelector, TraefikMetricsPort string
}

// DefaultConfig is sized for a 4 GiB node shared by many deployments.
func DefaultConfig() Config {
	return Config{
		IngressClass: "traefik", TLS: true,
		CPURequest: "25m", CPULimit: "500m",
		MemoryRequest: "64Mi", MemoryLimit: "256Mi",
		QuotaCPU: "1", QuotaMemory: "4Gi", QuotaPods: 20,
		RunAsNonRoot:   true,
		RolloutTimeout: 3 * time.Minute,
	}
}

type quantities struct {
	cpuReq, cpuLim, memReq, memLim, quotaCPU, quotaMem resource.Quantity
}

// Kubernetes implements worker.Deployer.
type Kubernetes struct {
	client kubernetes.Interface
	env    EnvSource
	cfg    Config
	q      quantities

	// Certificates reads cert-manager Certificates of custom domains
	// (optional; see DomainCertificate).
	Certificates dynamic.Interface

	// Processes provides process sets (Faz 20, processes.go). New takes it
	// from env when env implements it; nil deploys the web process only.
	Processes ProcessSource
}

var _ worker.Deployer = (*Kubernetes)(nil)

// New validates cfg and returns a deployer. env may be nil (no variables).
func New(client kubernetes.Interface, env EnvSource, cfg Config) (*Kubernetes, error) {
	k := &Kubernetes{client: client, env: env, cfg: cfg}
	k.Processes, _ = env.(ProcessSource)
	for _, f := range []struct {
		name, val string
		dst       *resource.Quantity
	}{
		{"cpu request", cfg.CPURequest, &k.q.cpuReq},
		{"cpu limit", cfg.CPULimit, &k.q.cpuLim},
		{"memory request", cfg.MemoryRequest, &k.q.memReq},
		{"memory limit", cfg.MemoryLimit, &k.q.memLim},
		{"quota cpu", cfg.QuotaCPU, &k.q.quotaCPU},
		{"quota memory", cfg.QuotaMemory, &k.q.quotaMem},
	} {
		q, err := resource.ParseQuantity(f.val)
		if err != nil {
			return nil, fmt.Errorf("deploy: %s %q: %w", f.name, f.val, err)
		}
		*f.dst = q
	}
	if k.q.cpuReq.Cmp(k.q.cpuLim) > 0 || k.q.memReq.Cmp(k.q.memLim) > 0 {
		return nil, errors.New("deploy: requests must not exceed limits")
	}
	if cfg.Domain == "" {
		return nil, errors.New("deploy: domain is required")
	}
	if cfg.QuotaPods < 1 {
		return nil, errors.New("deploy: quota pods must be positive")
	}
	if k.cfg.RolloutTimeout <= 0 {
		k.cfg.RolloutTimeout = 3 * time.Minute
	}
	if k.cfg.PollInterval <= 0 {
		k.cfg.PollInterval = 2 * time.Second
	}
	k.cfg.TraefikNamespace = cmp.Or(k.cfg.TraefikNamespace, "kube-system")
	k.cfg.TraefikSelector = cmp.Or(k.cfg.TraefikSelector, "app.kubernetes.io/name=traefik")
	k.cfg.TraefikMetricsPort = cmp.Or(k.cfg.TraefikMetricsPort, "9100")
	return k, nil
}

// NewClient uses the in-cluster service account when running in a pod,
// otherwise kubeconfig (explicit path, else $KUBECONFIG / ~/.kube/config).
func NewClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil || kubeconfig != "" {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = kubeconfig
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("deploy: kubernetes config: %w", err)
		}
	}
	cfg.Timeout = 30 * time.Second // per request; rollouts are bounded separately
	return cfg, nil
}

// Check verifies that the API server is reachable, so a misconfiguration
// shows up at startup instead of on the first push.
func (k *Kubernetes) Check() error {
	if _, err := k.client.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("deploy: kubernetes API unreachable: %w", err)
	}
	return nil
}

func (k *Kubernetes) Deploy(ctx context.Context, d store.Deployment, image string, log worker.Logger) error {
	ns, name := naming.Namespace(d.AppName), d.ObjectName()
	log("==> deploying %s", image)

	if err := k.ensureNamespace(ctx, d.AppName); err != nil {
		return fmt.Errorf("namespace %s: %w", ns, err)
	}
	log("    namespace %s (quota: %s cpu requests, %s memory, %d pods)",
		ns, k.cfg.QuotaCPU, k.cfg.QuotaMemory, k.cfg.QuotaPods)

	env := map[string]string{}
	if k.env != nil {
		var err error
		if env, err = k.env.DeploymentEnv(ctx, d); err != nil {
			return fmt.Errorf("load env: %w", err)
		}
	}
	secret, err := k.ensureSecret(ctx, d, env)
	if err != nil {
		return fmt.Errorf("secret %s/%s: %w", ns, name+"-env", err)
	}
	log("    secret %s: %d variable(s) of the %s environment", secret.Name, len(env), d.Target)

	// Faz 20: workers and crons (processes.go).
	procs, err := k.processSet(ctx, d)
	if err != nil {
		return fmt.Errorf("process set: %w", err)
	}
	if !procs.HasWeb() {
		return k.deployWithoutWeb(ctx, d, image, envHash(env), secret, procs, log)
	}

	dep, err := k.ensureDeployment(ctx, d, image, envHash(env), procs.WebExec)
	if err != nil {
		return fmt.Errorf("deployment %s/%s: %w", ns, name, err)
	}
	if _, err := k.ensureService(ctx, d, dep); err != nil {
		return fmt.Errorf("service %s/%s: %w", ns, name, err)
	}
	// A redeploy of a sleeping deployment (crash recovery) runs it again.
	if err := k.deleteActivatorSlice(ctx, ns, name); err != nil {
		return err
	}
	// The Secret is created before the Deployment (pods need it at start),
	// so it is attached to its owner afterwards for garbage collection.
	if err := k.adoptSecret(ctx, secret, dep); err != nil {
		return fmt.Errorf("secret %s/%s: %w", ns, secret.Name, err)
	}
	log("    deployment + service %s/%s (ClusterIP :80 → :%d)", ns, name, Port)
	host, err := k.ensureDeploymentIngress(ctx, d, dep)
	if err != nil {
		return fmt.Errorf("ingress %s/%s: %w", ns, name, err)
	}
	log("    ingress %s/%s → %s", ns, name, k.url(host))
	if err := k.startProcesses(ctx, d, image, envHash(env), procs, ownerRef(dep), log); err != nil {
		return err
	}

	if err := k.waitReady(ctx, ns, name, log); err != nil {
		return err
	}
	return k.waitWorkers(ctx, d, procs, log)
}

// envVars are set by the platform and take precedence over envFrom.
func envVars(d store.Deployment) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PORT", Value: fmt.Sprint(Port)},
		{Name: "PAAS_APP", Value: d.AppName},
		{Name: "PAAS_COMMIT_SHA", Value: d.CommitSHA},
		{Name: "PAAS_BRANCH", Value: d.Branch},
		// Faz 20: which process of the deployment this is (web, a worker, a cron).
		{Name: "PAAS_PROCESS", Value: "web"},
	}
}

func (k *Kubernetes) url(host string) string {
	if k.cfg.TLS {
		return "https://" + host
	}
	return "http://" + host
}
