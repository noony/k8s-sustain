package controller

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// A departed identity with no observed-resources snapshot has no container
// set to compute against, so it is not computed; the skip is counted so a
// stuck snapshot write cannot hide.
//
// The namespace is its own: wlrRefreshTotal is a package-level collector
// other tests count from zero.
func TestReconcileSkipsDepartedIdentityWithoutSnapshot(t *testing.T) {
	const ns = "nosnapshot"
	bare := departedWLR(ns)
	bare.Status.ObservedResources = nil
	inputs := recommendertest.NewStaticInputs()
	r := reconcilerWithInputs(t, inputs, false, barePodPolicy(t, "pol"), bare)
	before := testutil.ToFloat64(wlrRefreshTotal.WithLabelValues(ns, "Pod", WLRRefreshNoSnapshot))

	reconcileOnce(t, r, "pol")

	if inputs.Requested(identityOf(ns, "Pod", "dag-task")) {
		t.Error("a departed identity without a snapshot cannot be computed, yet it was fetched")
	}
	if after := testutil.ToFloat64(wlrRefreshTotal.WithLabelValues(ns, "Pod", WLRRefreshNoSnapshot)); after-before != 1 {
		t.Errorf("no-snapshot refresh delta = %v, want 1", after-before)
	}
}

func TestContainersFromObservedRespectsExcludeInit(t *testing.T) {
	obs := map[string]sustainv1alpha1.ObservedContainerResources{
		"main": {Init: false},
		"prep": {Init: true},
	}
	got := containersFromObserved(obs, true)
	if len(got) != 1 || got[0].Name != "main" {
		t.Fatalf("excludeInit=true gave %v, want [main]", got)
	}
	got = containersFromObserved(obs, false)
	if len(got) != 2 {
		t.Fatalf("excludeInit=false gave %d containers, want 2", len(got))
	}
}

// Pins the guarantee in docs/guides/standalone-pods-and-grouping.md: owner-name
// grouping collapses api-blue and api-green into ONE identity, recommendation
// and computation, but must not collapse the APPLY phase — a member that is
// never applied to drifts forever while looking healthy, neither skipped nor
// failed nor logged.
//
// The fetch-count bound is the other half: fanning out over members must reuse
// the identity's inputs, not re-fetch per member.
func TestReconcileAppliesToEveryMemberOfAnOwnerNameGroup(t *testing.T) {
	const ns = "grouped"
	ongoing := sustainv1alpha1.UpdateModeOngoing
	p95 := int32(95)
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
				},
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing}},
			},
		},
	}

	// Both Deployments report into the single "Deployment/api" identity.
	dep := func(name string) *appsv1.Deployment {
		d := annotatedDeployment(ns, name, "p")
		d.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
		d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "api"
		d.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
		}
		return d
	}
	// 10m current vs ~100m recommended: an increase, so the downsize threshold
	// cannot suppress it and any non-application is unambiguous.
	pod := func(name, app string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": app}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	inputs := usageFor(ns, "Deployment", "api")
	r := reconcilerWithInputs(t, inputs, true /* in-place */, policy,
		dep("api-blue"), dep("api-green"), pod("blue-pod", "api-blue"), pod("green-pod", "api-green"))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	for _, name := range []string{"blue-pod", "green-pod"} {
		var got corev1.Pod
		if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if got.Spec.Containers[0].Resources.Requests.Cpu().Cmp(resource.MustParse("10m")) == 0 {
			t.Errorf("%s still at its original 10m: every member of an owner-name group must be applied, not just the last one listed", name)
		}
	}

	var list sustainv1alpha1.WorkloadRecommendationList
	if err := r.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d WorkloadRecommendations, want 1: a group shares one identity", len(list.Items))
	}
	if want := wlrcache.Name("Deployment", "api"); list.Items[0].Name != want {
		t.Errorf("WLR name = %q, want %q", list.Items[0].Name, want)
	}

	// The group is one identity, fetched once. A second request would mean the
	// fan-out re-fetched per member.
	if calls := inputs.Calls(); len(calls) != 1 || len(calls[0]) != 1 {
		t.Errorf("fetch calls = %v, want one call requesting the one identity: group members must share one computation", calls)
	}
}

