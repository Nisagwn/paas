package deploy

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nisagwn/paas/internal/naming"
)

// DomainCertificateExpiry returns status.notAfter of a custom domain's
// cert-manager Certificate (Faz 26: expiring certificate notifications).
// ok is false without TLS, without a dynamic client, without a
// certificate or before cert-manager recorded the date.
func (k *Kubernetes) DomainCertificateExpiry(ctx context.Context, app, host string) (time.Time, bool, error) {
	if !k.cfg.TLS || k.Certificates == nil {
		return time.Time{}, false, nil
	}
	cert, err := k.Certificates.Resource(certificateGVR).Namespace(naming.Namespace(app)).
		Get(ctx, domainSecretName(host), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	v, found, _ := unstructured.NestedString(cert.Object, "status", "notAfter")
	if !found || v == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false, nil
	}
	return t, true, nil
}
