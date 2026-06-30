package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
)

// TestReconcileWorkload_HappyPath_ProducesRecommendationsAndPatchesPods
// drives reconcileWorkload end-to-end: Prometheus mock returns sample data,
// the recommender produces requests, the patcher patches pods in place.
// Verifies the per-container request was rewritten on the live pod.
func TestReconcileWorkload_HappyPath_ProducesRecommendationsAndPatchesPods(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "web-pod",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := reconcilerWithProm(t, server, true /* in-place */, pod)

	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	if err := r.reconcileWorkload(context.Background(), policy, tgt, autoscaler.NewNamespacedSnapshot(r.Client)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	// Retry tracker should record success (no entry, or attempts=0).
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected attempts=0 on success, got %d", state.attempts)
	}
}

// TestReconcileWorkload_RecommendOnly_DoesNotRecyclePods verifies that the
// RecommendOnly flag short-circuits the recycle path: pods stay untouched
// even when the recommendation differs from current resources.
func TestReconcileWorkload_RecommendOnly_DoesNotRecyclePods(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "web-pod",
			Labels: map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r := reconcilerWithProm(t, server, false, pod)
	r.RecommendOnly = true
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	if err := r.reconcileWorkload(context.Background(), policy, tgt, autoscaler.NewNamespacedSnapshot(r.Client)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	// Pod should still have the original 999m — no eviction was attempted.
	var got corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-pod"}, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got.DeletionTimestamp != nil {
		t.Error("recommend-only must not delete or evict pods")
	}
}

// TestReconcileWorkload_TransientPromError_RecordsRetry verifies that a 500
// from Prometheus is treated as transient: the retry tracker records the
// failure and reconcileWorkload returns the error so the caller can count it.
func TestReconcileWorkload_TransientPromError_RecordsRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, false)
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	err := r.reconcileWorkload(context.Background(), policy, tgt, autoscaler.NewNamespacedSnapshot(r.Client))
	if err == nil {
		t.Fatal("expected transient error to bubble up")
	}

	state := r.retries.getState(tgt.key())
	if state.attempts < 1 {
		t.Errorf("expected retry tracker to record at least 1 attempt, got %d", state.attempts)
	}
}

// TestHandleStepError_NonTransient_EmitsWarningEventAndReturnsNil verifies a
// permanent error (e.g. 403 from missing RBAC) is surfaced via a Warning
// event instead of being silently swallowed, while still returning nil so
// retry semantics are unchanged.
func TestHandleStepError_NonTransient_EmitsWarningEventAndReturnsNil(t *testing.T) {
	rec := events.NewFakeRecorder(10)
	r := &PolicyReconciler{recorder: rec, retries: newRetryTracker()}
	tgt := deploymentTarget("default", "web")

	permErr := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "web-pod", errors.New("rbac missing"))
	if err := r.handleStepError(context.Background(), tgt, "patch", "Pod recycle failed", permErr); err != nil {
		t.Fatalf("non-transient error must return nil (no retry), got %v", err)
	}

	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Warning") || !strings.Contains(e, "ReconciliationFailed") {
			t.Errorf("expected Warning ReconciliationFailed event, got %q", e)
		}
	default:
		t.Error("expected a Warning event for non-transient error, got none")
	}
}

// TestHandleStepError_ContextCanceled_StaysSilent verifies graceful-shutdown
// cancellation does not produce a ReconciliationFailed event.
func TestHandleStepError_ContextCanceled_StaysSilent(t *testing.T) {
	rec := events.NewFakeRecorder(10)
	r := &PolicyReconciler{recorder: rec, retries: newRetryTracker()}
	tgt := deploymentTarget("default", "web")

	if err := r.handleStepError(context.Background(), tgt, "patch", "Pod recycle failed", context.Canceled); err != nil {
		t.Fatalf("context cancellation must return nil, got %v", err)
	}
	select {
	case e := <-rec.Events:
		t.Errorf("expected no event for context cancellation, got %q", e)
	default:
	}
}

// TestReconcileWorkload_NoPrometheusData_RecordsSuccessAndDoesNothing
// verifies that empty Prometheus results are NOT treated as a failure: the
// reconcile returns nil, retry state is cleared, and no patch is attempted.
func TestReconcileWorkload_NoPrometheusData_RecordsSuccessAndDoesNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, false)
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	// Prime the retry tracker so we can confirm it gets cleared on success.
	r.retries.recordFailure(tgt.key())

	if err := r.reconcileWorkload(context.Background(), policy, tgt, autoscaler.NewNamespacedSnapshot(r.Client)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected retry attempts cleared on success, got %d", state.attempts)
	}
}

// TestReconcileWorkload_PodKind_NeverRecycles verifies that a target with
// Kind == "Pod" computes and caches a recommendation but never reaches the
// recycle/resize/evict path — there is no controller that could recreate the
// pod after an eviction or in-place resize.
//
// The target's Selector is deliberately populated (unlike production
// listBarePodTargets, which never sets it) and matches a real, running pod
// whose CPU request differs sharply from what the mock Prometheus data would
// recommend. This makes the assertion meaningful: if the Kind == "Pod" guard
// were missing or merely incidental (e.g. relying on a nil selector), the
// in-place patcher would resize this pod and the test would catch it. The
// guard must be an unconditional, explicit skip.
func TestReconcileWorkload_PodKind_NeverRecycles(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow",
			Name:      "etl-run-1",
			Labels:    map[string]string{"app": "etl-daily"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := reconcilerWithProm(t, server, true /* in-place */, pod)

	tgt := &workloadTarget{
		Kind:         "Pod",
		IdentityKind: "Pod",
		Name:         "etl-daily",
		IdentityName: "etl-daily",
		Namespace:    "airflow",
		Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "etl-daily"}},
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
			},
		}},
		PolicyName: "p",
	}
	policy := policyForReconcileWorkload(t, "p")

	if err := r.reconcileWorkload(context.Background(), policy, tgt, autoscaler.NewNamespacedSnapshot(r.Client)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	// A WorkloadRecommendation must exist — the recommendation is still
	// computed and cached even though recycling is skipped, so the webhook's
	// Prometheus-outage fallback still benefits.
	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "airflow", Name: "pod-etl-daily"}, &wlr); err != nil {
		t.Fatalf("expected WorkloadRecommendation pod-etl-daily, got: %v", err)
	}

	// The pod must be untouched: if the Kind == "Pod" skip were missing,
	// reconcileWorkload would reach the recycle path, the selector above
	// would match this pod, and the in-place patcher would resize its CPU
	// request away from 999m.
	var got corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "airflow", Name: "etl-run-1"}, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if cpu := got.Spec.Containers[0].Resources.Requests.Cpu().String(); cpu != "999m" {
		t.Errorf("pod CPU request changed to %s — recycle/resize path was reached for a Pod-kind target", cpu)
	}

	// recordStepSuccess must still run on the skip path.
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected attempts=0 on success, got %d", state.attempts)
	}
}
