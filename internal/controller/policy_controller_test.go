package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
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

// policyManaging is a Policy managing kinds, holding the cleanup finalizer a
// first Reconcile would add.
func policyManaging(name string, kinds sustainv1alpha1.UpdateTypes) *sustainv1alpha1.Policy {
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
	return policyManaging(name, sustainv1alpha1.UpdateTypes{Deployment: &ongoing})
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

// runningPod is a Running pod of establishedDeployment(ns, app), with
// container "app" at 10m CPU.
func runningPod(ns, name, app string) *corev1.Pod {
	return runningPodAt(ns, name, app, "10m")
}

func runningPodAt(ns, name, app, cpu string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: map[string]string{"app": app},
			OwnerReferences: controllerRef("Deployment", app),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
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
// through the same fetch and a failure propagates identically. The next pass
// is due when the retry is, not a whole reconcile interval later.
func TestReconcile_PartialFailure_SetsConditionAndRequeues(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	policy := policyManaging("p", sustainv1alpha1.UpdateTypes{Job: &ongoing})
	job := annotatedJob("default", "app", "p")
	inputs := recommendertest.NewStaticInputs().Fail(identityOf("default", "Job", "app"), errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, false, policy, job)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != baseRetryDelay {
		t.Errorf("RequeueAfter = %v, want %v: the failed member's first retry", res.RequeueAfter, baseRetryDelay)
	}

	ready := readyCondition(t, r, "p")
	if ready.Status == metav1.ConditionTrue {
		t.Error("Ready should NOT be True on partial failure")
	}
	if !strings.Contains(ready.Message, "failed") && !strings.Contains(ready.Reason, "Failure") {
		t.Errorf("expected failure-flavoured Ready condition, got reason=%q msg=%q", ready.Reason, ready.Message)
	}
}

// Every count of workloads is a count of identities: an owner-name group is
// one workload in the Ready message, whether its two members fail or succeed.
func TestReconcile_ReadyCountsIdentities(t *testing.T) {
	const ns = "counted"
	api := identityOf(ns, "Deployment", "api")
	for _, tc := range []struct {
		name   string
		inputs *recommendertest.StaticInputs
		want   string
	}{
		{"failing", recommendertest.NewStaticInputs().Fail(api, errors.New("prometheus down")), "1 of 1 workloads failed; Blocked workload identities: 1"},
		{"succeeding", usageFor(ns, "Deployment", "api"), "All 1 workloads have been processed."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reconcilerWithInputs(t, tc.inputs, true, ongoingDeployments("p"),
				groupedDeployment(ns, "api-blue", "api", "p"), groupedDeployment(ns, "api-green", "api", "p"))

			reconcileOnce(t, r, "p")

			if got := readyCondition(t, r, "p").Message; got != tc.want {
				t.Errorf("Ready message = %q, want %q", got, tc.want)
			}
		})
	}
}

// Job and bare-Pod identities are fetched like every other kind, and counted
// by k8s_sustain_policy_batch_requested_count.
func TestReconcileFetchesJobAndPodIdentities(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	const policyName = "batch-job-pod"
	policy := policyManaging(policyName, sustainv1alpha1.UpdateTypes{Job: &ongoing, Pod: &ongoing})
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

// The normal path: the identity's Recommendation and its trace are written to
// its WorkloadRecommendation and applied to its running pod, which is then
// counted and no longer stale.
func TestReconcile_RecommendsPersistsAndApplies(t *testing.T) {
	const ns = "applied"
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
		ongoingDeployments("p"), establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web"))

	reconcileOnce(t, r, "p")

	wlr := getWLRFor(t, r, ns, "Deployment", "web")
	if got := wlr.Status.Containers["app"].CPURequest; got == nil || got.String() != "100m" {
		t.Errorf("stored CPU recommendation = %v, want 100m", got)
	}
	if tr := wlr.Status.Trace["app"].CPU; tr == nil || len(tr.Request) < 2 || tr.Request[len(tr.Request)-1].Value.String() != "100m" {
		t.Errorf("stored CPU trace = %+v, want the stages that arrived at 100m", tr)
	}
	if cpu := podCPU(t, r, ns, "web-pod"); cpu != "100m" {
		t.Errorf("pod CPU = %s, want the 100m recommendation applied in place", cpu)
	}
	if got := gaugeOf(t, "k8s_sustain_workload_pods", ns, "Deployment", "web"); got != 1 {
		t.Errorf("pods = %v, want 1", got)
	}
	if got := gaugeOf(t, "k8s_sustain_workload_stale_pods", ns, "Deployment", "web"); got != 0 {
		t.Errorf("stale pods = %v, want 0 after the in-place resize", got)
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
	if got := r.ledger.health("Deployment/default/broken").blockedOn; got != phasePrometheus {
		t.Errorf("broken: blocked on %q, want %q", got, phasePrometheus)
	}
	if cpu := podCPU(t, r, "default", "broken-pod"); cpu != "10m" {
		t.Errorf("broken pod CPU = %s, want it untouched at 10m", cpu)
	}
	if got := r.ledger.health("Deployment/default/healthy").blockedOn; got != "" {
		t.Errorf("healthy: blocked on %q, want not Blocked", got)
	}
	if cpu := podCPU(t, r, "default", "healthy-pod"); cpu != "100m" {
		t.Errorf("healthy pod CPU = %s, want its 100m recommendation despite its neighbour failing", cpu)
	}
}

// A Prometheus call that runs out of its own timeout wraps
// context.DeadlineExceeded. That is a failed fetch, not a shutdown: an
// overloaded Prometheus must turn the Policy not-Ready and back the identity
// off, never pass for a healthy cycle.
func TestReconcile_QueryTimeoutIsAFetchFailure(t *testing.T) {
	timeout := fmt.Errorf("querying cpu: %w", context.DeadlineExceeded)
	inputs := recommendertest.NewStaticInputs().Fail(identityOf("default", "Deployment", "web"), timeout)
	r := reconcilerWithInputs(t, inputs, true,
		ongoingDeployments("p"), establishedDeployment("default", "web", "p"), runningPod("default", "web-pod", "web"))
	rec := r.recorder.(*events.FakeRecorder)

	reconcileOnce(t, r, "p")

	ready := readyCondition(t, r, "p")
	if ready.Status == metav1.ConditionTrue || !strings.Contains(ready.Message, "1 of 1 workloads failed") {
		t.Errorf("Ready = %s %q, want False naming 1 of 1 workloads failed", ready.Status, ready.Message)
	}
	if got := r.ledger.health("Deployment/default/web").blockedOn; got != phasePrometheus {
		t.Errorf("blocked on %q, want %q", got, phasePrometheus)
	}
	if !slices.ContainsFunc(drainEvents(rec), func(e string) bool { return strings.Contains(e, "ReconciliationRetryScheduled") }) {
		t.Error("want a ReconciliationRetryScheduled event on the workload")
	}
}

func eventsWith(evs []string, reason string) int {
	n := 0
	for _, e := range evs {
		if strings.Contains(e, " "+reason+" ") {
			n++
		}
	}
	return n
}

// A forbidden eviction cannot succeed until someone fixes RBAC, but it is
// still a failure: the identity is Blocked and the Policy not Ready. It is
// retried every pass, so a fix is picked up without a restart, while the
// member's Warning is emitted once, when it enters the permanent failure.
func TestReconcile_ForbiddenEvictionBlocksAndWarnsOnce(t *testing.T) {
	const ns = "forbidden"
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, ongoingDeployments("p"), establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, obj, _ client.Object, _ ...client.SubResourceCreateOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods/" + sub}, obj.GetName(), errors.New("no RBAC"))
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, usageFor(ns, "Deployment", "web"), false)
	rec := r.recorder.(*events.FakeRecorder)
	var evs []string

	for pass := range 2 {
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if res.RequeueAfter != r.ReconcileInterval {
			t.Errorf("pass %d: RequeueAfter = %v, want the reconcile interval: a permanent failure does not back off", pass, res.RequeueAfter)
		}
		evs = append(evs, drainEvents(rec)...)
	}

	if ready := readyCondition(t, r, "p"); ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %s %q, want False", ready.Status, ready.Message)
	}
	state := seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, "Deployment", "web")
	if len(state) != 1 || labelOf(state[0], "reason") != phasePatch {
		t.Errorf("retry_state = %v, want the identity Blocked on %s", state, phasePatch)
	}
	if got := policyGauge(t, "k8s_sustain_policy_blocked_count", "p"); got != 1 {
		t.Errorf("policy_blocked_count = %v, want 1", got)
	}
	if got := eventsWith(evs, "ReconciliationFailed"); got != 1 {
		t.Errorf("ReconciliationFailed events over two passes = %d, want 1: %v", got, evs)
	}
}

