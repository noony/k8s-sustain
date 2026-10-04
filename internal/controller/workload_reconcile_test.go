package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func TestReconcileWorkload_HappyPath_ProducesRecommendationsAndPatchesPods(t *testing.T) {
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
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), true /* in-place */, pod)

	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	// Retry tracker should record success (no entry, or attempts=0).
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected attempts=0 on success, got %d", state.attempts)
	}
}

// The pod template is never patched, so it always differs from the
// recommendation: the event must follow what happened to pods, or it fires on
// every reconcile.
func TestReconcileWorkload_ResourcesUpdatedEvent_OnlyWhenPodsChanged(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "web-pod",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), true, pod)
	rec := r.recorder.(*events.FakeRecorder)

	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "ResourcesUpdated") || !strings.Contains(e, "1 pod(s)") {
			t.Fatalf("expected a ResourcesUpdated event naming the pod count, got %q", e)
		}
	default:
		t.Fatal("expected a ResourcesUpdated event after the pod was resized")
	}

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	select {
	case e := <-rec.Events:
		t.Fatalf("no pod changed on the second reconcile, got event %q", e)
	default:
	}
}

func TestReconcileWorkload_RecommendOnly_DoesNotRecyclePods(t *testing.T) {
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

	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), false, pod)
	r.RecommendOnly = true
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
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

// The per-policy spec.rightSizing.recommendOnly field must short-circuit the
// recycle path exactly like the global flag.
func TestReconcileWorkload_PolicyRecommendOnly_DoesNotRecyclePods(t *testing.T) {
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

	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), false, pod)
	// Global flag stays false — only the policy opts into dry-run.
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")
	policy.Spec.RightSizing.RecommendOnly = true

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	var got corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-pod"}, &got); err != nil {
		t.Fatalf("get pod: %v (policy recommend-only must not evict pods)", err)
	}
	if got.DeletionTimestamp != nil {
		t.Error("policy recommend-only must not delete or evict pods")
	}

	// Compute-and-cache still happens: the WorkloadRecommendation upsert runs
	// before the dry-run gate.
	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &wlr); err != nil {
		t.Errorf("expected WorkloadRecommendation default/deployment-web to be upserted in dry-run: %v", err)
	}
}

// A standalone Job is re-created on every run, so its object is always seconds
// old however long the identity has been producing samples; its
// WorkloadRecommendation is what records when k8s-sustain first saw it. The two
// cases are identical seconds-old Jobs differing only in WLR age, so dating the
// identity by anything but the older of the two collapses them onto one outcome.
func TestReconcile_AgeGateDatesTheIdentityByItsWorkloadRecommendation(t *testing.T) {
	cases := []struct {
		name        string
		wlrCreated  time.Time
		wantOutcome sustainv1alpha1.RecommendationOutcome
	}{
		{"identity known for 3h clears the gate", time.Now().Add(-3 * time.Hour), sustainv1alpha1.OutcomeComputed},
		{"identity first seen 30s ago stays gated", time.Now().Add(-30 * time.Second), sustainv1alpha1.OutcomeTooYoung},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ongoing := sustainv1alpha1.UpdateModeOngoing
			policy := policyForReconcileWorkload(t, "p")
			policy.Finalizers = []string{"k8s.sustain.io/cleanup"}
			policy.Spec.RightSizing.Update.Types.Job = &ongoing
			job := annotatedJob("default", "nightly-etl", "p")
			// This run's object: seconds old, every run.
			job.CreationTimestamp = metav1.NewTime(time.Now().Add(-5 * time.Second))
			wlr := &sustainv1alpha1.WorkloadRecommendation{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: wlrcache.Name("Job", "nightly-etl"),
					Labels:            map[string]string{wlrPolicyLabel: "p"},
					CreationTimestamp: metav1.NewTime(tc.wlrCreated),
				},
				Spec: sustainv1alpha1.WorkloadRecommendationSpec{
					WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Job", Namespace: "default", Name: "nightly-etl"},
					Policy:      "p",
				},
			}
			r := reconcilerWithInputs(t, usageFor("default", "Job", "nightly-etl"), true, policy, job, wlr)

			reconcileOnce(t, r, "p")

			got := getWLRFor(t, r, "default", "Job", "nightly-etl")
			if got.Status.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", got.Status.Outcome, tc.wantOutcome)
			}
		})
	}
}

