package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// seriesMatching returns every series of the named family whose labels
// include want, whatever its other labels.
func seriesMatching(t *testing.T, name string, want map[string]string) []*dto.Metric {
	t.Helper()
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []*dto.Metric
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.Metric {
			if hasLabels(m, want) {
				out = append(out, m)
			}
		}
	}
	return out
}

func hasLabels(m *dto.Metric, want map[string]string) bool {
	matched := 0
	for _, l := range m.Label {
		if v, ok := want[l.GetName()]; ok {
			if v != l.GetValue() {
				return false
			}
			matched++
		}
	}
	return matched == len(want)
}

// seriesForWorkload returns every series of the named family under the
// (namespace, owner_kind, owner_name) triple, whatever its other labels.
func seriesForWorkload(t *testing.T, name, namespace, kind, workload string) []*dto.Metric {
	t.Helper()
	return seriesMatching(t, name, map[string]string{"namespace": namespace, "owner_kind": kind, "owner_name": workload})
}

// gaugeValue is the value of the one series of the named family with exactly
// labels, failing the test when there is none.
func gaugeValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	for _, m := range seriesMatching(t, name, labels) {
		if len(m.Label) == len(labels) {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %q with labels %v not found", name, labels)
	return 0
}

// memberGauges are the series published per member and container, under its
// Policy.
var memberGauges = []string{
	"k8s_sustain_recommended_cpu_cores",
	"k8s_sustain_recommended_memory_bytes",
	"k8s_sustain_workload_template_cpu_cores",
	"k8s_sustain_workload_template_memory_bytes",
}

// memberSeriesFor counts the series of every member gauge of member
// ns/kind/name under policy, for container when it is not empty.
func memberSeriesFor(t *testing.T, ns, kind, name, container, policy string) map[string]int {
	t.Helper()
	want := map[string]string{"namespace": ns, "owner_kind": kind, "owner_name": name, "policy": policy}
	if container != "" {
		want["container"] = container
	}
	out := map[string]int{}
	for _, metric := range memberGauges {
		if n := len(seriesMatching(t, metric, want)); n > 0 {
			out[metric] = n
		}
	}
	return out
}

// withMemoryRequest gives the Deployment's containers a memory request, so
// its template memory series is published too.
func withMemoryRequest(d *appsv1.Deployment) *appsv1.Deployment {
	for i := range d.Spec.Template.Spec.Containers {
		c := &d.Spec.Template.Spec.Containers[i]
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("256Mi")
	}
	return d
}

func update(t *testing.T, r *PolicyReconciler, obj client.Object, mutate func()) {
	t.Helper()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	mutate()
	if err := r.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, r *PolicyReconciler, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := r.Delete(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
}

func optInto(d *appsv1.Deployment, policy string) func() {
	return func() { d.Spec.Template.Annotations[sustainv1alpha1.PolicyAnnotation] = policy }
}

// A deleted workload's identity is departed: nothing is left to publish
// a Recommendation or pod counts for.
func TestSeries_DeletedWorkloadLosesItsMemberAndHealthSeries(t *testing.T) {
	const ns = "series-deleted"
	dep, pod := withMemoryRequest(establishedDeployment(ns, "web", "p")), runningPod(ns, "web-pod", "web")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true, ongoingDeployments("p"), dep, pod)

	reconcileOnce(t, r, "p")
	if got := memberSeriesFor(t, ns, "Deployment", "web", "app", "p"); len(got) != len(memberGauges) {
		t.Fatalf("pass 1 must publish every member gauge, got %v", got)
	}
	if got := gaugeValue(t, "k8s_sustain_workload_template_cpu_cores", map[string]string{
		"namespace": ns, "owner_kind": "Deployment", "owner_name": "web", "container": "app", "container_kind": "regular", "policy": "p",
	}); got != 0.01 {
		t.Errorf("template cpu = %v, want the template's 10m", got)
	}
	if got := len(seriesForWorkload(t, "k8s_sustain_workload_pods", ns, "Deployment", "web")); got != 1 {
		t.Fatalf("pass 1 must publish the pods series, got %d", got)
	}

	remove(t, r, dep, pod)
	reconcileOnce(t, r, "p")

	if got := memberSeriesFor(t, ns, "Deployment", "web", "", "p"); len(got) != 0 {
		t.Errorf("a deleted workload keeps member series %v", got)
	}
	for _, metric := range []string{"k8s_sustain_workload_pods", "k8s_sustain_workload_stale_pods"} {
		if got := len(seriesForWorkload(t, metric, ns, "Deployment", "web")); got != 0 {
			t.Errorf("a departed identity keeps %s", metric)
		}
	}
}

func TestSeries_OptedOutWorkloadLosesEverySeries(t *testing.T) {
	const ns = "series-optout"
	dep := establishedDeployment(ns, "web", "p")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true, ongoingDeployments("p"), dep, runningPod(ns, "web-pod", "web"))
	r.ObserveOOM(context.Background(), "p", identityOf(ns, "Deployment", "web"), "app")

	reconcileOnce(t, r, "p")
	if len(memberSeriesFor(t, ns, "Deployment", "web", "app", "p")) == 0 ||
		len(seriesForWorkload(t, "k8s_sustain_oom_observed_total", ns, "Deployment", "web")) != 1 {
		t.Fatal("pass 1 must publish the member's and the identity's series")
	}

	update(t, r, dep, func() { dep.Spec.Template.Annotations[sustainv1alpha1.OptOutAnnotation] = "true" })
	reconcileOnce(t, r, "p")

	if got := memberSeriesFor(t, ns, "Deployment", "web", "", "p"); len(got) != 0 {
		t.Errorf("an opted-out workload keeps member series %v", got)
	}
	for _, metric := range []string{"k8s_sustain_workload_pods", "k8s_sustain_oom_observed_total"} {
		if got := len(seriesForWorkload(t, metric, ns, "Deployment", "web")); got != 0 {
			t.Errorf("an opted-out identity keeps %s", metric)
		}
	}
}