// Two failed attempts are Warned about separately, each naming when the next
// pass runs, and that pass is the one Reconcile schedules.
func TestReconcile_RetryEventNamesTheScheduledPass(t *testing.T) {
	inputs := recommendertest.NewStaticInputs().Fail(identityOf("default", "Deployment", "web"), errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, true, ongoingDeployments("p"), establishedDeployment("default", "web", "p"))
	clk := clocktesting.NewFakePassiveClock(time.Now())
	r.ledger = newLedger(clk)
	rec := r.recorder.(*events.FakeRecorder)

	for attempt, wantDelay := range []time.Duration{baseRetryDelay, 2 * baseRetryDelay} {
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
		if err != nil {
			t.Fatal(err)
		}
		if res.RequeueAfter != wantDelay {
			t.Errorf("attempt %d: RequeueAfter = %v, want %v", attempt+1, res.RequeueAfter, wantDelay)
		}
		due := clk.Now().Add(wantDelay).Format(time.RFC3339)
		if !slices.ContainsFunc(drainEvents(rec), func(e string) bool {
			return strings.Contains(e, "ReconciliationRetryScheduled") && strings.Contains(e, due)
		}) {
			t.Errorf("attempt %d: want a ReconciliationRetryScheduled event naming %s", attempt+1, due)
		}
		clk.SetTime(clk.Now().Add(res.RequeueAfter))
	}
}