func TestReconcileWorkload_TransientPromError_RecordsRetry(t *testing.T) {
	inputs := recommendertest.NewStaticInputs().Fail(identityOf("default", "Deployment", "web"), errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, false)
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt))
	if err == nil {
		t.Fatal("expected transient error to bubble up")
	}

	state := r.retries.getState(tgt.key())
	if state.attempts < 1 {
		t.Errorf("expected retry tracker to record at least 1 attempt, got %d", state.attempts)
	}
}

// A permanent error (e.g. 403 from missing RBAC) must be surfaced via a Warning
// event rather than swallowed, while still returning nil.
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

// Empty Prometheus results are NOT a failure: retry state is cleared and no
// patch is attempted.
func TestReconcileWorkload_NoPrometheusData_RecordsSuccessAndDoesNothing(t *testing.T) {
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs(), false)
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")

	// Prime a past failure whose backoff has elapsed, so the member is
	// processed and the success must clear it.
	r.retries.states[tgt.key()] = &retryState{attempts: 1, nextRetry: time.Now().Add(-time.Second), phase: "patch"}

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("expected retry attempts cleared on success, got %d", state.attempts)
	}
}

// A Kind == "Pod" target must never reach the selector-driven recycle path —
// no controller could recreate the pod after an eviction.
//
// The target's Selector is deliberately populated (production
// listBarePodTargets never sets it) and matches a running pod that carries
// neither the policy nor the owner-name annotation, so it belongs to no bare-pod
// group either. It stays at 999m only if the Kind == "Pod" branch holds AND the
// bare-pod resize path resolves members from the grouping rule rather than the
// selector.
func TestReconcileWorkload_PodKind_NeverRecycles(t *testing.T) {
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
	r := reconcilerWithInputs(t, usageFor("airflow", "Pod", "etl-daily"), true /* in-place */, pod)

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
	}
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
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

// The OnCreate gate: the recommendation is computed and persisted as a WLR (the
// dashboard/webhook need it) but no pod is recycled or resized.
func TestReconcileWorkload_OnCreateMode_CachesButNeverRecycles(t *testing.T) {
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
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), true /* in-place */, pod)
	tgt := deploymentTarget("default", "web")
	tgt.UpdateMode = sustainv1alpha1.UpdateModeOnCreate
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}

	var got corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-pod"}, &got); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if cpu := got.Spec.Containers[0].Resources.Requests.Cpu().String(); cpu != "999m" {
		t.Errorf("OnCreate must not resize pods; cpu request = %s, want 999m", cpu)
	}

	var wlr sustainv1alpha1.WorkloadRecommendation
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deployment-web"}, &wlr); err != nil {
		t.Fatalf("expected WLR to be cached for OnCreate target: %v", err)
	}
}

// RecycleOptions are variadic, so a refactor dropping
// WithIgnoreSafeToEvictAnnotations from the RecyclePods call would still
// compile. This pins the wiring end to end: by default a pod annotated
// safe-to-evict=false must never be evicted, and the policy override must
// evict it.
func TestReconcileWorkload_SafeToEvictAnnotation_PolicyWiring(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ignore      bool
		wantEvicted bool
	}{
		{name: "default false blocks eviction", ignore: false, wantEvicted: false},
		{name: "policy override evicts annotated pod", ignore: true, wantEvicted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Stale Running pod (999m vs the ~100m recommendation) owned by
			// the reconciled Deployment and annotated safe-to-evict=false.
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   "default",
					Name:        "web-pod",
					Labels:      map[string]string{"app": "web"},
					Annotations: map[string]string{workload.SafeToEvictAnnotation: "false"},
					OwnerReferences: []metav1.OwnerReference{{
						Controller: ptr.To(true), APIVersion: "apps/v1",
						Kind: "Deployment", Name: "web", UID: "dep-uid",
					}},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
					},
				}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}

			r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), false /* eviction mode, not in-place */)
			var evicted bool
			r.Client = fake.NewClientBuilder().
				WithScheme(r.Scheme).
				WithStatusSubresource(&sustainv1alpha1.Policy{}).
				WithObjects(pod).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
						if sub == "eviction" {
							evicted = true
							// Remove the pod so the post-eviction replacement
							// wait sees it gone and returns immediately.
							return c.Delete(ctx, obj)
						}
						return nil
					},
				}).
				Build()
			r.patcher = workload.New(r.Client, false, /* eviction mode */
				workload.WithReadyPollInterval(time.Millisecond),
				workload.WithReadyTimeout(50*time.Millisecond))

			tgt := deploymentTarget("default", "web")
			tgt.Object = &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web", UID: "dep-uid"}}

			policy := policyForReconcileWorkload(t, "p")
			policy.Spec.RightSizing.Update.Eviction.IgnoreAutoscalerSafeToEvictAnnotations = tc.ignore

			if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
				t.Fatalf("reconcileWorkload: %v", err)
			}
			if evicted != tc.wantEvicted {
				t.Errorf("evicted = %v, want %v (ignoreAutoscalerSafeToEvictAnnotations=%v)",
					evicted, tc.wantEvicted, tc.ignore)
			}
		})
	}
}

