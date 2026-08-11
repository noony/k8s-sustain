package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

func TestCollectComputeItemsIncludesDepartedIdentity(t *testing.T) {
	// A bare-pod identity with a recommendation but NO live target: exactly
	// the Airflow shape that used to be uncomputable.
	departed := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      wlrcache.Name("Pod", "dag-task"),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: "ns", Name: "dag-task"},
			Policy:      "pol",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			Departed: true,
			ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{
				"worker": {},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(departed).Build()
	r := &PolicyReconciler{Client: c}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "pol"}}

	items, err := r.collectComputeItems(context.Background(), policy, targetIndex{})
	if err != nil {
		t.Fatalf("collectComputeItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1: a departed identity must still be computed", len(items))
	}
	if len(items[0].Targets) != 0 {
		t.Error("Targets must be empty for a departed identity")
	}
	if items[0].Identity.OwnerName != "dag-task" {
		t.Errorf("identity = %q, want dag-task", items[0].Identity.OwnerName)
	}
}

func TestCollectComputeItemsLinksLiveTarget(t *testing.T) {
	live := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      wlrcache.Name("Deployment", "api"),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "ns", Name: "api"},
			Policy:      "pol",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{"main": {}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(live).Build()
	r := &PolicyReconciler{Client: c}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "pol"}}

	target := &workloadTarget{
		Kind: "Deployment", Name: "api", Namespace: "ns",
		IdentityKind: "Deployment", IdentityName: "api",
	}
	idx := targetIndex{
		{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "api"}: {target},
	}

	items, err := r.collectComputeItems(context.Background(), policy, idx)
	if err != nil {
		t.Fatalf("collectComputeItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if len(items[0].Targets) != 1 || items[0].Targets[0] != target {
		t.Error("live identity must be linked to its target so application can run")
	}
}

func TestCollectComputeItemsIgnoresOtherPolicies(t *testing.T) {
	other := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      wlrcache.Name("Deployment", "other"),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "different"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: "ns", Name: "other"},
			Policy:      "different",
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(other).Build()
	r := &PolicyReconciler{Client: c}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "pol"}}

	items, err := r.collectComputeItems(context.Background(), policy, targetIndex{})
	if err != nil {
		t.Fatalf("collectComputeItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d items, want 0: another policy's WLRs are not this policy's work", len(items))
	}
}

// TestJobAndPodIdentitiesBecomeShardCandidates is a collectComputeItems +
// containersFromObserved smoke check, NOT a pin on the Job/Pod
// shard-candidacy exclusion: collectComputeItems has no kind filter of its
// own, and the candidate-building loop this simulates below re-implements
// only the empty-containers half of Reconcile's real filter, so it cannot
// observe whether Reconcile's own kind-exclusion branch exists in either
// direction. It passes identically with or without that branch. The test
// that actually pins the exclusion's removal is
// TestReconcileBatchesJobAndPodIdentities in policy_controller_test.go
// (policy_controller_test.go:253-270 at the time of writing), which drives a
// real Reconcile() and reads the real candidate count.
func TestJobAndPodIdentitiesBecomeShardCandidates(t *testing.T) {
	mk := func(kind, name string) *sustainv1alpha1.WorkloadRecommendation {
		return &sustainv1alpha1.WorkloadRecommendation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns",
				Name:      wlrcache.Name(kind, name),
				Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
			},
			Spec: sustainv1alpha1.WorkloadRecommendationSpec{
				WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: kind, Namespace: "ns", Name: name},
				Policy:      "pol",
			},
			Status: sustainv1alpha1.WorkloadRecommendationStatus{
				ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{"main": {}},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(mk("Job", "nightly"), mk("Pod", "dag-task"), mk("Deployment", "api")).Build()
	r := &PolicyReconciler{Client: c}
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "pol"}}

	items, err := r.collectComputeItems(context.Background(), policy, targetIndex{})
	if err != nil {
		t.Fatalf("collectComputeItems: %v", err)
	}

	var cands []promclient.ShardCandidate
	for i := range items {
		containers := containersFromObserved(items[i].WLR.Status.ObservedResources, false)
		if len(containers) == 0 {
			continue
		}
		cands = append(cands, promclient.ShardCandidate{Identity: items[i].Identity, Containers: len(containers)})
	}

	kinds := map[string]bool{}
	for _, c := range cands {
		kinds[c.Identity.OwnerKind] = true
	}
	for _, want := range []string{"Job", "Pod", "Deployment"} {
		if !kinds[want] {
			t.Errorf("%s missing after collectComputeItems + containersFromObserved: "+
				"every kind's WLR must surface a non-empty container snapshot here regardless of kind "+
				"(this does not, by itself, prove Reconcile's own candidate loop batches it -- see "+
				"TestReconcileBatchesJobAndPodIdentities for that)", want)
		}
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

// TestReconcileAppliesToEveryMemberOfAnOwnerNameGroup pins the guarantee
// documented in docs/guides/standalone-pods-and-grouping.md: "Recycling stays
// per-real-Deployment — each Deployment's own pods are evicted or resized
// independently, using the shared computed recommendation."
//
// Owner-name grouping collapses api-blue and api-green into ONE identity, one
// WorkloadRecommendation and one Prometheus computation. It must not collapse
// the APPLY phase: each Deployment owns its own pods, and a group member that
// is never applied to drifts forever while looking healthy — it is not
// skipped, not failed, and not logged.
//
// The query-count bound is the other half of the invariant: fanning out over
// members must reuse the identity's already-batched inputs rather than
// re-querying Prometheus per member.
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

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestCount.Add(1)
		_ = req.ParseForm()
		q := req.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		// The sharded batch attributes samples back to an identity via the
		// namespace/owner_kind/owner_name labels, so a bare {container} metric
		// would silently resolve to no inputs at all.
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"grouped","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"grouped","owner_kind":"Deployment","owner_name":"api","container":"app"},"value":[0,"67108864"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true /* in-place */, policy,
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

	// One CPU + one memory + one OOM shard query for the single identity.
	// Doubling would mean the fan-out re-queried Prometheus per member.
	if got := requestCount.Load(); got > 3 {
		t.Errorf("%d Prometheus requests, want <= 3: group members must share one computation", got)
	}
}

