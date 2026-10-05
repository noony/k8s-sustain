package controller

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

// When Ensure fails the identity is still computed, but not applied: its
// recommendation has nowhere to be cached. A persistent cause (missing RBAC, a
// rejecting admission webhook, a quota) must not leave the Policy reporting
// Ready while the cache silently goes stale, and the one failure is counted
// once, as its withheld member's.
func TestReconcileSurfacesDiscoveryFailures(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing}},
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = sustainv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Every WorkloadRecommendation create is rejected, as a missing RBAC rule
	// on the resource would do.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(policy, annotatedDeployment("default", "web", "p")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok {
					return apierrors.NewForbidden(
						schema.GroupResource{Group: "k8s.sustain.io", Resource: "workloadrecommendations"},
						obj.GetName(), errors.New("no RBAC"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := reconcilerOn(c, scheme, recommendertest.NewStaticInputs(), true)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got sustainv1alpha1.Policy
	if err := c.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
		}
	}
	if ready == nil {
		t.Fatal("no Ready condition written")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %s, want False: an identity that could not be registered must not report success", ready.Status)
	}
	if want := "1 of 1 workloads failed; Blocked workload identities: 1"; ready.Message != want {
		t.Errorf("message = %q, want %q: the failed Ensure counted once", ready.Message, want)
	}
	if got := r.ledger.health("Deployment/default/web").blockedOn; got != phaseStore {
		t.Errorf("member blocked on %q, want %q: a member withheld for a failed Ensure is Blocked", got, phaseStore)
	}
}