// A pass that cannot list its workloads still runs when the retry the last
// pass's events named falls due, not a whole reconcile interval later.
func TestReconcile_ListFailedRequeuesAtTheDueRetry(t *testing.T) {
	var listFails atomic.Bool
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, ongoingDeployments("p"), establishedDeployment("default", "web", "p")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*appsv1.DeploymentList); ok && listFails.Load() {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return cl.List(ctx, list, opts...)
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, recommendertest.NewStaticInputs().Fail(identityOf("default", "Deployment", "web"), errors.New("prometheus down")), true)
	reconcileOnce(t, r, "p")
	listFails.Store(true)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}})
	if err != nil {
		t.Fatal(err)
	}

	if got := readyCondition(t, r, "p").Reason; got != "ListFailed" {
		t.Fatalf("Ready reason = %q, want ListFailed", got)
	}
	if res.RequeueAfter != baseRetryDelay {
		t.Errorf("RequeueAfter = %v, want %v: the retry the first pass scheduled", res.RequeueAfter, baseRetryDelay)
	}
}

// A pass that leaves a member in backoff has not processed every workload: the
// Policy stays not Ready until the member's retry succeeds.
func TestReconcile_PassSkippingABackedOffMemberIsNotReady(t *testing.T) {
	inputs := recommendertest.NewStaticInputs().Fail(identityOf("default", "Deployment", "web"), errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, true, ongoingDeployments("p"), establishedDeployment("default", "web", "p"))

	reconcileOnce(t, r, "p")
	reconcileOnce(t, r, "p")

	ready := readyCondition(t, r, "p")
	if ready.Status != metav1.ConditionFalse || !strings.Contains(ready.Message, "Blocked workload identities: 1") {
		t.Errorf("Ready = %s %q, want False naming the Blocked identity", ready.Status, ready.Message)
	}
	if strings.Contains(ready.Message, "workloads failed") {
		t.Errorf("message = %q: the backed-off member was skipped, not failed again", ready.Message)
	}
}

