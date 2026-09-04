package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func nsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("scheme core: %v", err)
	}
	return s
}

func TestNSAnnotations_MemoisesPerNamespace(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "team-a",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	var gets int
	c := fake.NewClientBuilder().WithScheme(nsScheme(t)).WithObjects(ns).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				gets++
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	n := newNSAnnotations(c)
	for range 3 {
		got, err := n.get(context.Background(), "team-a")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got[sustainv1alpha1.PolicyAnnotation] != "p" {
			t.Fatalf("annotations = %v, want policy=p", got)
		}
	}
	if gets != 1 {
		t.Errorf("expected 1 apiserver Get for 3 lookups of the same namespace, got %d — "+
			"a namespace with N workloads must not cost N reads", gets)
	}
}

// A namespace that does not exist is not an error: it can be deleted between
// the workload List and this lookup. It resolves as "no annotations" and the
// workload simply falls through to not-managed.
func TestNSAnnotations_NotFoundIsEmptyNotError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(nsScheme(t)).Build()
	got, err := newNSAnnotations(c).get(context.Background(), "gone")
	if err != nil {
		t.Fatalf("a missing namespace must not be an error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no annotations for a missing namespace, got %v", got)
	}
}

func TestNSAnnotations_ForPodsCoversEveryNamespace(t *testing.T) {
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "a", Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "pa"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "b", Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "pb"}}},
	}
	c := fake.NewClientBuilder().WithScheme(nsScheme(t)).WithObjects(objs...).Build()
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "2"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "3"}},
	}
	got, err := newNSAnnotations(c).forPods(context.Background(), pods)
	if err != nil {
		t.Fatalf("forPods: %v", err)
	}
	if got["a"][sustainv1alpha1.PolicyAnnotation] != "pa" || got["b"][sustainv1alpha1.PolicyAnnotation] != "pb" {
		t.Errorf("forPods = %v, want a→pa and b→pb", got)
	}
}

// Counterpart to TestNSAnnotations_NotFoundIsEmptyNotError: a broken read (RBAC
// gap, apiserver outage) must surface as an error rather than degrade to "no
// annotations", or collectTargets would silently un-manage every
// namespace-opted-in workload for as long as the read keeps failing.
func TestNSAnnotations_NonNotFoundErrorPropagates(t *testing.T) {
	boom := apierrors.NewInternalError(errors.New("boom"))
	c := fake.NewClientBuilder().WithScheme(nsScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	_, err := newNSAnnotations(c).get(context.Background(), "team-a")
	if err == nil {
		t.Fatal("expected a non-NotFound namespace read failure to propagate as an error")
	}
}
