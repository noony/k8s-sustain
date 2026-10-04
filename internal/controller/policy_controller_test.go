package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
)

func reconcileOnce(t *testing.T, r *PolicyReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func readyCondition(t *testing.T, r *PolicyReconciler, name string) *metav1.Condition {
	t.Helper()
	var got sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("get policy: %v", err)
	}
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			return &got.Status.Conditions[i]
		}
	}
	t.Fatal("expected Ready condition")
	return nil
}

func ongoingPolicy(name string, kinds sustainv1alpha1.UpdateTypes) *sustainv1alpha1.Policy {
	return &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{Types: kinds},
			},
		},
	}
}

func ongoingDeployments(name string) *sustainv1alpha1.Policy {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	return ongoingPolicy(name, sustainv1alpha1.UpdateTypes{Deployment: &ongoing})
}

// establishedDeployment is an opted-in Deployment old enough to clear the age
// gate, running container "app" at 10m CPU.
func establishedDeployment(ns, name, policy string) *appsv1.Deployment {
	d := annotatedDeployment(ns, name, policy)
	d.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
	d.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
	}
	return d
}

// runningPod is a Running pod selected by establishedDeployment(ns, app),
// with container "app" at 10m CPU.
func runningPod(ns, name, app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": app}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func podCPU(t *testing.T, r *PolicyReconciler, ns, name string) string {
	t.Helper()
	var pod corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod.Spec.Containers[0].Resources.Requests.Cpu().String()
}

func TestReconcile_NoInputsFetcher_ReturnsError(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	r := reconcilerForPolicy(t, policy)
	r.Inputs = nil

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err == nil {
		t.Fatal("expected error when no inputs fetcher is configured")
	}
}

func TestReconcile_PolicyNotFound_NoError(t *testing.T) {
	r := reconcilerForPolicy(t, &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "exists"}})

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
	r := reconcilerForPolicy(t, policy)

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
	r := reconcilerForPolicy(t, ongoingDeployments("p"))

	reconcileOnce(t, r, "p")

	ready := readyCondition(t, r, "p")
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
	r := reconcilerForPolicy(t, policy)

	reconcileOnce(t, r, "p")

	var got sustainv1alpha1.Policy
	err := r.Get(context.Background(), types.NamespacedName{Name: "p"}, &got)
	// The fake client garbage-collects the object once finalizers are removed,
	// so a NotFound here is also acceptable.
	if err == nil && slices.Contains(got.Finalizers, "k8s.sustain.io/cleanup") {
		t.Error("expected finalizer to be removed on deletion")
	}
}

// The failing target is a standalone Job, an arbitrary choice: every kind goes
// through the same fetch and a failure propagates identically.
func TestReconcile_PartialFailure_SetsConditionAndRequeues(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := ongoingPolicy("p", sustainv1alpha1.UpdateTypes{Job: &ongoing})
	job := annotatedJob("default", "app", "p")
	inputs := recommendertest.NewStaticInputs().Fail(identityOf("default", "Job", "app"), errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, false, policy, job)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != time.Hour {
		t.Errorf("RequeueAfter = %v, want 1h even on partial failure", res.RequeueAfter)
	}

	ready := readyCondition(t, r, "p")
	if ready.Status == metav1.ConditionTrue {
		t.Error("Ready should NOT be True on partial failure")
	}
	if !strings.Contains(ready.Message, "failed") && !strings.Contains(ready.Reason, "Failure") {
		t.Errorf("expected failure-flavoured Ready condition, got reason=%q msg=%q", ready.Reason, ready.Message)
	}
}

