package controller

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// When the claim on an identity's WorkloadRecommendation fails, the identity
// is still computed but not applied: its recommendation has nowhere to be
// cached. A persistent cause (missing RBAC, a rejecting admission webhook, a
// quota) must not leave the Policy reporting Ready while the cache silently
// goes stale, and the one failure is counted once, as its withheld member's.
func TestReconcile_FailedClaimWithholdsTheIdentityAndCountsOnce(t *testing.T) {
	const ns = "unclaimed"
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, ongoingDeployments("p"), annotatedDeployment(ns, "web", "p")).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok {
					return apierrors.NewForbidden(
						schema.GroupResource{Group: "k8s.sustain.io", Resource: "workloadrecommendations"},
						obj.GetName(), errors.New("no RBAC"))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, recommendertest.NewStaticInputs(), true)

	reconcileOnce(t, r, "p")

	ready := readyCondition(t, r, "p")
	if want := "1 of 1 workloads failed; Blocked workload identities: 1"; ready.Status != metav1.ConditionFalse || ready.Message != want {
		t.Errorf("Ready = %s %q, want False %q: the failed claim counted once", ready.Status, ready.Message, want)
	}
	state := seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, "Deployment", "web")
	if len(state) != 1 || labelOf(state[0], "reason") != string(workload.StepStore) {
		t.Errorf("retry_state = %v, want the identity Blocked on %s", state, workload.StepStore)
	}
}

// A Policy that adopts an identity records into the object its own claim
// wrote, so the Recommendation it computes is attributed to it. Recording into
// the inventory's copy, read before the claim, would attribute the new numbers
// to the previous Policy, and the webhook would withhold them from this
// Policy's pods (ADR 0003).
func TestReconcile_AdoptedRecommendationIsComputedByTheAdoptingPolicy(t *testing.T) {
	const ns = "adopted"
	previous := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: wlrcache.Name("Deployment", "web"),
			Labels:            map[string]string{sustainv1alpha1.WLRPolicyLabel: "old"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Deployment", Namespace: ns, Name: "web"},
			Policy:      "old",
		},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second)),
			Outcome:    sustainv1alpha1.OutcomeComputed,
			ComputedBy: "old",
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{"app": {CPURequest: qty("250m")}},
		},
	}
	r := reconcilerWithInputs(t, usageFor(ns, "Deployment", "web"), true,
		ongoingDeployments("new"), establishedDeployment(ns, "web", "new"), previous)

	reconcileOnce(t, r, "new")

	got := getWLRFor(t, r, ns, "Deployment", "web")
	if got.Spec.Policy != "new" || got.Status.ComputedBy != "new" {
		t.Errorf("spec.policy/computedBy = %q/%q, want both the adopting Policy", got.Spec.Policy, got.Status.ComputedBy)
	}
	if cpu := got.Status.Containers["app"].CPURequest; cpu == nil || cpu.String() != "100m" {
		t.Errorf("stored CPU = %v, want the adopting Policy's 100m", cpu)
	}
}

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
	r := reconcilerWithInputs(t, inputs, false, barePodPolicy("pol"), bare)
	before := testutil.ToFloat64(wlrRefreshTotal.WithLabelValues(ns, "Pod", WLRRefreshNoSnapshot))

	reconcileOnce(t, r, "pol")

	if inputs.Requested(identityOf(ns, "Pod", "dag-task")) {
		t.Error("a departed identity without a snapshot cannot be computed, yet it was fetched")
	}
	if after := testutil.ToFloat64(wlrRefreshTotal.WithLabelValues(ns, "Pod", WLRRefreshNoSnapshot)); after-before != 1 {
		t.Errorf("no-snapshot refresh delta = %v, want 1", after-before)
	}
	if got := readyCondition(t, r, "pol").Message; got != "All 0 workloads have been processed." {
		t.Errorf("Ready message = %q: an identity the pass did not compute was not processed", got)
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
	inputs := usageFor(ns, "Deployment", "api")
	// 10m current vs ~100m recommended: an increase, so the downsize threshold
	// cannot suppress it and any non-application is unambiguous.
	r := reconcilerWithInputs(t, inputs, true /* in-place */, ongoingDeployments("p"),
		groupedDeployment(ns, "api-blue", "api", "p"), groupedDeployment(ns, "api-green", "api", "p"),
		runningPod(ns, "blue-pod", "api-blue"), runningPod(ns, "green-pod", "api-green"))

	reconcileOnce(t, r, "p")

	for _, name := range []string{"blue-pod", "green-pod"} {
		if cpu := podCPU(t, r, ns, name); cpu == "10m" {
			t.Errorf("%s still at its original 10m: every member of an owner-name group must be applied, not just the last one listed", name)
		}
	}

	var list sustainv1alpha1.WorkloadRecommendationList
	if err := r.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
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
	if calls := inputs.Calls(); len(calls) != 1 || len(calls[0].Requests) != 1 {
		t.Errorf("fetch calls = %v, want one call requesting the one identity: group members must share one computation", calls)
	}
}