// The controller's own status write must not trigger the next pass: that pass
// would find the member that had just failed in backoff, and do nothing.
func TestPolicyEvents_OnlyGenerationChangesTriggerAPass(t *testing.T) {
	old := ongoingDeployments("p")
	old.Generation = 1
	statusOnly := old.DeepCopy()
	statusOnly.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "PartialFailure"}}
	finalizerOnly := old.DeepCopy()
	finalizerOnly.Finalizers = nil
	specChange := old.DeepCopy()
	specChange.Generation = 2
	for _, tc := range []struct {
		name string
		next *sustainv1alpha1.Policy
		want bool
	}{
		{"status write", statusOnly, false},
		{"finalizer write", finalizerOnly, false},
		{"spec change", specChange, true},
	} {
		if got := policyEvents.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: tc.next}); got != tc.want {
			t.Errorf("%s: triggers a pass = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !policyEvents.Create(event.CreateEvent{Object: old}) || !policyEvents.Delete(event.DeleteEvent{Object: old}) {
		t.Error("creating or deleting a Policy must trigger a pass")
	}
}

func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// clockAdvancingInputs moves clk forward by took during every fetch, as a
// batch that takes minutes on a real cluster moves the ledger's clock.
type clockAdvancingInputs struct {
	*recommendertest.StaticInputs
	clk  *clocktesting.FakePassiveClock
	took time.Duration
}

func (s clockAdvancingInputs) FetchInputs(
	ctx context.Context, cfg sustainv1alpha1.ResourcesConfigs, reqs []recommender.InputsRequest,
) map[promclient.WorkloadIdentity]recommender.InputsResult {
	s.clk.SetTime(s.clk.Now().Add(s.took))
	return s.StaticInputs.FetchInputs(ctx, cfg, reqs)
}

// An identity whose members are all in retry backoff is not fetched, and the
// decision holds for the whole pass. Backoff is time-based and the fetch can
// take minutes: "expiring" leaves backoff mid-fetch, and asking again at apply
// time would process it with nothing fetched, recording a success that clears
// its retry state.
func TestReconcile_BackedOffIdentityIsNotFetched(t *testing.T) {
	static := recommendertest.NewStaticInputs()
	clk := clocktesting.NewFakePassiveClock(time.Now())
	expiring := annotatedDeployment("default", "expiring", "p")
	r := reconcilerWithInputs(t, clockAdvancingInputs{StaticInputs: static, clk: clk, took: 2 * baseRetryDelay}, true,
		ongoingDeployments("p"), expiring, annotatedDeployment("default", "healthy", "p"))
	r.ledger = newLedger(clk)
	member := targetFromObject(expiring, "Deployment")
	r.ledger.record(context.Background(), failedStep(&member, phasePatch, errors.New("apiserver unavailable")))

	reconcileOnce(t, r, "p")

	if static.Requested(identityOf("default", "Deployment", "expiring")) {
		t.Error("an identity whose every member is in backoff was fetched")
	}
	if !static.Requested(identityOf("default", "Deployment", "healthy")) {
		t.Error("the healthy identity was not fetched")
	}
	if got := r.ledger.health(member.key()).blockedOn; got != phasePatch {
		t.Errorf("expiring: blocked on %q, want it still Blocked on %s: the member skipped at fetch time must be skipped at apply time", got, phasePatch)
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
	if got := r.ledger.health("Deployment/default/app").blockedOn; got != "" {
		t.Errorf("blocked on %q for a successful-but-empty response, want not Blocked", got)
	}
}

// The replacements of the pods apply evicts read the identity's stored
// Recommendation at admission. When storing it fails, evicting would start
// them on template resources — as it did when an informer-lag 404 silently
// dropped the record. The identity's members are left alone this cycle, and
// the Policy reports the failure.
func TestReconcile_DoesNotApplyAnIdentityWhoseRecordFailed(t *testing.T) {
	const ns = "unrecorded"
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, ongoingDeployments("p"), establishedDeployment(ns, "api", "p"), runningPod(ns, "api-pod", "api")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if wlr, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok && wlr.Status.Outcome != "" {
					return apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, usageFor(ns, "Deployment", "api"), true)

	reconcileOnce(t, r, "p")

	if cpu := podCPU(t, r, ns, "api-pod"); cpu != "10m" {
		t.Errorf("api-pod CPU = %s, want it untouched at 10m: its recommendation was never stored", cpu)
	}
	if ready := readyCondition(t, r, "p"); ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %s, want False: the identity was not applied", ready.Status)
	}
}