// Only the Policy now governing a moved workload publishes its member series;
// its identity's series, which name no Policy, survive the move.
func TestSeries_MovedWorkloadIsPublishedOnlyByItsNewPolicy(t *testing.T) {
	const ns = "series-moved"
	dep := establishedDeployment(ns, "web", "p")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
		ongoingDeployments("p"), ongoingDeployments("q"), dep, runningPod(ns, "web-pod", "web"))
	r.ObserveOOM(context.Background(), "p", identityOf(ns, "Deployment", "web"), "app")

	reconcileOnce(t, r, "p")
	if len(memberSeriesFor(t, ns, "Deployment", "web", "app", "p")) == 0 {
		t.Fatal("pass 1 must publish the member series under p")
	}

	update(t, r, dep, optInto(dep, "q"))
	reconcileOnce(t, r, "q")
	reconcileOnce(t, r, "p")

	if got := memberSeriesFor(t, ns, "Deployment", "web", "", "p"); len(got) != 0 {
		t.Errorf("p keeps member series %v of a workload q governs", got)
	}
	if got := memberSeriesFor(t, ns, "Deployment", "web", "app", "q"); len(got) == 0 {
		t.Error("q publishes no member series for the workload it governs")
	}
	for _, metric := range []string{"k8s_sustain_workload_pods", "k8s_sustain_oom_observed_total"} {
		if got := len(seriesForWorkload(t, metric, ns, "Deployment", "web")); got != 1 {
			t.Errorf("%s: want the identity's one series to survive the move, got %d", metric, got)
		}
	}
}

func TestSeries_RemovedContainerLosesItsSeries(t *testing.T) {
	const ns = "series-container"
	dep := establishedDeployment(ns, "web", "p")
	dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, corev1.Container{Name: "side"})
	inputs := recommendertest.NewStaticInputs().Set(identityOf(ns, "Deployment", "web"), &recommender.WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"app": 0.1, "side": 0.05},
		MemPerPod: promclient.ContainerValues{"app": 64 << 20, "side": 32 << 20},
	})
	r := reconcilerWithInputs(t, inputs, true, ongoingDeployments("p"), dep)

	reconcileOnce(t, r, "p")
	if len(memberSeriesFor(t, ns, "Deployment", "web", "side", "p")) == 0 {
		t.Fatal("pass 1 must publish the side container's series")
	}

	update(t, r, dep, func() { dep.Spec.Template.Spec.Containers = dep.Spec.Template.Spec.Containers[:1] })
	reconcileOnce(t, r, "p")

	if got := memberSeriesFor(t, ns, "Deployment", "web", "side", "p"); len(got) != 0 {
		t.Errorf("a removed container keeps series %v", got)
	}
	if len(memberSeriesFor(t, ns, "Deployment", "web", "app", "p")) == 0 {
		t.Error("the remaining container lost its series")
	}
}