// barePodPolicy manages bare pods, so it governs departedWLR's identity.
func barePodPolicy(name string) *sustainv1alpha1.Policy {
	return policyManaging(name, sustainv1alpha1.UpdateTypes{Pod: updateMode(sustainv1alpha1.UpdateModeOngoing)})
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
	r := reconcilerWithInputs(t, usageFor(ns, "Pod", "dag-task"), false, barePodPolicy("pol"), departedWLR(ns))
	r.RecommendationRetention = 24 * time.Hour

	reconcileOnce(t, r, "pol")

	got := getWLRFor(t, r, ns, "Pod", "dag-task")
	if rec := got.Status.Containers["app"]; rec.CPURequest == nil || rec.CPURequest.String() != "100m" {
		t.Errorf("departed identity's recommendation = %+v, want it recomputed to 100m", got.Status.Containers)
	}
}

// The pass records a departed identity's decision together with the fact that
// it has no live member left, which is what stamps its retained Recommendation
// servable for the retention window rather than the staleness budget: its
// observedAt stops advancing once its samples age out, so without the mark
// every run after the first would start on template resources.
func TestReconcile_MarksDepartedIdentity(t *testing.T) {
	const ns = "airflow-departs"
	wlr := departedWLR(ns)
	wlr.Status.Departed = false
	r := reconcilerWithInputs(t, usageFor(ns, "Pod", "dag-task"), false, barePodPolicy("pol"), wlr)
	r.RecommendationRetention = 24 * time.Hour

	reconcileOnce(t, r, "pol")

	if got := getWLRFor(t, r, ns, "Pod", "dag-task"); !got.Status.Departed {
		t.Error("a departed identity's record did not mark it departed")
	}
}

// A departed identity whose samples are still in its window is recomputed to
// the same Recommendation every cycle. That cost two status writes per cycle —
// the record cleared departed, the sweep set it back — whatever the refresh
// interval said. One record carries both, so a converged identity costs none.
func TestReconcile_DepartedIdentityCostsNoWriteOnceRecorded(t *testing.T) {
	const ns = "airflow-steady"
	var writes atomic.Int32
	r := reconcilerCountingWLRStatusWrites(t, usageFor(ns, "Pod", "dag-task"), &writes, barePodPolicy("pol"), departedWLR(ns))
	r.RecommendationRetention = 24 * time.Hour

	reconcileOnce(t, r, "pol")
	if writes.Load() == 0 {
		t.Fatal("the first cycle recorded nothing")
	}
	writes.Store(0)
	reconcileOnce(t, r, "pol")
	reconcileOnce(t, r, "pol")

	if n := writes.Load(); n != 0 {
		t.Errorf("%d status writes over two cycles for a converged departed identity, want 0", n)
	}
	if got := getWLRFor(t, r, ns, "Pod", "dag-task"); !got.Status.Departed {
		t.Error("the converged departed identity is no longer marked departed")
	}
}

// shutdownDuringFetch cancels the reconcile's context while its fetch runs, as
// a manager shutting down mid-pass does.
type shutdownDuringFetch struct {
	*recommendertest.StaticInputs
	cancel context.CancelFunc
}

func (s shutdownDuringFetch) FetchInputs(
	ctx context.Context, at time.Time, cfg sustainv1alpha1.ResourcesConfigs, reqs []recommender.InputsRequest,
) map[promclient.WorkloadIdentity]recommender.InputsResult {
	s.cancel()
	return s.StaticInputs.FetchInputs(ctx, at, cfg, reqs)
}

// As for a live member, a departed identity's failed fetch is a failure,
// counted and refreshed as an error, unless the reconcile's own context
// ended: then it is shutdown, and counts nothing.
func TestReconcile_DepartedFailureCountsUnlessAtShutdown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		shutdown   bool
		wantReady  metav1.ConditionStatus
		wantErrors float64
	}{
		{"failure", false, metav1.ConditionFalse, 1},
		{"shutdown", true, metav1.ConditionTrue, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := "airflow-" + tc.name
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			static := recommendertest.NewStaticInputs().Fail(identityOf(ns, "Pod", "dag-task"), context.Canceled)
			var inputs recommender.InputsFetcher = static
			if tc.shutdown {
				inputs = shutdownDuringFetch{StaticInputs: static, cancel: cancel}
			}
			r := reconcilerWithInputs(t, inputs, false, barePodPolicy("pol"), departedWLR(ns))
			r.RecommendationRetention = 24 * time.Hour
			errorRefreshes := func() float64 {
				return testutil.ToFloat64(wlrRefreshTotal.WithLabelValues(ns, "Pod", WLRRefreshError))
			}
			before := errorRefreshes()

			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "pol"}}); err != nil {
				t.Fatal(err)
			}

			if ready := readyCondition(t, r, "pol"); ready.Status != tc.wantReady {
				t.Errorf("Ready = %s %q, want %s", ready.Status, ready.Message, tc.wantReady)
			}
			if got := errorRefreshes() - before; got != tc.wantErrors {
				t.Errorf("error refresh delta = %v, want %v", got, tc.wantErrors)
			}
		})
	}
}

