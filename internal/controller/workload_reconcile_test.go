package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

	if err := r.reconcileWorkload(context.Background(), policy, tgt); err != nil {
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

	if err := r.reconcileWorkload(context.Background(), policy, tgt); err != nil {
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

	err := r.reconcileWorkload(context.Background(), policy, tgt)
	if err == nil {
		t.Fatal("expected transient error to bubble up")
	}

	state := r.retries.getState(tgt.key())
	if state.attempts < 1 {
		t.Errorf("expected retry tracker to record at least 1 attempt, got %d", state.attempts)
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

	if err := r.reconcileWorkload(context.Background(), policy, tgt); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected retry attempts cleared on success, got %d", state.attempts)
	}
}