// A departed identity keeps its counters while its Policy keeps recomputing
// it, and loses them once its WorkloadRecommendation is swept.
func TestSeries_DepartedIdentityLosesItsCountersPastRetention(t *testing.T) {
	const ns = "series-departed"
	dep, pod := establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true, ongoingDeployments("p"), dep, pod)
	r.RecommendationRetention = time.Hour
	r.ObserveOOM(context.Background(), "p", identityOf(ns, "Deployment", "web"), "app")
	oomSeries := func() int {
		return len(seriesForWorkload(t, "k8s_sustain_oom_observed_total", ns, "Deployment", "web"))
	}

	reconcileOnce(t, r, "p")
	remove(t, r, dep, pod)
	reconcileOnce(t, r, "p")
	if oomSeries() != 1 {
		t.Fatal("a departed identity within retention must keep its counters")
	}

	r.RecommendationRetention = time.Nanosecond
	reconcileOnce(t, r, "p")
	if wlrExists(t, r, ns, "Deployment", "web") {
		t.Fatal("past retention, the sweep must delete the WorkloadRecommendation")
	}
	if oomSeries() != 1 {
		t.Fatal("the pass that swept the WorkloadRecommendation included the identity before the sweep")
	}
	reconcileOnce(t, r, "p")

	if oomSeries() != 0 {
		t.Error("an identity whose WorkloadRecommendation was swept keeps its counters")
	}
}

// A departed identity without an observed-resources snapshot is not computed,
// yet its Policy still governs it: it keeps its counters until its
// WorkloadRecommendation is swept, as any departed identity does.
func TestSeries_DepartedIdentityWithoutSnapshotKeepsItsCountersUntilTheSweep(t *testing.T) {
	const ns = "series-nosnapshot"
	bare := departedWLR(ns)
	bare.Status.ObservedResources = nil
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs(), false, barePodPolicy("pol"), bare)
	r.RecommendationRetention = 72 * time.Hour
	r.ObserveOOM(context.Background(), "pol", identityOf(ns, "Pod", "dag-task"), "app")
	oomSeries := func() int {
		return len(seriesForWorkload(t, "k8s_sustain_oom_observed_total", ns, "Pod", "dag-task"))
	}

	reconcileOnce(t, r, "pol")
	if oomSeries() != 1 {
		t.Fatal("a departed identity within retention must keep its counters")
	}

	r.RecommendationRetention = time.Nanosecond
	reconcileOnce(t, r, "pol")
	if wlrExists(t, r, ns, "Pod", "dag-task") {
		t.Fatal("past retention, the sweep must delete the WorkloadRecommendation")
	}
	reconcileOnce(t, r, "pol")

	if got := oomSeries(); got != 0 {
		t.Errorf("an identity whose WorkloadRecommendation was swept keeps %d counter series", got)
	}
}

// A Conflicted identity is governed by no Policy (ADR 0002), so none
// publishes it.
func TestSeries_ConflictedIdentityLosesEverySeries(t *testing.T) {
	const ns = "series-conflicted"
	blue := groupedDeployment(ns, "api-blue", "api", "p")
	green := groupedDeployment(ns, "api-green", "api", "p")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "api"), true,
		ongoingDeployments("p"), ongoingDeployments("q"), blue, green,
		deploymentPod(ns, "api-blue-1", "api-blue"), deploymentPod(ns, "api-green-1", "api-green"))

	reconcileOnce(t, r, "p")
	for _, member := range []string{"api-blue", "api-green"} {
		if len(memberSeriesFor(t, ns, "Deployment", member, "app", "p")) == 0 {
			t.Fatalf("pass 1 must publish %s's member series", member)
		}
	}

	update(t, r, green, optInto(green, "q"))
	reconcileOnce(t, r, "p")

	for _, member := range []string{"api-blue", "api-green"} {
		if got := memberSeriesFor(t, ns, "Deployment", member, "", "p"); len(got) != 0 {
			t.Errorf("Conflicted member %s keeps series %v", member, got)
		}
	}
	if got := len(seriesForWorkload(t, "k8s_sustain_workload_pods", ns, "Deployment", "api")); got != 0 {
		t.Error("a Conflicted identity keeps its pods series")
	}
}

