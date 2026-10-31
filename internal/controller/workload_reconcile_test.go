package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func updateMode(m sustainv1alpha1.UpdateMode) *sustainv1alpha1.UpdateMode { return &m }

// The pod template is never patched, so it always differs from the
// recommendation: the event must follow what happened to pods, or it fires on
// every reconcile.
func TestReconcile_ResourcesUpdatedEventOnlyWhenPodsChanged(t *testing.T) {
	const ns = "updated-event"
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
		ongoingDeployments("p"), establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web"))
	rec := r.recorder.(*events.FakeRecorder)

	reconcileOnce(t, r, "p")
	evs := drainEvents(rec)
	want := "Updated resources on 1 pod(s) (1 resized in place, 0 evicted) for containers: [app]"
	if eventsWith(evs, "ResourcesUpdated") != 1 || !strings.Contains(strings.Join(evs, "\n"), want) {
		t.Fatalf("want one ResourcesUpdated event %q after the pod was resized, got %v", want, evs)
	}

	reconcileOnce(t, r, "p")
	if evs := drainEvents(rec); eventsWith(evs, "ResourcesUpdated") != 0 {
		t.Fatalf("no pod changed on the second reconcile, got %v", evs)
	}
}

// Recommend-only, from the flag or the Policy's own field, stores the
// Recommendation but touches no pod, and drops the identity's pod counts:
// nothing drives its pods toward it.
func TestReconcile_RecommendOnlyStoresButTouchesNoPod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		flag, field bool
	}{
		{"flag", true, false},
		{"policy", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const ns = "recommend-only"
			policy := ongoingDeployments("p")
			policy.Spec.RightSizing.RecommendOnly = tc.field
			r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
				policy, establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web"))
			r.RecommendOnly = tc.flag
			r.series.publish("p", publication{identities: []identitySeries{{
				key:    identityOf(ns, "Deployment", "web"),
				health: healthState{pods: workload.PodCounts{Total: 3, Stale: 3}, measured: true},
			}}})

			reconcileOnce(t, r, "p")

			if cpu := podCPU(t, r, ns, "web-pod"); cpu != "10m" {
				t.Errorf("pod CPU = %s, want it untouched at 10m", cpu)
			}
			if cpu := getWLRFor(t, r, ns, "Deployment", "web").Status.Containers["app"].CPURequest; cpu == nil || cpu.String() != "100m" {
				t.Errorf("stored CPU = %v, want the 100m Recommendation", cpu)
			}
			if got := len(seriesForWorkload(t, "k8s_sustain_workload_stale_pods", ns, "Deployment", "web")); got != 0 {
				t.Error("recommend-only must delete the stale-pods series")
			}
		})
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
			policy := policyManaging("p", sustainv1alpha1.UpdateTypes{Job: updateMode(sustainv1alpha1.UpdateModeOngoing)})
			job := annotatedJob("default", "nightly-etl", "p")
			// This run's object: seconds old, every run.
			job.CreationTimestamp = metav1.NewTime(time.Now().Add(-5 * time.Second))
			wlr := &sustainv1alpha1.WorkloadRecommendation{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: wlrcache.Name("Job", "nightly-etl"),
					Labels:            map[string]string{sustainv1alpha1.WLRPolicyLabel: "p"},
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

// A pass with nothing to apply is a success: once a failed member's retry is
// due, an empty Prometheus answer clears its failure and no pod is touched.
func TestReconcile_NothingToApplyClearsAPastFailure(t *testing.T) {
	const ns = "cleared"
	web := identityOf(ns, "Deployment", "web")
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs().Fail(web, errors.New("prometheus down")), true,
		ongoingDeployments("p"), establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web"))
	clk := clocktesting.NewFakePassiveClock(time.Now())
	r.ledger = newLedger(clk)
	reconcileOnce(t, r, "p")
	if ready := readyCondition(t, r, "p"); ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %s after the failed fetch, want False", ready.Status)
	}

	r.Inputs = recommendertest.NewStaticInputs()
	clk.SetTime(clk.Now().Add(baseRetryDelay))
	reconcileOnce(t, r, "p")

	if ready := readyCondition(t, r, "p"); ready.Message != "All 1 workloads have been processed." {
		t.Errorf("Ready = %s %q, want the failure cleared by the empty answer", ready.Status, ready.Message)
	}
	if got := seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, "Deployment", "web"); len(got) != 0 {
		t.Errorf("retry_state = %v, want none once the failure is cleared", got)
	}
	if cpu := podCPU(t, r, ns, "web-pod"); cpu != "10m" {
		t.Errorf("pod CPU = %s, want it untouched: there was nothing to apply", cpu)
	}
}

// A failed OnCreate count must not enter retry backoff: backoff leaves the
// identity unfetched and would freeze the WorkloadRecommendation the webhook
// reads.
func TestReconcile_OnCreateCountFailureIsNotAFailure(t *testing.T) {
	const ns = "oncreate-count"
	scheme := testFullScheme(t)
	policy := policyManaging("p", sustainv1alpha1.UpdateTypes{Deployment: updateMode(sustainv1alpha1.UpdateModeOnCreate)})
	c := fakeClientBuilder(scheme, policy, establishedDeployment(ns, "web", "p"), runningPodAt(ns, "web-pod", "web", "999m")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, usageFor(ns, "Deployment", "web"), true)
	rec := r.recorder.(*events.FakeRecorder)

	reconcileOnce(t, r, "p")
	reconcileOnce(t, r, "p")

	if ready := readyCondition(t, r, "p"); ready.Message != "All 1 workloads have been processed." {
		t.Errorf("Ready = %s %q, want True: a failed OnCreate count neither fails nor blocks", ready.Status, ready.Message)
	}
	for _, e := range drainEvents(rec) {
		if strings.HasPrefix(e, corev1.EventTypeWarning) {
			t.Errorf("unexpected Warning event: %q", e)
		}
	}
}

// A failed apply Blocks the identity on the step Apply failed on.
func TestReconcile_FailedApplyBlocksOnItsStep(t *testing.T) {
	const ns = "failed-step"
	resources := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "batch-1", UID: "batch-1-uid"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: resources}}},
		}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "batch-1-abc", UID: types.UID("batch-1-abc-uid"),
			Labels:          map[string]string{batchv1.JobNameLabel: "batch-1"},
			OwnerReferences: controllerRef("Job", "batch-1"),
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: resources}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, policyManaging("p", sustainv1alpha1.UpdateTypes{Job: updateMode(sustainv1alpha1.UpdateModeOngoing)}), job, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			// Only the pod's resize fails: a failed WorkloadRecommendation write
			// would withhold the apply altogether.
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod {
					return apierrors.NewServiceUnavailable("apiserver unavailable")
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, usageFor(ns, "Job", "batch-1"), true)

	reconcileOnce(t, r, "p")

	if ready := readyCondition(t, r, "p"); ready.Message != "1 of 1 workloads failed; Blocked workload identities: 1" {
		t.Errorf("Ready = %s %q, want the identity failed and Blocked", ready.Status, ready.Message)
	}
	state := seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, "Job", "batch-1")
	if len(state) != 1 || labelOf(state[0], "reason") != string(workload.StepResize) {
		t.Errorf("retry_state = %v, want Blocked on %s", state, workload.StepResize)
	}
}