// Job and bare-Pod identities are fetched like every other kind, and counted
// by k8s_sustain_policy_batch_requested_count.
func TestReconcileFetchesJobAndPodIdentities(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	const policyName = "batch-job-pod"
	policy := ongoingPolicy(policyName, sustainv1alpha1.UpdateTypes{Job: &ongoing, Pod: &ongoing})
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
	inputs := recommendertest.NewStaticInputs()
	r := reconcilerWithInputs(t, inputs, false, policy, job, pod)

	reconcileOnce(t, r, policyName)

	for _, id := range []promclient.WorkloadIdentity{identityOf("default", "Job", "nightly"), identityOf("default", "Pod", "dag-task")} {
		if !inputs.Requested(id) {
			t.Errorf("%v was not fetched", id)
		}
	}
	if requested := gaugeValue(t, "k8s_sustain_policy_batch_requested_count", map[string]string{"policy": policyName}); requested != 2 {
		t.Errorf("policy_batch_requested_count = %v, want 2", requested)
	}
}

// A Policy's identities are fetched in one call, not one per workload.
func TestReconcile_FetchesEveryIdentityInOneCall(t *testing.T) {
	const numWorkloads = 10
	extras := []runtime.Object{ongoingDeployments("p")}
	for i := range numWorkloads {
		extras = append(extras, annotatedDeployment("default", fmt.Sprintf("web-%d", i), "p"))
	}
	inputs := recommendertest.NewStaticInputs()
	r := reconcilerWithInputs(t, inputs, true, extras...)

	reconcileOnce(t, r, "p")

	calls := inputs.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d fetch calls, want 1 for the whole Policy", len(calls))
	}
	if len(calls[0]) != numWorkloads {
		t.Errorf("the call requested %d identities, want %d", len(calls[0]), numWorkloads)
	}
	for _, req := range calls[0] {
		if req.Containers != 1 {
			t.Errorf("%v requested with size %d, want its 1 container", req.Identity, req.Containers)
		}
	}
}

// The normal path: the identity's Recommendation is written to its
// WorkloadRecommendation and applied to its running pod.
func TestReconcile_RecommendsPersistsAndApplies(t *testing.T) {
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), true,
		ongoingDeployments("p"), establishedDeployment("default", "web", "p"), runningPod("default", "web-pod", "web"))

	reconcileOnce(t, r, "p")

	wlr := getWLRFor(t, r, "default", "Deployment", "web")
	if got := wlr.Status.Containers["app"].CPURequest; got == nil || got.String() != "100m" {
		t.Errorf("stored CPU recommendation = %v, want 100m", got)
	}
	if cpu := podCPU(t, r, "default", "web-pod"); cpu != "100m" {
		t.Errorf("pod CPU = %s, want the 100m recommendation applied in place", cpu)
	}
	if ready := readyCondition(t, r, "p"); ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s, want True", ready.Status)
	}
}

// One identity whose inputs cannot be read fails alone: it enters retry
// backoff and turns the Policy not-Ready, while its neighbour is still
// recommended and applied. A total outage reaches the controller only this
// way, so without it a dead Prometheus would look like "already right-sized".
func TestReconcile_FetchFailureFailsOnlyThatIdentity(t *testing.T) {
	inputs := usageFor("default", "Deployment", "healthy").
		Fail(identityOf("default", "Deployment", "broken"), errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, true, ongoingDeployments("p"),
		establishedDeployment("default", "broken", "p"), runningPod("default", "broken-pod", "broken"),
		establishedDeployment("default", "healthy", "p"), runningPod("default", "healthy-pod", "healthy"))
	// A delta: the counter is shared by policy label "p" across this package.
	before := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p"))

	reconcileOnce(t, r, "p")

	if after := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p")); after-before != 1 {
		t.Errorf("policy_batch_failures_total delta = %v, want 1", after-before)
	}
	ready := readyCondition(t, r, "p")
	if ready.Status == metav1.ConditionTrue || !strings.Contains(ready.Message, "1 of 2 workloads failed") {
		t.Errorf("Ready = %s %q, want False naming 1 of 2 workloads failed", ready.Status, ready.Message)
	}
	if state := r.retries.getState("Deployment/default/broken"); state == nil || state.attempts < 1 {
		t.Errorf("broken: retry state = %+v, want a recorded attempt", state)
	}
	if cpu := podCPU(t, r, "default", "broken-pod"); cpu != "10m" {
		t.Errorf("broken pod CPU = %s, want it untouched at 10m", cpu)
	}
	if state := r.retries.getState("Deployment/default/healthy"); state != nil {
		t.Errorf("healthy: retry state = %+v, want none", state)
	}
	if cpu := podCPU(t, r, "default", "healthy-pod"); cpu != "100m" {
		t.Errorf("healthy pod CPU = %s, want its 100m recommendation despite its neighbour failing", cpu)
	}
}

