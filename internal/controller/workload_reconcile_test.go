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
	if eventsWith(evs, "ResourcesUpdated") != 1 || !strings.Contains(strings.Join(evs, "\n"), "1 pod(s)") {
		t.Fatalf("want one ResourcesUpdated event naming the pod count after the pod was resized, got %v", evs)
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

// OnCreate stores the Recommendation, the webhook's to inject at the next
// rollout, but resizes no running pod; the pods still waiting for that
// rollout are counted stale.
func TestReconcile_OnCreateStoresAndCountsWithoutTouchingPods(t *testing.T) {
	const ns = "oncreate"
	policy := policyManaging("p", sustainv1alpha1.UpdateTypes{Deployment: updateMode(sustainv1alpha1.UpdateModeOnCreate)})
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
		policy, establishedDeployment(ns, "web", "p"), runningPodAt(ns, "web-pod", "web", "999m"))

	reconcileOnce(t, r, "p")

	if cpu := podCPU(t, r, ns, "web-pod"); cpu != "999m" {
		t.Errorf("OnCreate must not resize pods; cpu request = %s, want 999m", cpu)
	}
	if cpu := getWLRFor(t, r, ns, "Deployment", "web").Status.Containers["app"].CPURequest; cpu == nil || cpu.String() != "100m" {
		t.Errorf("stored CPU = %v, want the 100m Recommendation for the webhook", cpu)
	}
	if got := gaugeOf(t, "k8s_sustain_workload_stale_pods", ns, "Deployment", "web"); got != 1 {
		t.Errorf("stale pods = %v, want 1", got)
	}
}

// A refactor dropping IgnoreSafeToEvict from the ApplySettings the controller
// builds would still compile. This pins the wiring end to end: by default a
// pod annotated safe-to-evict=false must never be evicted, and the policy
// override must evict it.
func TestReconcile_SafeToEvictAnnotationPolicyWiring(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ignore      bool
		wantEvicted bool
	}{
		{name: "default false blocks eviction", ignore: false, wantEvicted: false},
		{name: "policy override evicts annotated pod", ignore: true, wantEvicted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const ns = "safe-to-evict"
			policy := ongoingDeployments("p")
			policy.Spec.RightSizing.Update.Eviction.IgnoreAutoscalerSafeToEvictAnnotations = tc.ignore
			// Stale (999m vs the 100m recommendation) and annotated
			// safe-to-evict=false.
			pod := runningPodAt(ns, "web-pod", "web", "999m")
			pod.Annotations = map[string]string{workload.SafeToEvictAnnotation: "false"}
			var evicted bool
			scheme := testFullScheme(t)
			c := fakeClientBuilder(scheme, policy, establishedDeployment(ns, "web", "p"), pod).
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
			r := reconcilerOn(c, scheme, usageFor(ns, "Deployment", "web"), false)
			r.patcher = workload.New(c, false,
				workload.WithReadyPollInterval(time.Millisecond),
				workload.WithReadyTimeout(50*time.Millisecond))

			reconcileOnce(t, r, "p")

			if evicted != tc.wantEvicted {
				t.Errorf("evicted = %v, want %v (ignoreAutoscalerSafeToEvictAnnotations=%v)",
					evicted, tc.wantEvicted, tc.ignore)
			}
		})
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

// A bare-pod identity's running pods are resized in place under Ongoing,
// never evicted, and left alone under OnCreate.
func TestReconcile_BarePodsAreResizedOnlyUnderOngoing(t *testing.T) {
	for _, tc := range []struct {
		mode        sustainv1alpha1.UpdateMode
		wantResized bool
	}{
		{sustainv1alpha1.UpdateModeOngoing, true},
		{sustainv1alpha1.UpdateModeOnCreate, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			pod := barePod("airflow", "etl-run-1", "etl-daily")
			pod.Spec.Containers[0].Name = "app"
			r := reconcilerWithInputs(t, usageFor("airflow", "Pod", "etl-daily"), true)
			rec := recordPods(t, r, policyManaging("p", sustainv1alpha1.UpdateTypes{Pod: updateMode(tc.mode)}), pod)

			reconcileOnce(t, r, "p")

			if rec.resized["etl-run-1"] != tc.wantResized {
				t.Errorf("resized = %v, want %v", rec.resized["etl-run-1"], tc.wantResized)
			}
			if len(rec.evicted) != 0 {
				t.Errorf("bare pods must never be evicted, got %v", rec.evicted)
			}
		})
	}
}

// jobWithPod is a standalone Job in ns opted into "p", running container
// "app" at cpu, and its one running pod.
func jobWithPod(ns, name, cpu string) (*batchv1.Job, *corev1.Pod) {
	resources := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name + "-uid")},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: resources}}},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name + "-abc",
			Labels:          map[string]string{batchv1.JobNameLabel: name},
			OwnerReferences: controllerRef("Job", name),
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: resources}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return job, pod
}