// TestDepartedRefreshWithNilInputsFetchesPerWorkload covers the departed half
// of buildRecommendations' nil-inputs fallback: whenever a departed identity
// reaches refreshDepartedRecommendation with a nil inputs (whether because
// Reconcile's candidate loop had no snapshot to batch it with yet, or -- as
// simulated directly here -- any other reason), it must fall back to
// buildRecommendations' own per-workload fetch rather than treating nil as
// "no data". A departed bare pod is the single most common shape on this path,
// so a nil that silently produced nothing would re-freeze exactly the
// recommendations this change exists to refresh.
func TestDepartedRefreshWithNilInputsFetchesPerWorkload(t *testing.T) {
	const ns = "airflow"
	server := promServerForReconcile(t)
	defer server.Close()
	pc, err := promclient.New(server.URL)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}

	wlr := &sustainv1alpha1.WorkloadRecommendation{
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
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(wlr).Build()
	r := &PolicyReconciler{Client: c, PrometheusClient: pc}

	it := computeItem{
		WLR:      wlr,
		Observed: identityObserved(wlr, nil),
		Identity: promclient.WorkloadIdentity{Namespace: ns, OwnerKind: "Pod", OwnerName: "dag-task"},
	}
	// nil inputs: simulating a nil the batch never covered.
	// refreshDepartedRecommendation no longer takes a snapshotPending
	// parameter -- see its call site in computeAndApply for why one would
	// always be false there anyway.
	if err := r.refreshDepartedRecommendation(
		context.Background(), policyForReconcileWorkload(t, "pol"), it, nil, nil); err != nil {
		t.Fatalf("refreshDepartedRecommendation: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: ns, Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Containers) != 1 {
		t.Fatalf("containers = %d, want 1: a nil inputs must trigger the per-workload fetch, not be read as no data", len(got.Status.Containers))
	}
	// Fresh samples prove the identity ran again, so the successful write
	// clears Departed. Only the empty-result path preserves it.
	if got.Status.Departed {
		t.Error("Departed must be cleared by a successful write")
	}
}

// TestDepartedRefreshNeverWipesGoodRecommendation pins the empty-recs write
// rule, which is the load-bearing behaviour of WLR-driven refresh: recomputing
// every identity every cycle means a departed identity WILL eventually produce
// nothing, because its samples age out of the query window while the retention
// window still holds the recommendation. Writing anything at all on that path
// — even just ObservedAt — would either wipe the retained last-known-good or
// tell the admission webhook that data still exists behind it.
//
// The namespace is deliberately not "ns": this path emits the
// (namespace, ownerKind, outcome) = ("ns", "Pod", retained-empty) series that
// TestEmitWLRRefreshRecordsOutcome counts from zero, and wlrRefreshTotal is a
// package-level collector shared across the suite. With -shuffle=on the two
// tests would otherwise collide depending on execution order.
func TestDepartedRefreshNeverWipesGoodRecommendation(t *testing.T) {
	const ns = "airflow"
	q := resource.MustParse("250m")
	old := metav1.NewTime(time.Now().Add(-90 * time.Minute).Truncate(time.Second))
	wlr := &sustainv1alpha1.WorkloadRecommendation{
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
			ObservedAt:        old,
			Source:            sustainv1alpha1.RecommendationSourcePrometheus,
			Containers:        map[string]sustainv1alpha1.ContainerRecommendation{"worker": {CPURequest: &q}},
			ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{"worker": {}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(wlr).Build()
	r := &PolicyReconciler{Client: c}

	// Prometheus returns nothing: the identity's samples aged out of the window.
	empty := &recommender.WorkloadInputs{
		CPUPerPod: promclient.ContainerValues{},
		MemPerPod: promclient.ContainerValues{},
	}
	it := computeItem{
		WLR:      wlr,
		Observed: identityObserved(wlr, nil),
		Identity: promclient.WorkloadIdentity{Namespace: ns, OwnerKind: "Pod", OwnerName: "dag-task"},
	}
	policy := policyForReconcileWorkload(t, "pol")
	if err := r.refreshDepartedRecommendation(context.Background(), policy, it, empty, nil); err != nil {
		t.Fatalf("refreshDepartedRecommendation: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: ns, Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
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

// discover() Creates the WorkloadRecommendation objects and collectComputeItems
// immediately Lists them back through the same cache-backed manager client.
// That is the read-after-write race internal/wlrcache documents and that was
// already fixed for Get at three sites; the List was missed. Nothing watches
// WorkloadRecommendation, so an identity the cache has not caught up on gets no
// recommendation at all until the next --reconcile-interval.
//
// The interceptor below models exactly that lag: the first
// WorkloadRecommendationList of the process comes back empty even though
// discovery just created an object. A fake client is read-your-writes and
// structurally cannot express this on its own, which is why the defect was
// invisible to the whole suite.
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

	// The sharded batch attributes samples back to an identity via the
	// namespace/owner_kind/owner_name labels, so promServerForReconcile's bare
	// {container} metric would resolve to no inputs at all.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		q := req.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		const labels = `"namespace":"default","owner_kind":"Deployment","owner_name":"web","container":"app"`
		switch {
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{` + labels + `},"value":[0,"0.1"]}]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{` + labels + `},"value":[0,"67108864"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	var wlrLists atomic.Int32
	r := reconcilerWithLaggingWLRList(t, server, &wlrLists, policy, dep)

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
			"collectComputeItems trusted a cache-backed List that cannot yet see what discover just created")
	}
}

// TestGroupedIdentityStopsWritingStatusAfterTheFirstCycle is the write-path
// control for owner-name grouping.
//
// A group shares ONE WorkloadRecommendation, so exactly one thing may decide
// what that object holds. Writing it once per TARGET made every member patch
// the identity's status back to its own view on every cycle: two members whose
// container specs differ at all produced a permanent per-cycle write storm, and
// the surviving content — both status.observedResources and, because each
// member computed its own recommendation, status.containers — was decided by
// whichever member's goroutine finished last. Those run in parallel, so it was
// a race, not listing order: half a group's containers could be missing from
// the stored recommendation depending on scheduling.
//
// The invariant this pins: a STABLE group writes nothing after the first cycle,
// and what is stored is the identity's own view (the union of the members'
// containers) rather than any single member's.
//
// A discovery-only test cannot see this — discovery already writes one merged
// snapshot per identity. Only a full Reconcile, which runs the computation
// write path too, discriminates.
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

	sample := func(container, value string) string {
		return `{"metric":{"namespace":"` + ns + `","owner_kind":"Deployment","owner_name":"api","container":"` +
			container + `"},"value":[0,"` + value + `"]}`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		q := req.Form.Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "workload_max_pod_cpu"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				sample("main", "0.1") + `,` + sample("sidecar", "0.05") + `]}}`))
		case strings.Contains(q, "workload_max_pod_memory"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				sample("main", "67108864") + `,` + sample("sidecar", "33554432") + `]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer server.Close()

	var statusWrites atomic.Int32
	r := reconcilerCountingWLRStatusWrites(t, server, &statusWrites, policy, blue, green)

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

// TestComputeIdentity_MarksNoDataForLiveIdentityWithoutSamples: a LIVE
// identity whose Prometheus queries come back empty must be recorded as
// nodata, exactly as the departed path records it.
//
// The failure this guards against is not in the controller but downstream: a
// zero status.observedAt is what the webhook reads as "no recommendation
// exists yet" (RecSourceMissing), and it answers that by asking the
// controller for one — a stub Create/Get per identity per dedup window, for
// an object discovery had already created. Left unmarked, that is the steady
// state for any workload whose series never appear, and it keeps the nodata
// bucket that ErrRecommendationNoData exists to fill permanently empty.
func TestComputeIdentity_MarksNoDataForLiveIdentityWithoutSamples(t *testing.T) {
	const ns = "nodata"
	ongoing := sustainv1alpha1.UpdateModeOngoing
	p95 := int32(95)
	policy := &sustainv1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
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

	dep := annotatedDeployment(ns, "api", "p")
	dep.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour)) // past MinWorkloadAge
	dep.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
	}

	// Prometheus knows nothing about this identity.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	r := reconcilerWithProm(t, server, true, policy, dep)

	// Two passes: discovery creates the WLR on the first, computation sees it
	// on the second.
	for i := range 2 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}

	got := getWLRFor(t, r, ns, "Deployment", "api")
	if got.Status.ObservedAt.IsZero() {
		t.Error("live identity with no samples left a zero ObservedAt; the webhook reads that as source=missing " +
			"and answers every admission with a stub write")
	}
	if got.Status.Source != sustainv1alpha1.RecommendationSourceNoData {
		t.Errorf("status.source = %q, want %q", got.Status.Source, sustainv1alpha1.RecommendationSourceNoData)
	}
}
