package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// TestReconcile_NilPrometheusClient_ReturnsError verifies the early-exit path
// when the controller has no Prometheus client wired up.
func TestReconcile_NilPrometheusClient_ReturnsError(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()
	r.PrometheusClient = nil

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err == nil {
		t.Fatal("expected error when PrometheusClient is nil")
	}
}

// TestReconcile_PolicyNotFound_NoError verifies that a missing policy is
// silently ignored (controller-runtime IgnoreNotFound semantics).
func TestReconcile_PolicyNotFound_NoError(t *testing.T) {
	r, server := reconcilerForPolicy(t, &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "exists"}})
	defer server.Close()

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing"}})
	if err != nil {
		t.Fatalf("expected no error for missing policy, got %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected zero RequeueAfter for missing policy, got %v", res.RequeueAfter)
	}
}

// TestReconcile_AddsFinalizerAndRequeues verifies the first reconcile of a
// policy adds the cleanup finalizer and returns a RequeueAfter equal to the
// configured interval.
func TestReconcile_AddsFinalizerAndRequeues(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != time.Hour {
		t.Errorf("RequeueAfter = %v, want 1h", res.RequeueAfter)
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if !slices.Contains(got.Finalizers, "k8s.sustain.io/cleanup") {
		t.Errorf("expected finalizer to be added, got %v", got.Finalizers)
	}
}

// TestReconcile_EmptyTargets_SetsReadyCondition verifies the success path:
// no workloads matched, finalizer added, Ready condition stamped.
func TestReconcile_EmptyTargets_SetsReadyCondition(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing},
				},
			},
		},
	}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if len(got.Status.Conditions) == 0 {
		t.Fatal("expected at least one status condition")
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
			break
		}
	}
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status = %v, want True", ready.Status)
	}
	if ready.Reason != "ReconciliationSucceeded" {
		t.Errorf("Ready.Reason = %q", ready.Reason)
	}
}

// TestReconcile_DeletedPolicy_RemovesFinalizer verifies the deletion path:
// when DeletionTimestamp is set, the cleanup finalizer is removed so garbage
// collection can complete.
func TestReconcile_DeletedPolicy_RemovesFinalizer(t *testing.T) {
	now := metav1.Now()
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "p",
			Finalizers:        []string{"k8s.sustain.io/cleanup"},
			DeletionTimestamp: &now,
		},
	}
	r, server := reconcilerForPolicy(t, policy)
	defer server.Close()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got sustainv1alpha1.Policy
	err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got)
	// The fake client garbage-collects the object once finalizers are removed,
	// so a NotFound here is also acceptable.
	if err == nil && slices.Contains(got.Finalizers, "k8s.sustain.io/cleanup") {
		t.Error("expected finalizer to be removed on deletion")
	}
}

// TestReconcile_PartialFailure_SetsConditionAndRequeues verifies that when a
// reconcileWorkload fails (e.g. Prometheus error), the Reconcile loop reports
// PartialFailure on the policy status and still requeues.
func TestReconcile_PartialFailure_SetsConditionAndRequeues(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing},
				},
			},
		},
	}
	dep := annotatedDeployment("default", "app", "p")

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = rolloutsv1alpha1.AddToScheme(scheme)
	_ = sustainv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Prometheus mock that always returns 500 — drives reconcileWorkload to
	// the transient-error retry path (which still surfaces an aggregate
	// PartialFailure to the caller via failCount).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}).
		WithRuntimeObjects(policy, dep).
		Build()

	r := &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		PrometheusClient:         pc,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 1,
		recorder:                 events.NewFakeRecorder(100),
		patcher:                  workload.New(c, false),
		retries:                  newRetryTracker(),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != time.Hour {
		t.Errorf("RequeueAfter = %v, want 1h even on partial failure", res.RequeueAfter)
	}

	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
			break
		}
	}
	if ready == nil {
		t.Fatal("expected Ready condition")
	}
	if ready.Status == metav1.ConditionTrue {
		t.Error("Ready should NOT be True on partial failure")
	}
	if !strings.Contains(ready.Message, "failed") && !strings.Contains(ready.Reason, "Failure") {
		t.Errorf("expected failure-flavoured Ready condition, got reason=%q msg=%q", ready.Reason, ready.Message)
	}
}