// barePodPolicy computes like policyForReconcileWorkload and manages bare
// pods, so it governs departedWLR's identity.
func barePodPolicy(t *testing.T, name string) *sustainv1alpha1.Policy {
	t.Helper()
	p := policyForReconcileWorkload(t, name)
	p.Finalizers = []string{"k8s.sustain.io/cleanup"}
	ongoing := sustainv1alpha1.UpdateModeOngoing
	p.Spec.RightSizing.Update.Types.Pod = &ongoing
	return p
}

// departedWLR is the WorkloadRecommendation of a bare-pod identity whose pods
// are all gone, known for two days, with a snapshot of container "app".
func departedWLR(ns string) *sustainv1alpha1.WorkloadRecommendation {
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: wlrcache.Name("Pod", "dag-task"),
			Labels:            map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: ns, Name: "dag-task"},
			Policy:      "pol",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			Departed:          true,
			ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{"app": {}},
		},
	}
}

// A departed identity is still recomputed every cycle, from its stored
// snapshot, so a recurring bare-pod identity's recommendation keeps up with
// its runs between them.
func TestReconcile_RefreshesDepartedIdentity(t *testing.T) {
	const ns = "airflow-refresh"
	r := reconcilerWithInputs(t, usageFor(ns, "Pod", "dag-task"), false, barePodPolicy(t, "pol"), departedWLR(ns))
	r.RecommendationRetention = 24 * time.Hour

	reconcileOnce(t, r, "pol")

	got := getWLRFor(t, r, ns, "Pod", "dag-task")
	if rec := got.Status.Containers["app"]; rec.CPURequest == nil || rec.CPURequest.String() != "100m" {
		t.Errorf("departed identity's recommendation = %+v, want it recomputed to 100m", got.Status.Containers)
	}
}

// Recomputing every identity every cycle means a departed one WILL eventually
// produce nothing: its samples age out of the query window while the retention
// window still holds the recommendation. Writing anything on that path — even
// just ObservedAt — would either wipe the retained last-known-good or tell the
// webhook that data still exists behind it.
func TestReconcile_DepartedRefreshNeverWipesGoodRecommendation(t *testing.T) {
	const ns = "airflow"
	q := resource.MustParse("250m")
	old := metav1.NewTime(time.Now().Add(-90 * time.Minute).Truncate(time.Second))
	wlr := departedWLR(ns)
	wlr.Status.ObservedAt = old
	wlr.Status.Outcome = sustainv1alpha1.OutcomeComputed
	wlr.Status.Containers = map[string]sustainv1alpha1.ContainerRecommendation{"app": {CPURequest: &q}}
	// Prometheus returns nothing: the identity's samples aged out of the window.
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs(), false, barePodPolicy(t, "pol"), wlr)
	r.RecommendationRetention = 24 * time.Hour

	reconcileOnce(t, r, "pol")

	got := getWLRFor(t, r, ns, "Pod", "dag-task")
	if len(got.Status.Containers) != 1 {
		t.Fatal("retained recommendation was wiped when its data aged out")
	}
	if !got.Status.ObservedAt.Equal(&old) {
		t.Error("ObservedAt bumped for a computation that produced nothing")
	}
	if !got.Status.Departed {
		t.Error("Departed cleared without a successful write")
	}
}

