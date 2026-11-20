package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dbv1 "github.com/lordofduckkk/postgres-reliability-operator/api/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := dbv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReconcileStatefulSetIdempotent proves that repeated reconciles do not
// churn the StatefulSet, and that spec drift (replicas, image) is corrected.
func TestReconcileStatefulSetIdempotent(t *testing.T) {
	scheme := testScheme(t)
	pg := testCluster()
	pg.Spec.Replicas = 3

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pg).Build()
	r := &PostgresClusterReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()
	key := types.NamespacedName{Namespace: pg.Namespace, Name: pg.Name}

	if err := r.reconcileStatefulSet(ctx, pg); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, key, sts); err != nil {
		t.Fatalf("statefulset not created: %v", err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas")
	}
	rv1 := sts.ResourceVersion

	// Second reconcile must be a no-op.
	if err := r.reconcileStatefulSet(ctx, pg); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := c.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	if sts.ResourceVersion != rv1 {
		t.Fatalf("reconcile was not idempotent: resourceVersion changed %s -> %s", rv1, sts.ResourceVersion)
	}

	// Scale down: drift must be corrected.
	pg.Spec.Replicas = 2
	if err := r.reconcileStatefulSet(ctx, pg); err != nil {
		t.Fatalf("scale reconcile: %v", err)
	}
	if err := c.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	if *sts.Spec.Replicas != 2 {
		t.Fatalf("expected 2 replicas after drift, got %d", *sts.Spec.Replicas)
	}
}

func TestEqualEnv(t *testing.T) {
	a := []corev1.EnvVar{
		{Name: "A", Value: "1"},
		{Name: "B", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k",
			},
		}},
	}
	b := []corev1.EnvVar{
		{Name: "A", Value: "1"},
		{Name: "B", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k",
			},
		}},
	}
	if !equalEnv(a, b) {
		t.Fatal("identical env lists must be equal")
	}
	b[0].Value = "2"
	if equalEnv(a, b) {
		t.Fatal("different value must not be equal")
	}
	if equalEnv(a, a[:1]) {
		t.Fatal("different length must not be equal")
	}
}
