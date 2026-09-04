package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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

// The failing target is a standalone Job, an arbitrary choice: every kind goes
// through the same FetchWorkloadInputsBatch prefetch and an outage propagates
// identically via BatchStats.Failures (see
// TestReconcile_TotalOutage_DeploymentGetsPartialFailureAndRetry).
func TestReconcile_PartialFailure_SetsConditionAndRequeues(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Job: &ongoing},
				},
			},
		},
	}
	job := annotatedJob("default", "app", "p")

	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
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
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithRuntimeObjects(policy, job).
		Build()

	r := &PolicyReconciler{
		Client:                   c,
		Scheme:                   scheme,
		PrometheusClient:         pc,
		ReconcileInterval:        time.Hour,
		WorkloadConcurrencyLimit: 1,
		QueryShardMaxSamples:     10_000_000,
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

// Pins that Job and bare-Pod identities become shard candidates. It drives a
// real Reconcile() with them as the ONLY targets (no Deployment, so nothing
// else can inflate the count) and reads the candidate count the loop actually
// built, via k8s_sustain_policy_batch_requested_count: 0 means both were
// withheld by the old kind exclusion, 2 means both were batched.
//
// TestJobAndPodIdentitiesBecomeShardCandidates in computation_test.go cannot
// substitute -- it re-implements only the empty-containers half of the filter
// inline, so it passes whether or not the kind exclusion exists.
func TestReconcileBatchesJobAndPodIdentities(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	const policyName = "batch-job-pod"
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: policyName, Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{
					Types: sustainv1alpha1.UpdateTypes{Job: &ongoing, Pod: &ongoing},
				},
			},
		},
	}
	job := annotatedJob("default", "nightly", policyName)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "dag-task-run-1",
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    policyName,
				sustainv1alpha1.OwnerNameAnnotation: "dag-task",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	r, server := reconcilerForPolicy(t, policy, job, pod)
	defer server.Close()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	requested := gaugeValue(t, "k8s_sustain_policy_batch_requested_count", map[string]string{"policy": policyName})
	if requested != 2 {
		t.Errorf("policy_batch_requested_count = %v, want 2: Job and Pod identities must be shard candidates now that HistoryStart is gone", requested)
	}
}

// Reconciling many workloads under one Policy must issue a roughly constant
// number of Prometheus queries (one shard per resource, shared across a
// namespace/owner_kind group), not a set PER WORKLOAD. No other test in this
// file counts requests, so without this one the controller could silently fall
// back to per-workload queries and the suite would stay green.
//
// All workloads are Deployments for setup convenience; every kind goes through
// the same batch path.
func TestReconcile_BatchesPrometheusQueriesAcrossWorkloads(t *testing.T) {
	const numWorkloads = 10

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

	extras := []runtime.Object{policy}
	for i := range numWorkloads {
		extras = append(extras, annotatedDeployment("default", fmt.Sprintf("web-%d", i), "p"))
	}

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true, extras...)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := requestCount.Load()
	// 10 Deployments sharing one (namespace, owner_kind) pair fit comfortably
	// under QueryShardMaxSamples and collapse into exactly one CPU shard, one
	// memory shard, and one OOM shard (OOM reuses the CPU shard partition,
	// per FetchWorkloadInputsBatch's doc comment) = 3 requests total,
	// independent of numWorkloads. Assert well under the pre-batching
	// per-workload bound (3 queries/workload x 10 = 30) rather than pinning
	// the exact count, so this doesn't become brittle to an unrelated shard
	// implementation change while still failing loudly on a regression to
	// per-workload fan-out.
	if got >= numWorkloads {
		t.Errorf("expected a batched (roughly constant) query count, got %d requests for %d workloads -- looks like per-workload fan-out, not batching", got, numWorkloads)
	}
	t.Logf("observed %d Prometheus requests for %d workloads", got, numWorkloads)
}