// The snapshot Lists WorkloadRecommendations through the cache-backed client,
// which has not caught up on the one discover() is about to Create — the
// read-after-write race internal/wlrcache documents. Nothing watches
// WorkloadRecommendation, so an identity the cache has not caught up on must
// still be computed from its members, not wait for the next
// --reconcile-interval.
//
// The interceptor models that lag. A fake client is read-your-writes and cannot
// express it on its own, which is why the defect was invisible to the suite.
func TestReconcile_ComputesIdentityMissingFromLaggingWLRList(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	const policyName = "lagging"
	policy := policyForReconcileWorkload(t, policyName)
	policy.Finalizers = []string{"k8s.sustain.io/cleanup"}
	policy.Spec.RightSizing.Update.Types = sustainv1alpha1.UpdateTypes{Deployment: &ongoing}
	dep := annotatedDeployment("default", "web", policyName)
	// Older than MinWorkloadAge so the young-workload gate is not what decides
	// this test: the object's own age is the signal, and it is well past it.
	dep.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))

	var wlrLists atomic.Int32
	r := reconcilerWithLaggingWLRList(t, usageFor("default", "Deployment", "web"), &wlrLists, policy, dep)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: policyName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if wlrLists.Load() == 0 {
		t.Fatal("the interceptor never saw a WorkloadRecommendationList; the test is not exercising the race")
	}

	var wlr sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "default", Name: wlrcache.Name("Deployment", "web")}
	if err := r.Get(context.Background(), key, &wlr); err != nil {
		t.Fatalf("get WorkloadRecommendation: %v", err)
	}
	if len(wlr.Status.Containers) == 0 {
		t.Error("a newly discovered workload produced no recommendation on the cycle it was first seen: " +
			"the pass trusted a cache-backed List that cannot yet see what discover just created")
	}
}

// A group shares ONE WorkloadRecommendation, so exactly one thing may decide
// what it holds. Writing it once per TARGET made every member patch the status
// back to its own view every cycle, with the surviving content decided by
// whichever goroutine finished last — a race, so half a group's containers could
// be missing depending on scheduling.
//
// The invariant: a STABLE group writes nothing after the first cycle, and what
// is stored is the union of the members' containers rather than any one
// member's. Only a full Reconcile discriminates — discovery alone already writes
// one merged snapshot per identity.
func TestGroupedIdentityStopsWritingStatusAfterTheFirstCycle(t *testing.T) {
	const ns = "flapgroup"
	ongoing := sustainv1alpha1.UpdateModeOngoing
	p95 := int32(95)
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Finalizers: []string{"k8s.sustain.io/cleanup"}},
		Spec: sustainv1alpha1.PolicySpec{
			RightSizing: sustainv1alpha1.RightSizingSpec{
				ResourcesConfigs: sustainv1alpha1.ResourcesConfigs{
					CPU:    sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
					Memory: sustainv1alpha1.ResourceConfig{Window: "168h", Requests: sustainv1alpha1.ResourceRequestsConfig{Percentile: &p95}},
				},
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing}},
			},
		},
	}

	// Deliberately DIFFERENT container specs: blue runs "main" alone at 100m,
	// green runs "main" at 250m plus a "sidecar" blue does not have. Identical
	// members would make the flap invisible.
	dep := func(name string, containers ...corev1.Container) *appsv1.Deployment {
		d := annotatedDeployment(ns, name, "p")
		d.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
		d.Spec.Template.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "api"
		d.Spec.Template.Spec.Containers = containers
		return d
	}
	withCPU := func(name, cpu string) corev1.Container {
		return corev1.Container{
			Name:      name,
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}},
		}
	}
	blue := dep("api-blue", withCPU("main", "100m"))
	green := dep("api-green", withCPU("main", "250m"), corev1.Container{Name: "sidecar"})

	inputs := recommendertest.NewStaticInputs().Set(identityOf(ns, "Deployment", "api"), &recommender.WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{"main": 0.1, "sidecar": 0.05},
		MemPerPod: promclient.ContainerValues{"main": 64 << 20, "sidecar": 32 << 20},
	})
	var statusWrites atomic.Int32
	r := reconcilerCountingWLRStatusWrites(t, inputs, &statusWrites, policy, blue, green)

	key := types.NamespacedName{Namespace: ns, Name: wlrcache.Name("Deployment", "api")}
	cycle := func(n int) (sustainv1alpha1.WorkloadRecommendationStatus, int32) {
		t.Helper()
		statusWrites.Store(0)
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
			t.Fatalf("Reconcile cycle %d: %v", n, err)
		}
		var got sustainv1alpha1.WorkloadRecommendation
		if err := r.Get(context.Background(), key, &got); err != nil {
			t.Fatalf("get WorkloadRecommendation after cycle %d: %v", n, err)
		}
		return got.Status, statusWrites.Load()
	}

	first, firstWrites := cycle(1)
	if firstWrites == 0 {
		t.Fatal("no status write at all on the first cycle: the recommendation was never cached")
	}
	if len(first.Containers) != 2 {
		t.Errorf("recommendation covers %d containers, want 2 (the union of the group): a member's own "+
			"view must not decide what the shared identity carries, got %v", len(first.Containers), first.Containers)
	}
	if len(first.ObservedResources) != 2 {
		t.Errorf("snapshot covers %d containers, want 2 (the union of the group), got %v",
			len(first.ObservedResources), first.ObservedResources)
	}

	for n := 2; n <= 3; n++ {
		status, writes := cycle(n)
		if writes != 0 {
			t.Errorf("cycle %d issued %d WorkloadRecommendation status writes, want 0: nothing changed, "+
				"so a stable group must cost no writes at all", n, writes)
		}
		if !reflect.DeepEqual(first.ObservedResources, status.ObservedResources) {
			t.Errorf("cycle %d changed the stored snapshot:\nfirst = %v\nnow   = %v",
				n, first.ObservedResources, status.ObservedResources)
		}
		if !reflect.DeepEqual(first.Containers, status.Containers) {
			t.Errorf("cycle %d changed the stored recommendation:\nfirst = %v\nnow   = %v",
				n, first.Containers, status.Containers)
		}
	}
}