// Reproduces the crash that killed the operator process: the same target in two
// errgroup goroutines (a namespace repeated in spec.selector.namespaces made
// that possible), one on the transient-error path while the other clears retry
// state. handleStepError used to read the state back in a second, unsynchronised
// call, so the intervening recordSuccess made it nil and the deref panicked
// inside an errgroup closure, which does not recover.
func TestHandleStepError_ConcurrentSuccess_NoPanic(t *testing.T) {
	rec := events.NewFakeRecorder(1024)
	r := &PolicyReconciler{recorder: rec, retries: newRetryTracker()}
	tgt := deploymentTarget("default", "web")
	transient := apierrors.NewServiceUnavailable("prometheus down")

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := r.handleStepError(context.Background(), tgt, "prometheus", "Prometheus query failed", transient); err == nil {
				t.Error("transient error must be returned for requeue")
			}
		}()
		go func() {
			defer wg.Done()
			r.recordStepSuccess(tgt)
		}()
	}
	wg.Wait()
}

func stalePodsLabels(ns, name string) map[string]string {
	return map[string]string{"namespace": ns, "owner_kind": "Deployment", "owner_name": name}
}

func webPod(cpu string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-pod", Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestReconcileWorkload_EmitsPodCountsAfterApply(t *testing.T) {
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), true, webPod("50m"))
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")
	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, "k8s_sustain_workload_pods", stalePodsLabels("default", "web")); got != 1 {
		t.Errorf("pods = %v, want 1", got)
	}
	if got := gaugeValue(t, "k8s_sustain_workload_stale_pods", stalePodsLabels("default", "web")); got != 0 {
		t.Errorf("stale = %v, want 0 after in-place resize", got)
	}
}

func TestReconcileWorkload_OnCreate_CountsStaleWithoutTouchingPods(t *testing.T) {
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), true, webPod("999m"))
	tgt := deploymentTarget("default", "web")
	tgt.UpdateMode = sustainv1alpha1.UpdateModeOnCreate
	policy := policyForReconcileWorkload(t, "p")
	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, "k8s_sustain_workload_stale_pods", stalePodsLabels("default", "web")); got != 1 {
		t.Errorf("stale = %v, want 1", got)
	}
	var got corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "web-pod"}, &got); err != nil {
		t.Fatal(err)
	}
	if q := got.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]; q.String() != "999m" {
		t.Errorf("OnCreate must not resize, cpu = %s", q.String())
	}
}

func TestReconcileWorkload_RecommendOnly_DeletesPodCounts(t *testing.T) {
	EmitWorkloadPods(testIdentity("default", "Deployment", "web"), workload.PodCounts{Total: 3, Stale: 3})
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), false, webPod("999m"))
	r.RecommendOnly = true
	tgt := deploymentTarget("default", "web")
	policy := policyForReconcileWorkload(t, "p")
	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatal(err)
	}
	if workloadStalePods.DeleteLabelValues("default", "Deployment", "web") {
		t.Error("recommend-only must delete the stale-pods series")
	}
}

// A failed OnCreate dry run must not enter retry backoff: backoff skips the
// compute phase and would freeze the WorkloadRecommendation the webhook reads.
func TestReconcileWorkload_OnCreate_DryRunErrorIsNotAStepFailure(t *testing.T) {
	r := reconcilerWithInputs(t, usageFor("default", "Deployment", "web"), true, webPod("999m"))
	wrapped := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return apierrors.NewServiceUnavailable("apiserver unavailable")
			}
			return c.List(ctx, list, opts...)
		},
	})
	r.Client = wrapped
	r.patcher = workload.New(wrapped, true)
	tgt := deploymentTarget("default", "web")
	tgt.UpdateMode = sustainv1alpha1.UpdateModeOnCreate
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(tgt)); err != nil {
		t.Fatalf("OnCreate dry-run failure must not fail the step, got %v", err)
	}
	if state := r.retries.getState(tgt.key()); state != nil && state.attempts != 0 {
		t.Errorf("OnCreate dry-run failure must not enter retry backoff, attempts = %d", state.attempts)
	}
	rec := r.recorder.(*events.FakeRecorder)
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "Warning") {
				t.Errorf("unexpected Warning event: %q", e)
			}
			continue
		default:
		}
		break
	}
}