// A standalone Job's running pod is resized in place under Ongoing, never
// evicted, and only counted under OnCreate.
func TestReconcile_JobPodIsResizedOnlyUnderOngoing(t *testing.T) {
	for _, tc := range []struct {
		mode        sustainv1alpha1.UpdateMode
		wantResized bool
	}{
		{sustainv1alpha1.UpdateModeOngoing, true},
		{sustainv1alpha1.UpdateModeOnCreate, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			ns := "job-" + strings.ToLower(string(tc.mode))
			job, pod := jobWithPod(ns, "batch-1", "999m")
			r := reconcilerWithInputs(t, usageFor(ns, "Job", "batch-1"), true)
			rec := recordPods(t, r, policyManaging("p", sustainv1alpha1.UpdateTypes{Job: updateMode(tc.mode)}), job, pod)

			reconcileOnce(t, r, "p")

			if rec.resized[pod.Name] != tc.wantResized {
				t.Errorf("resized = %v, want %v", rec.resized[pod.Name], tc.wantResized)
			}
			if len(rec.evicted) != 0 {
				t.Errorf("a standalone job pod must never be evicted, got %v", rec.evicted)
			}
			if tc.mode != sustainv1alpha1.UpdateModeOnCreate {
				return
			}
			if got := gaugeOf(t, "k8s_sustain_workload_pods", ns, "Job", "batch-1"); got != 1 {
				t.Errorf("pods = %v, want 1", got)
			}
			if got := gaugeOf(t, "k8s_sustain_workload_stale_pods", ns, "Job", "batch-1"); got != 1 {
				t.Errorf("stale = %v, want 1", got)
			}
		})
	}
}

// A CronJob's running job pods are resized in place, the CronJob spec is left
// alone, and the event names the in-place family.
func TestReconcile_CronJobResizesActiveRunInPlace(t *testing.T) {
	const ns = "cronjob"
	cj := annotatedCronJob(ns, "nightly", "p")
	run, pod := jobWithPod(ns, "nightly-1", "500m")
	run.OwnerReferences = controllerRef("CronJob", "nightly")
	r := reconcilerWithInputs(t, usageFor(ns, "CronJob", "nightly"), true)
	rec := recordPods(t, r, policyManaging("p", sustainv1alpha1.UpdateTypes{CronJob: updateMode(sustainv1alpha1.UpdateModeOngoing)}), cj, run, pod)
	evRec := r.recorder.(*events.FakeRecorder)

	reconcileOnce(t, r, "p")

	if !rec.resized["nightly-1-abc"] || len(rec.evicted) != 0 {
		t.Errorf("want the active run's pod resized in place and nothing evicted, resized %v evicted %v", rec.resized, rec.evicted)
	}
	if evs := drainEvents(evRec); !strings.Contains(strings.Join(evs, "\n"), "In-place resized 1 cronjob pod(s)") {
		t.Errorf("events = %v, want a ResourcesUpdated event in the in-place family's text", evs)
	}
}

// The failed step a Blocked identity reports follows the kind's family:
// "patch" for kinds that evict, "resize" for kinds only ever resized in place.
func TestReconcile_FailedApplyBlocksOnItsFamilyStep(t *testing.T) {
	unavailable := apierrors.NewServiceUnavailable("apiserver unavailable")
	// Only the pods' resize and eviction fail: a failed WorkloadRecommendation
	// write would withhold the apply altogether.
	failPods := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return unavailable
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object, opts ...client.SubResourceCreateOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return unavailable
			}
			return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
		},
	}
	const deployNS, jobNS = "family-evict", "family-resize"
	job, jobPod := jobWithPod(jobNS, "batch-1", "999m")
	ongoing := updateMode(sustainv1alpha1.UpdateModeOngoing)
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, policyManaging("p", sustainv1alpha1.UpdateTypes{Deployment: ongoing, Job: ongoing}),
		establishedDeployment(deployNS, "web", "p"), runningPodAt(deployNS, "web-pod", "web", "999m"), job, jobPod).
		WithInterceptorFuncs(failPods).
		Build()
	inputs := usageFor(deployNS, "Deployment", "web").Set(identityOf(jobNS, "Job", "batch-1"), appUsage())
	r := reconcilerOn(c, scheme, inputs, true)

	reconcileOnce(t, r, "p")

	if ready := readyCondition(t, r, "p"); ready.Message != "2 of 2 workloads failed; Blocked workload identities: 2" {
		t.Errorf("Ready = %s %q, want both identities failed and Blocked", ready.Status, ready.Message)
	}
	for _, tc := range []struct{ ns, kind, name, wantStep string }{
		{deployNS, "Deployment", "web", phasePatch},
		{jobNS, "Job", "batch-1", phaseResize},
	} {
		state := seriesForWorkload(t, "k8s_sustain_workload_retry_state", tc.ns, tc.kind, tc.name)
		if len(state) != 1 || labelOf(state[0], "reason") != tc.wantStep {
			t.Errorf("%s retry_state = %v, want Blocked on %s", tc.kind, state, tc.wantStep)
		}
	}
}