// Deleting a Policy deletes every series it published and every series of
// the identities it governed, and leaves another Policy's alone.
func TestSeries_PolicyDeletionRemovesEverything(t *testing.T) {
	const ns, doomed, survivor = "series-policy-deleted", "series-doomed", "series-survivor"
	web, broken := identityOf(ns, "Deployment", "web"), identityOf(ns, "Deployment", "broken")
	inputs := usageFor(ns, "Deployment", "web").Set(identityOf(ns, "Deployment", "kept"), appUsage()).Fail(broken, errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, true,
		ongoingDeployments(doomed), ongoingDeployments(survivor),
		withMemoryRequest(establishedDeployment(ns, "web", doomed)), runningPod(ns, "web-pod", "web"),
		establishedDeployment(ns, "broken", doomed),
		establishedDeployment(ns, "kept", survivor), runningPod(ns, "kept-pod", "kept"))
	r.ObserveOOM(context.Background(), doomed, web, "app")

	reconcileOnce(t, r, doomed)
	reconcileOnce(t, r, survivor)

	policySeries := append([]string{
		"k8s_sustain_policy_workload_count",
		"k8s_sustain_policy_blocked_count",
		"k8s_sustain_policy_batch_requested_count",
		"k8s_sustain_policy_batch_resolved_count",
		"k8s_sustain_policy_batch_failures_total",
		"k8s_sustain_reconcile_total",
		"k8s_sustain_reconcile_duration_seconds",
	}, memberGauges...)
	identitySeries := map[promclient.WorkloadIdentity][]string{
		web:    {"k8s_sustain_workload_pods", "k8s_sustain_oom_observed_total"},
		broken: {"k8s_sustain_workload_retry_state", "k8s_sustain_workload_retry_attempts"},
	}
	// Guard: a series never published here would pass the deletion check
	// vacuously.
	for _, metric := range policySeries {
		if len(seriesMatching(t, metric, map[string]string{"policy": doomed})) == 0 {
			t.Fatalf("%s has no series for %s before its deletion", metric, doomed)
		}
	}
	for id, names := range identitySeries {
		for _, metric := range names {
			if len(seriesForWorkload(t, metric, id.Namespace, id.OwnerKind, id.OwnerName)) == 0 {
				t.Fatalf("%s has no series for %s before its Policy's deletion", metric, id.OwnerName)
			}
		}
	}

	var live sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: doomed}, &live); err != nil {
		t.Fatal(err)
	}
	remove(t, r, &live)
	reconcileOnce(t, r, doomed)

	for _, metric := range policySeries {
		if got := len(seriesMatching(t, metric, map[string]string{"policy": doomed})); got != 0 {
			t.Errorf("%s keeps %d series of the deleted Policy", metric, got)
		}
	}
	for id, names := range identitySeries {
		for _, metric := range names {
			if got := len(seriesForWorkload(t, metric, id.Namespace, id.OwnerKind, id.OwnerName)); got != 0 {
				t.Errorf("%s keeps %d series of %s, whose Policy was deleted", metric, got, id.OwnerName)
			}
		}
	}
	if got := gaugeValue(t, "k8s_sustain_policy_workload_count", map[string]string{"policy": survivor}); got != 1 {
		t.Errorf("%s workload count = %v, want 1", survivor, got)
	}
	if len(memberSeriesFor(t, ns, "Deployment", "kept", "app", survivor)) == 0 ||
		len(seriesForWorkload(t, "k8s_sustain_workload_pods", ns, "Deployment", "kept")) != 1 {
		t.Errorf("%s lost its series to another Policy's deletion", survivor)
	}
}

// removeByHand deletes Policy name as a user who first strips its cleanup
// finalizer does: it is gone before any deletion branch could run.
func removeByHand(t *testing.T, r *PolicyReconciler, name string) {
	t.Helper()
	var live sustainv1alpha1.Policy
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &live); err != nil {
		t.Fatal(err)
	}
	update(t, r, &live, func() { live.Finalizers = nil })
	remove(t, r, &live)
}