// A Conflicted identity is governed by no Policy (ADR 0002): neither party
// rewrites its WorkloadRecommendation's spec.policy, recomputes it, or applies
// it. The stored Recommendation stays frozen, the outcome records why, and the
// identity counts towards neither Policy.
func TestReconcile_ConflictedIdentityFreezesItsRecommendation(t *testing.T) {
	const ns = "conflicted"
	blue := establishedDeployment(ns, "api-blue", "p")
	blue.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "api"
	green := establishedDeployment(ns, "api-green", "q")
	green.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "api"
	frozen := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: wlrcache.Name("Deployment", "api"),
			Labels:            map[string]string{wlrPolicyLabel: "p"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: ns, Name: "api"},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second)),
			Outcome:    sustainv1alpha1.OutcomeComputed,
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{"app": {CPURequest: qty("250m")}},
		},
	}
	inputs := usageFor(ns, "Deployment", "api").Set(identityOf(ns, "Deployment", "solo"), appUsage())
	r := reconcilerWithInputs(t, inputs, true,
		ongoingDeployments("p"), ongoingDeployments("q"), blue, green, frozen,
		runningPod(ns, "blue-pod", "api-blue"), runningPod(ns, "green-pod", "api-green"),
		establishedDeployment(ns, "solo", "p"), runningPod(ns, "solo-pod", "solo"))
	r.RecommendationRetention = wlrcache.DefaultRetention

	reconcileOnce(t, r, "p")
	reconcileOnce(t, r, "q")
	reconcileOnce(t, r, "p")

	got := getWLRFor(t, r, ns, "Deployment", "api")
	if got.Spec.Policy != "p" || got.Labels[wlrPolicyLabel] != "p" {
		t.Errorf("spec.policy/label = %q/%q, want both still p: no Policy governs a Conflicted identity",
			got.Spec.Policy, got.Labels[wlrPolicyLabel])
	}
	if got.Status.Outcome != sustainv1alpha1.OutcomeConflicted {
		t.Errorf("outcome = %q, want Conflicted", got.Status.Outcome)
	}
	if cpu := got.Status.Containers["app"].CPURequest; cpu == nil || cpu.String() != "250m" {
		t.Errorf("stored CPU = %v, want the frozen 250m", cpu)
	}
	if !got.Status.ObservedAt.Equal(&frozen.Status.ObservedAt) {
		t.Errorf("observedAt = %v, want it frozen at %v", got.Status.ObservedAt, frozen.Status.ObservedAt)
	}
	if inputs.Requested(identityOf(ns, "Deployment", "api")) {
		t.Error("a Conflicted identity was fetched for recomputation")
	}
	for _, pod := range []string{"blue-pod", "green-pod"} {
		if cpu := podCPU(t, r, ns, pod); cpu != "10m" {
			t.Errorf("%s CPU = %s, want it untouched at 10m", pod, cpu)
		}
	}
	if cpu := podCPU(t, r, ns, "solo-pod"); cpu != "100m" {
		t.Errorf("solo-pod CPU = %s, want p to keep governing its other identity", cpu)
	}
	if got := gaugeValue(t, "k8s_sustain_policy_workload_count", map[string]string{"policy": "p"}); got != 1 {
		t.Errorf("p workload count = %v, want 1: the Conflicted identity counts for no Policy", got)
	}
	if got := gaugeValue(t, "k8s_sustain_policy_workload_count", map[string]string{"policy": "q"}); got != 0 {
		t.Errorf("q workload count = %v, want 0", got)
	}
}