// slowInputs delays every fetch, standing in for a batch that takes minutes on
// a real cluster.
type slowInputs struct {
	*recommendertest.StaticInputs
	delay time.Duration
}

func (s slowInputs) FetchInputs(
	ctx context.Context, cfg sustainv1alpha1.ResourcesConfigs, reqs []recommender.InputsRequest,
) map[promclient.WorkloadIdentity]recommender.InputsResult {
	time.Sleep(s.delay)
	return s.StaticInputs.FetchInputs(ctx, cfg, reqs)
}

// An identity whose members are all in retry backoff is not fetched, and the
// decision holds for the whole pass. Backoff is time-based and the fetch can
// take minutes: "expiring" leaves backoff mid-fetch, and asking again at apply
// time would process it with nothing fetched, recording a success that clears
// its retry state.
func TestReconcile_BackedOffIdentityIsNotFetched(t *testing.T) {
	static := recommendertest.NewStaticInputs()
	r := reconcilerWithInputs(t, slowInputs{StaticInputs: static, delay: 500 * time.Millisecond}, true,
		ongoingDeployments("p"), annotatedDeployment("default", "expiring", "p"), annotatedDeployment("default", "healthy", "p"))
	r.retries.mu.Lock()
	r.retries.states["Deployment/default/expiring"] = &retryState{attempts: 1, nextRetry: time.Now().Add(250 * time.Millisecond)}
	r.retries.mu.Unlock()

	reconcileOnce(t, r, "p")

	if static.Requested(identityOf("default", "Deployment", "expiring")) {
		t.Error("an identity whose every member is in backoff was fetched")
	}
	if !static.Requested(identityOf("default", "Deployment", "healthy")) {
		t.Error("the healthy identity was not fetched")
	}
	if state := r.retries.getState("Deployment/default/expiring"); state == nil || state.attempts != 1 {
		t.Errorf("expiring: retry state = %+v, want it untouched: the member skipped at fetch time must be skipped at apply time", state)
	}
}

// The other half of the outage distinction: queries that succeed with no
// samples are not a failure. Treating them as one would retry-storm every
// workload that legitimately has nothing yet.
func TestReconcile_EmptySuccessfulResponse_DeploymentSucceedsWithNoRetry(t *testing.T) {
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs(), true, ongoingDeployments("p"), annotatedDeployment("default", "app", "p"))
	before := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p"))

	reconcileOnce(t, r, "p")

	if after := testutil.ToFloat64(policyBatchFailuresTotal.WithLabelValues("p")); after != before {
		t.Errorf("policy_batch_failures_total moved for an empty-but-successful response: before=%v after=%v", before, after)
	}
	if requested := gaugeValue(t, "k8s_sustain_policy_batch_requested_count", map[string]string{"policy": "p"}); requested != 1 {
		t.Errorf("policy_batch_requested_count = %v, want 1", requested)
	}
	if resolved := gaugeValue(t, "k8s_sustain_policy_batch_resolved_count", map[string]string{"policy": "p"}); resolved != 0 {
		t.Errorf("policy_batch_resolved_count = %v, want 0 (empty-but-successful must not count as resolved)", resolved)
	}
	ready := readyCondition(t, r, "p")
	if ready.Status != metav1.ConditionTrue || ready.Reason != "ReconciliationSucceeded" {
		t.Errorf("Ready = %s/%s, want True/ReconciliationSucceeded", ready.Status, ready.Reason)
	}
	if state := r.retries.getState("Deployment/default/app"); state != nil && state.attempts != 0 {
		t.Errorf("expected no retry state for a successful-but-empty response, got %+v", state)
	}
}