func TestSeries_PolicyGoneWithoutItsFinalizerLosesEverySeries(t *testing.T) {
	const ns, policy = "series-finalizer-removed", "series-finalizer-removed"
	web := identityOf(ns, "Deployment", "web")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
		ongoingDeployments(policy), withMemoryRequest(establishedDeployment(ns, "web", policy)), runningPod(ns, "web-pod", "web"))
	r.ObserveOOM(context.Background(), policy, web, "app")
	identitySeries := []string{"k8s_sustain_workload_pods", "k8s_sustain_oom_observed_total"}

	reconcileOnce(t, r, policy)
	if len(seriesMatching(t, "k8s_sustain_policy_workload_count", map[string]string{"policy": policy})) != 1 ||
		len(memberSeriesFor(t, ns, "Deployment", "web", "app", policy)) != len(memberGauges) {
		t.Fatal("pass 1 must publish the Policy's and its member's series")
	}
	for _, metric := range identitySeries {
		if len(seriesForWorkload(t, metric, ns, "Deployment", "web")) != 1 {
			t.Fatalf("pass 1 must publish %s", metric)
		}
	}

	removeByHand(t, r, policy)
	reconcileOnce(t, r, policy)

	for _, metric := range []string{"k8s_sustain_policy_workload_count", "k8s_sustain_reconcile_total"} {
		if got := len(seriesMatching(t, metric, map[string]string{"policy": policy})); got != 0 {
			t.Errorf("%s keeps %d series of the Policy", metric, got)
		}
	}
	if got := memberSeriesFor(t, ns, "Deployment", "web", "", policy); len(got) != 0 {
		t.Errorf("the member keeps series %v of a Policy that is gone", got)
	}
	for _, metric := range identitySeries {
		if got := len(seriesForWorkload(t, metric, ns, "Deployment", "web")); got != 0 {
			t.Errorf("%s keeps %d series of an identity whose Policy is gone", metric, got)
		}
	}
}

// A kill counted once its Policy is gone and forgotten triggers a pass of that
// Policy, which finds it gone and forgets the kill with it.
func TestSeries_KillOfAGonePolicyGoesWithThePassItTriggers(t *testing.T) {
	const ns, policy = "series-late-kill", "series-late-kill"
	web := identityOf(ns, "Deployment", "web")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
		ongoingDeployments(policy), establishedDeployment(ns, "web", policy), runningPod(ns, "web-pod", "web"))
	triggers := make(chan event.GenericEvent, 1)
	r.OOMTriggers = triggers
	oomSeries := func() int {
		return len(seriesForWorkload(t, "k8s_sustain_oom_observed_total", ns, "Deployment", "web"))
	}
	reconcileOnce(t, r, policy)
	removeByHand(t, r, policy)
	reconcileOnce(t, r, policy)

	r.ObserveOOM(context.Background(), policy, web, "app")
	if oomSeries() != 1 {
		t.Fatal("the kill must be counted")
	}
	select {
	case ev := <-triggers:
		reconcileOnce(t, r, ev.Object.GetName())
	default:
		t.Fatal("the kill triggered no pass of its Policy")
	}

	if got := oomSeries(); got != 0 {
		t.Errorf("a kill counted once its Policy was gone keeps %d series past the pass it triggered", got)
	}
}

// Every failed member counts once per pass, under its identity.
func TestSeries_FailedMembersCountUnderTheirIdentity(t *testing.T) {
	const ns = "series-attempts"
	api := identityOf(ns, "Deployment", "api")
	inputs := recommendertest.NewStaticInputs().Fail(api, errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, true, ongoingDeployments("p"),
		groupedDeployment(ns, "api-blue", "api", "p"), groupedDeployment(ns, "api-green", "api", "p"))
	before := testutil.ToFloat64(workloadRetryAttempts.WithLabelValues(ns, "Deployment", "api"))

	reconcileOnce(t, r, "p")

	if got := testutil.ToFloat64(workloadRetryAttempts.WithLabelValues(ns, "Deployment", "api")) - before; got != 2 {
		t.Errorf("retry_attempts delta = %v, want 2: one per failed member", got)
	}
	for _, member := range []string{"api-blue", "api-green"} {
		if got := len(seriesForWorkload(t, "k8s_sustain_workload_retry_attempts", ns, "Deployment", member)); got != 0 {
			t.Errorf("retry_attempts carries member %s", member)
		}
	}
}