// The same rule holds for a live member: one whose eviction fails as the
// reconcile's own context ends was cut short by shutdown, so it is neither
// counted failed nor Blocked, and warned about to no one.
func TestReconcile_MemberFailingAtShutdownKeepsItsState(t *testing.T) {
	const ns = "member-shutdown"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheme := testFullScheme(t)
	c := fakeClientBuilder(scheme, ongoingDeployments("p"), establishedDeployment(ns, "web", "p"), runningPod(ns, "web-pod", "web")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
				cancel()
				return apierrors.NewServiceUnavailable("apiserver shutting down")
			},
		}).
		Build()
	r := reconcilerOn(c, scheme, usageFor(ns, "Deployment", "web"), false)

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}); err != nil {
		t.Fatal(err)
	}

	if ready := readyCondition(t, r, "p"); ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s %q, want True: a failure at shutdown counts nothing", ready.Status, ready.Message)
	}
	if got := seriesForWorkload(t, "k8s_sustain_workload_retry_state", ns, "Deployment", "web"); len(got) != 0 {
		t.Errorf("retry_state = %v, want the identity not Blocked", got)
	}
	if evs := drainEvents(r.recorder.(*events.FakeRecorder)); eventsWith(evs, "ReconciliationRetryScheduled") != 0 {
		t.Errorf("a failure at shutdown was warned about: %v", evs)
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
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs(), false, barePodPolicy("pol"), wlr)
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
// which has not caught up on the one the claim is about to Create — the
// read-after-write race internal/wlrcache documents. Nothing watches
// WorkloadRecommendation, so an identity the cache has not caught up on must
// still be computed from its members, not wait for the next
// --reconcile-interval.
//
// The interceptor models that lag. A fake client is read-your-writes and cannot
// express it on its own, which is why the defect was invisible to the suite.
func TestReconcile_ComputesIdentityMissingFromLaggingWLRList(t *testing.T) {
	const policyName = "lagging"
	// Older than MinWorkloadAge so the young-workload gate is not what decides
	// this test: the object's own age is the signal, and it is well past it.
	dep := establishedDeployment("default", "web", policyName)

	var wlrLists atomic.Int32
	r := reconcilerWithLaggingWLRList(t, usageFor("default", "Deployment", "web"), &wlrLists, ongoingDeployments(policyName), dep)

	reconcileOnce(t, r, policyName)
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
			"the pass trusted a cache-backed List that cannot yet see what the claim just created")
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
// member's. Only a full Reconcile discriminates — the claim alone already
// writes one merged snapshot per identity.
func TestGroupedIdentityStopsWritingStatusAfterTheFirstCycle(t *testing.T) {
	const ns = "flapgroup"

	// Deliberately DIFFERENT container specs: blue runs "main" alone at 100m,
	// green runs "main" at 250m plus a "sidecar" blue does not have. Identical
	// members would make the flap invisible.
	dep := func(name string, containers ...corev1.Container) *appsv1.Deployment {
		d := groupedDeployment(ns, name, "api", "p")
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

	inputs := recommendertest.NewStaticInputs().Set(identityOf(ns, "Deployment", "api"),
		recommender.WithCPUUsage(promclient.ContainerValues{"main": 0.1, "sidecar": 0.05}),
		recommender.WithMemoryUsage(promclient.ContainerValues{"main": 64 << 20, "sidecar": 32 << 20}))
	var statusWrites atomic.Int32
	r := reconcilerCountingWLRStatusWrites(t, inputs, &statusWrites, ongoingDeployments("p"), blue, green)

	key := types.NamespacedName{Namespace: ns, Name: wlrcache.Name("Deployment", "api")}
	cycle := func(n int) (sustainv1alpha1.WorkloadRecommendationStatus, int32) {
		t.Helper()
		statusWrites.Store(0)
		reconcileOnce(t, r, "p")
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

// A LIVE identity whose fetch comes back empty must be recorded as nodata,
// exactly as the departed path records it.
//
// The cost of leaving it unmarked is downstream: a zero status.observedAt reads
// to the webhook as "no recommendation exists yet", which it answers with a stub
// Create/Get per identity per dedup window for an object the claim had already
// created — and it keeps the nodata bucket permanently empty.
func TestReconcile_MarksNoDataForLiveIdentityWithoutSamples(t *testing.T) {
	const ns = "nodata"
	r := reconcilerWithInputs(t, recommendertest.NewStaticInputs(), true, ongoingDeployments("p"), establishedDeployment(ns, "api", "p"))

	reconcileOnce(t, r, "p")

	got := getWLRFor(t, r, ns, "Deployment", "api")
	if got.Status.Outcome != sustainv1alpha1.OutcomeNoData {
		t.Errorf("status.outcome = %q, want %q: an empty outcome reads to the webhook as source=missing "+
			"and answers every admission with a stub write", got.Status.Outcome, sustainv1alpha1.OutcomeNoData)
	}
}