// recommender.FetchWorkloadInputsBatch never returns an error by design -- one
// sick shard must not deny healthy shard-mates a recommendation -- so a genuine
// per-identity failure reaches PolicyReconciler only through
// BatchStats.Failures. Without that wiring a total outage flows through as
// empty-but-present inputs and records success, making a dead Prometheus
// indistinguishable from "everything is already correctly sized".
//
// Driven through the real Reconcile path so it covers the batch wiring, not just
// buildRecommendations' own fetchErr check.
func TestReconcile_TotalOutage_DeploymentGetsPartialFailureAndRetry(t *testing.T) {
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true, policy, dep)

	// A delta, not an absolute value: k8s_sustain_policy_batch_failures_total is
	// a Counter shared by policy label "p" across this file and persists across
	// `go test -count>1` re-runs in the same process.
	before := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p"))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The failure half of the coverage/failure independence pin;
	// TestReconcile_EmptySuccessfulResponse_DeploymentSucceedsWithNoRetry below
	// is the other half.
	if after := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p")); after-before != 1 {
		t.Errorf("policy_batch_failures_total delta = %v, want 1 (before=%v after=%v)", after-before, before, after)
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
		t.Error("Ready should NOT be True during a total Prometheus outage")
	}
	if !strings.Contains(ready.Message, "failed") && !strings.Contains(ready.Reason, "Failure") {
		t.Errorf("expected failure-flavoured Ready condition, got reason=%q msg=%q", ready.Reason, ready.Message)
	}

	state := r.retries.getState("Deployment/default/app")
	if state == nil || state.attempts < 1 {
		t.Errorf("expected retry state recorded for the deployment during a total outage, got %+v", state)
	}
}

// shouldSkip is purely time-based and the batch prefetch sits between the
// candidate loop and the processing loop, taking seconds to minutes on a real
// cluster. A workload whose backoff lapses inside that gap answered "skip" to
// the first question and "process" to the second: left out of the batch, then
// issued the very per-workload query the backoff existed to suppress. The
// decision must therefore be taken once.
//
// "expiring" is in backoff that lapses mid-prefetch; "healthy" exists only to
// make the prefetch take measurable time (with no candidates BuildShards issues
// no queries at all and the gap would be zero).
func TestReconcile_BackoffExpiringDuringPrefetch_StaysSkipped(t *testing.T) {
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
	expiring := annotatedDeployment("default", "expiring", "p")
	healthy := annotatedDeployment("default", "healthy", "p")

	// Records whether any exact-match (per-workload) query named "expiring".
	// Shard queries use owner_name=~"a|b"; per-workload ones use owner_name="a".
	var mu sync.Mutex
	perWorkloadForExpiring := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		if strings.Contains(q, `owner_name="expiring"`) {
			mu.Lock()
			perWorkloadForExpiring++
			mu.Unlock()
		}
		// Makes the prefetch straddle the backoff expiry below.
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true, policy, expiring, healthy)

	// Backoff lapses well before the prefetch above can finish, so the two
	// shouldSkip calls would disagree if both were still made.
	r.retries.mu.Lock()
	r.retries.states["Deployment/default/expiring"] = &retryState{
		attempts:  1,
		nextRetry: time.Now().Add(20 * time.Millisecond),
	}
	r.retries.mu.Unlock()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mu.Lock()
	got := perWorkloadForExpiring
	mu.Unlock()
	if got != 0 {
		t.Errorf("a workload left out of the batch for being in backoff was then processed anyway, "+
			"issuing %d per-workload Prometheus queries: the backoff decision must be taken once, "+
			"before the prefetch, and reused", got)
	}
}

// The other half of the distinction TestReconcile_TotalOutage_* pins: queries
// that succeed but return no samples must NOT be a failure. Without it, a change
// could "fix" the outage case by treating every empty BatchInputs entry as a
// failure and retry-storm every workload that legitimately has nothing yet.
func TestReconcile_EmptySuccessfulResponse_DeploymentSucceedsWithNoRetry(t *testing.T) {
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true, policy, dep)

	// See TestReconcile_TotalOutage_DeploymentGetsPartialFailureAndRetry for
	// why this is a before/after delta rather than an absolute value.
	before := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p"))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The coverage half of the independence pin: one workload requested, none
	// resolved, and critically the failures counter must NOT have moved.
	if after := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p")); after != before {
		t.Errorf("policy_batch_failures_total moved for an empty-but-successful response: before=%v after=%v", before, after)
	}
	if requested := gaugeValue(t, "k8s_sustain_policy_batch_requested_count", map[string]string{"policy": "p"}); requested != 1 {
		t.Errorf("policy_batch_requested_count = %v, want 1", requested)
	}
	if resolved := gaugeValue(t, "k8s_sustain_policy_batch_resolved_count", map[string]string{"policy": "p"}); resolved != 0 {
		t.Errorf("policy_batch_resolved_count = %v, want 0 (empty-but-successful must not count as resolved)", resolved)
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
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True for a successful-but-empty response, got %+v", ready)
	}
	if ready.Reason != "ReconciliationSucceeded" {
		t.Errorf("Ready.Reason = %q, want ReconciliationSucceeded", ready.Reason)
	}

	if state := r.retries.getState("Deployment/default/app"); state != nil && state.attempts != 0 {
		t.Errorf("expected no retry state for a successful-but-empty response, got %+v", state)
	}
}
