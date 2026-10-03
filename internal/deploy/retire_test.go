package deploy

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"
)

func TestRetireDeletesDeploymentAndSecret(t *testing.T) {
	k, cs := newDeployer(t, envMap{7: {"A": "1"}}, 5*time.Second)
	becomeAvailable(t, cs)
	var l logs
	ctx := context.Background()
	if err := k.Deploy(ctx, dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	cs.ClearActions()

	if err := k.Retire(ctx, dep); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deployment still there: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("secret still there: %v", err)
	}
	var sawDelete bool
	for _, a := range cs.Actions() {
		if del, ok := a.(k8stesting.DeleteActionImpl); ok && del.GetResource().Resource == "deployments" {
			sawDelete = true
			p := del.GetDeleteOptions().PropagationPolicy
			if p == nil || *p != metav1.DeletePropagationBackground {
				t.Errorf("propagation policy = %v, want Background (owned Service/Ingress/Secret go too)", p)
			}
		}
	}
	if !sawDelete {
		t.Fatal("no delete of the Deployment")
	}

	// Idempotent: everything is gone already.
	if err := k.Retire(ctx, dep); err != nil {
		t.Fatalf("second retire: %v", err)
	}
}

func TestRetireMissingNamespaceAndForeignObjects(t *testing.T) {
	k, cs := newDeployer(t, nil, 5*time.Second)
	ctx := context.Background()
	// A build that failed before deploying created nothing.
	if err := k.Retire(ctx, dep); err != nil {
		t.Fatalf("retire with nothing deployed: %v", err)
	}

	becomeAvailable(t, cs)
	var l logs
	if err := k.Deploy(ctx, dep, image, l.log); err != nil {
		t.Fatalf("deploy: %v\n%s", err, &l)
	}
	// Another deployment whose commit shares the 7-char prefix must not
	// delete these objects.
	other := dep
	other.ID = 99
	if err := k.Retire(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("foreign retire deleted the deployment: %v", err)
	}
}