// A Recommendation kept from before the Policy excluded init containers
// still names them; a member's series show only the containers the Policy
// recommends now.
func TestSeries_KeptRecommendationOfAnExcludedInitContainerHasNoSeries(t *testing.T) {
	const ns = "series-excluded-init"
	policy := ongoingDeployments("p")
	policy.Spec.RightSizing.ExcludeInitContainers = true
	dep := establishedDeployment(ns, "web", "p")
	dep.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
	kept := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: wlrcache.Name("Deployment", "web"),
			Labels:            map[string]string{wlrPolicyLabel: "p"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: ns, Name: "web"},
			Policy:      "p",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second)),
			Outcome:    sustainv1alpha1.OutcomeComputed,
			ComputedBy: "p",
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": {CPURequest: qty("100m")}, "migrate": {CPURequest: qty("50m")},
			},
		},
	}
	inputs := recommendertest.NewStaticInputs().Fail(identityOf(ns, "Deployment", "web"), errors.New("prometheus down"))
	r := reconcilerWithInputs(t, inputs, true, policy, dep, kept)

	reconcileOnce(t, r, "p")

	if got := memberSeriesFor(t, ns, "Deployment", "web", "migrate", "p"); len(got) != 0 {
		t.Errorf("migrate series = %v, want none: the Policy excludes init containers", got)
	}
	if got := gaugeValue(t, "k8s_sustain_recommended_cpu_cores", map[string]string{
		"namespace": ns, "owner_kind": "Deployment", "owner_name": "web", "container": "app", "container_kind": "regular", "policy": "p",
	}); got != 0.1 {
		t.Errorf("app recommended cpu = %v, want the kept 100m", got)
	}
}

// A pass that cannot compute an identity leaves what its last computation
// published: the Recommendation its members hold, so a Prometheus outage is
// no drop in savings, and the autoscaler it was shaped against. One that
// computes it without an autoscaler drops the autoscaler series.
func TestSeries_FollowTheLastComputation(t *testing.T) {
	const ns = "series-last-computation"
	web := identityOf(ns, "Deployment", "web")
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
			MaxReplicas:    4,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name:   corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](70)},
				},
			}},
		},
	}
	inputs := usageFor(ns, "Deployment", "web")
	r := reconcilerWithInputs(t, inputs, true, ongoingDeployments("p"), establishedDeployment(ns, "web", "p"), hpa)
	clk := clocktesting.NewFakePassiveClock(time.Now())
	r.ledger = newLedger(clk)
	autoscalerSeries := func() int {
		return len(seriesForWorkload(t, "k8s_sustain_autoscaler_present", ns, "Deployment", "web")) +
			len(seriesForWorkload(t, "k8s_sustain_autoscaler_target_configured", ns, "Deployment", "web"))
	}

	reconcileOnce(t, r, "p")
	if got := autoscalerSeries(); got != 2 {
		t.Fatalf("pass 1: want the HPA's present and cpu target series, got %d", got)
	}

	inputs.Fail(web, errors.New("prometheus down"))
	reconcileOnce(t, r, "p")
	if got := autoscalerSeries(); got != 2 {
		t.Errorf("a pass that could not compute the identity changed its autoscaler series: got %d", got)
	}
	if got := gaugeValue(t, "k8s_sustain_recommended_cpu_cores", map[string]string{
		"namespace": ns, "owner_kind": "Deployment", "owner_name": "web", "container": "app", "container_kind": "regular", "policy": "p",
	}); got != 0.1 {
		t.Errorf("recommended cpu = %v, want the 100m the member still holds", got)
	}

	remove(t, r, hpa)
	inputs.Set(web, appUsage())
	clk.SetTime(clk.Now().Add(maxRetryDelay))
	reconcileOnce(t, r, "p")
	if got := autoscalerSeries(); got != 0 {
		t.Errorf("an identity computed without an autoscaler keeps %d autoscaler series", got)
	}
}

// A departed identity is computed against no autoscaler, so its first
// computation as departed drops the autoscaler series it had while live.
func TestSeries_DepartedIdentityIsComputedWithoutItsAutoscaler(t *testing.T) {
	const ns = "series-departed-autoscaler"
	dep, pod := establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web")
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true, ongoingDeployments("p"), dep, pod, cpuHPA(ns, "web", 70))
	r.RecommendationRetention = time.Hour
	autoscalerSeries := func() int {
		return len(seriesForWorkload(t, "k8s_sustain_autoscaler_present", ns, "Deployment", "web"))
	}

	reconcileOnce(t, r, "p")
	if autoscalerSeries() != 1 {
		t.Fatal("pass 1 must publish the HPA's present series")
	}
	remove(t, r, dep, pod)
	reconcileOnce(t, r, "p")

	if got := autoscalerSeries(); got != 0 {
		t.Errorf("a departed identity keeps %d autoscaler series", got)
	}
}