// An identity's recommendation covers the union of its group's containers, so a
// member can be handed one for a container it does not declare. Nothing would
// be applied to it — no pod has that container — but changedContainers counts a
// recommended name with no matching container as CHANGED, which would put a
// phantom container into that member's ResourcesUpdated event on every cycle.
func TestRecsForTargetDropsContainersTheMemberDoesNotRun(t *testing.T) {
	recs := map[string]workload.ContainerRecommendation{
		"main":    {CPURequest: qty("100m")},
		"sidecar": {CPURequest: qty("50m")},
	}
	got := recsForTarget(recs, []corev1.Container{{Name: "main"}})
	if len(got) != 1 {
		t.Fatalf("got %d recommendations, want 1 (only the container this member runs): %v", len(got), got)
	}
	if _, ok := got["sidecar"]; ok {
		t.Error("sidecar must not be applied to a member that does not declare it")
	}
	if got["main"].CPURequest.Cmp(*qty("100m")) != 0 {
		t.Errorf("main = %v, want the identity's 100m unchanged", got["main"].CPURequest)
	}
}

// A LIVE identity whose fetch comes back empty must be recorded as nodata,
// exactly as the departed path records it.
//
// The cost of leaving it unmarked is downstream: a zero status.observedAt reads
// to the webhook as "no recommendation exists yet", which it answers with a stub
// Create/Get per identity per dedup window for an object discovery had already
// created — and it keeps the nodata bucket permanently empty.
func TestReconcile_MarksNoDataForLiveIdentityWithoutSamples(t *testing.T) {
	const ns = "nodata"
	dep := annotatedDeployment(ns, "api", "p")
	dep.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour)) // past MinWorkloadAge
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs(), true, ongoingDeployments("p"), dep)

	// Two passes: discovery creates the WLR on the first, computation sees it
	// on the second.
	reconcileOnce(t, r, "p")
	reconcileOnce(t, r, "p")

	got := getWLRFor(t, r, ns, "Deployment", "api")
	if got.Status.Outcome != sustainv1alpha1.OutcomeNoData {
		t.Errorf("status.outcome = %q, want %q: an empty outcome reads to the webhook as source=missing "+
			"and answers every admission with a stub write", got.Status.Outcome, sustainv1alpha1.OutcomeNoData)
	}
}
