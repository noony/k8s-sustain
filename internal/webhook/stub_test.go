package webhook

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

func TestCreateStubWritesEmptyStatusObject(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	h := &Handler{Client: c}

	if err := h.createStub(context.Background(), "prod", "Job", "etl", "p1", nil, nil); err != nil {
		t.Fatal(err)
	}

	var wlr sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "prod", Name: wlrcache.Name("Job", "etl")}
	if err := c.Get(context.Background(), key, &wlr); err != nil {
		t.Fatalf("stub not created: %v", err)
	}
	if wlr.Spec.WorkloadRef.Kind != "Job" || wlr.Spec.WorkloadRef.Name != "etl" {
		t.Fatalf("bad workloadRef: %+v", wlr.Spec.WorkloadRef)
	}
	if wlr.Spec.WorkloadRef.Namespace != "prod" {
		t.Fatalf("bad workloadRef namespace: %+v", wlr.Spec.WorkloadRef)
	}
	if wlr.Spec.Policy != "p1" {
		t.Fatalf("bad policy: %q", wlr.Spec.Policy)
	}
	if len(wlr.Status.Containers) != 0 {
		t.Fatalf("stub must have empty status, got %+v", wlr.Status)
	}
	if wlr.Labels[sustainv1alpha1.WLRPolicyLabel] != "p1" {
		t.Fatalf("stub must carry the policy label for reaping: %+v", wlr.Labels)
	}
	// Provenance marker: "empty status" cannot say on its own that the webhook
	// created this object, since a controller-created WLR is transiently
	// empty-status too. Nothing branches on it, so this pins the write only.
	if wlr.Labels[sustainv1alpha1.WLRStubLabel] != "true" {
		t.Fatalf("stub must carry the webhook-provenance marker: %+v", wlr.Labels)
	}
}

// A burst of pods for one workload issues many creates of the same object.
// All but the first must be absorbed silently — this is what makes the
// mechanism self-debouncing and is why no rate limiter is needed.
func TestCreateStubIsIdempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	h := &Handler{Client: c}

	for i := range 50 {
		if err := h.createStub(context.Background(), "prod", "Job", "etl", "p1", nil, nil); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}

	var list sustainv1alpha1.WorkloadRecommendationList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d stubs want 1", len(list.Items))
	}
}

// The missing-WLR admission path must request a recommendation, otherwise a
// workload the controller never catches alive (a short-lived Job, a bare-pod
// group) would start every one of its pods on template resources forever.
func TestAdmitRequestsRecommendationWhenWLRMissing(t *testing.T) {
	env := newAdmitEnv(t,
		basicPolicy("p1", sustainv1alpha1.UpdateModeOnCreate),
		deploymentReplicaSet("prod", "api-rs", "api"),
	)

	resp := env.handler.admit(context.Background(), admissionRequestFor(t, podWithRSOwner("prod", "api-rs-abc", "api-rs", "p1")))
	if !resp.Allowed {
		t.Fatal("pod must be allowed")
	}

	key := types.NamespacedName{Namespace: "prod", Name: wlrcache.Name("Deployment", "api")}
	var wlr sustainv1alpha1.WorkloadRecommendation
	// The create is detached from the AdmissionResponse on purpose, so poll.
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = env.handler.Client.Get(context.Background(), key, &wlr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("admission did not create a stub: %v", err)
	}
	if wlr.Spec.Policy != "p1" || wlr.Spec.WorkloadRef.Kind != "Deployment" || wlr.Spec.WorkloadRef.Name != "api" {
		t.Fatalf("stub does not identify the admitted workload: %+v", wlr.Spec)
	}
}

// The stale path must NOT create a stub: the object already exists, so the
// Create is a guaranteed AlreadyExists no-op — a wasted apiserver write on
// every admission of a workload whose controller has fallen behind, which is
// exactly when the apiserver is least able to absorb it.
func TestAdmitDoesNotRequestRecommendationWhenWLRStale(t *testing.T) {
	stale := freshWLR("Deployment", "prod", "api", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "128Mi"),
	})
	stale.Status.ObservedAt = metav1.NewTime(time.Now().Add(-24 * time.Hour))

	env := newAdmitEnv(t,
		basicPolicy("p1", sustainv1alpha1.UpdateModeOnCreate),
		deploymentReplicaSet("prod", "api-rs", "api"),
		stale,
	)

	resp := env.handler.admit(context.Background(), admissionRequestFor(t, podWithRSOwner("prod", "api-rs-abc", "api-rs", "p1")))
	if !resp.Allowed {
		t.Fatal("pod must be allowed")
	}
	if len(resp.Patch) != 0 {
		t.Fatalf("stale WLR must not be injected, got patch: %s", resp.Patch)
	}

	// Give any (incorrectly) spawned goroutine time to land before asserting.
	time.Sleep(100 * time.Millisecond)
	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "prod", Name: wlrcache.Name("Deployment", "api")}
	if err := env.handler.Client.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Containers) != 1 {
		t.Fatalf("stale WLR status was disturbed by admission: %+v", got.Status)
	}
}

// A nodata WLR must NOT trigger a stub request. The object already exists, so
// the Create could only ever return AlreadyExists and the follow-up Get would
// re-read an object that already has everything the webhook could add — wasted
// apiserver calls per admission, for as long as the identity has no history, on
// exactly the high-churn identities that stay nodata longest.
func TestAdmitDoesNotRequestRecommendationWhenNoData(t *testing.T) {
	nodata := freshWLR("Deployment", "prod", "api", nil)
	nodata.Status.Source = sustainv1alpha1.RecommendationSourceNoData

	env := newAdmitEnv(t,
		basicPolicy("p1", sustainv1alpha1.UpdateModeOnCreate),
		deploymentReplicaSet("prod", "api-rs", "api"),
		nodata,
	)

	before := testutil.ToFloat64(RecommendationSourceTotal.WithLabelValues(RecSourceNoData))
	resp := env.handler.admit(context.Background(), admissionRequestFor(t, podWithRSOwner("prod", "api-rs-abc", "api-rs", "p1")))
	if !resp.Allowed {
		t.Fatal("pod must be allowed")
	}
	if len(resp.Patch) != 0 {
		t.Fatalf("a nodata WLR carries nothing to inject, got patch: %s", resp.Patch)
	}
	if got := testutil.ToFloat64(RecommendationSourceTotal.WithLabelValues(RecSourceNoData)); got != before+1 {
		t.Fatalf("nodata source not counted: %v -> %v", before, got)
	}

	// Let any (incorrectly) spawned stub goroutine land before asserting the
	// object was left exactly as it was.
	time.Sleep(100 * time.Millisecond)
	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "prod", Name: wlrcache.Name("Deployment", "api")}
	if err := env.handler.Client.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Source != sustainv1alpha1.RecommendationSourceNoData {
		t.Fatalf("nodata status was disturbed by admission: %+v", got.Status)
	}
}

// A stub must never clobber a populated WLR — that would erase a live
// recommendation and cause the next admission to inject nothing.
func TestCreateStubDoesNotOverwritePopulatedWLR(t *testing.T) {
	existing := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: wlrcache.Name("Job", "etl")},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.Now(),
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{"app": {}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithObjects(existing).WithStatusSubresource(existing).Build()
	h := &Handler{Client: c}

	if err := h.createStub(context.Background(), "prod", "Job", "etl", "p1", nil, nil); err != nil {
		t.Fatal(err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "prod", Name: wlrcache.Name("Job", "etl")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Containers) != 1 {
		t.Fatalf("existing status was clobbered: %+v", got.Status)
	}
}

// countingCreateClient counts Create calls so a test can assert on apiserver
// write volume rather than only on the object that ends up existing.
type countingCreateClient struct {
	client.Client
	mu      sync.Mutex
	creates int
}

func (c *countingCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.mu.Lock()
	c.creates++
	c.mu.Unlock()
	return c.Client.Create(ctx, obj, opts...)
}

func (c *countingCreateClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates
}

// AlreadyExists makes duplicate creates harmless, not free. A stub is invisible
// to the informer until watch propagation, so without dedup a 500-replica
// scale-out issues 500 concurrent creates of one object name — apiserver write
// volume driven by pod churn, worst during the outage that keeps every
// admission classifying as "missing".
//
// The fake client never lags, so this is a LOWER bound on the real duplicate
// count; a real informer's propagation delay makes the burst larger.
func TestRequestRecommendation_DeduplicatesBurstForSameIdentity(t *testing.T) {
	counter := &countingCreateClient{Client: fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()}
	h := &Handler{Client: counter}

	const burst = 200
	var wg sync.WaitGroup
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.requestRecommendation(logr.Discard(), "prod", "Job", "etl", "p1", nil, nil)
		}()
	}
	wg.Wait()

	// Drain the detached creates.
	key := types.NamespacedName{Namespace: "prod", Name: wlrcache.Name("Job", "etl")}
	deadline := time.Now().Add(5 * time.Second)
	var wlr sustainv1alpha1.WorkloadRecommendation
	for time.Now().Before(deadline) {
		if err := h.Client.Get(context.Background(), key, &wlr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := h.Client.Get(context.Background(), key, &wlr); err != nil {
		t.Fatalf("the stub must still be created: %v", err)
	}

	// A small number above 1 is acceptable: goroutines already past the dedup
	// check when the first claim lands still proceed. What must not happen is
	// write volume tracking the burst size.
	if got := counter.count(); got > 5 {
		t.Errorf("%d creates issued for one identity across a %d-pod burst: stub requests must be "+
			"deduplicated per identity, or apiserver writes scale with pod churn", got, burst)
	}
}

// Deduplication must not silently swallow DISTINCT identities — a first Policy
// install legitimately needs a stub for every workload it matches, and losing
// those would leave each one cold-started forever.
func TestRequestRecommendation_DoesNotDropDistinctIdentities(t *testing.T) {
	counter := &countingCreateClient{Client: fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()}
	h := &Handler{Client: counter}

	const identities = 50
	for i := range identities {
		h.requestRecommendation(logr.Discard(), "prod", "Job", fmt.Sprintf("etl-%03d", i), "p1", nil, nil)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if counter.count() >= identities {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := counter.count(); got != identities {
		t.Errorf("created %d stubs for %d distinct identities: dedup and the in-flight bound must "+
			"delay or collapse repeats, never drop a new identity", got, identities)
	}
}

// The stub must record the admitted pod's container set: it is the only
// component that reliably sees an ephemeral identity's containers, and
// computation reads status.observedResources rather than re-resolving the
// workload.
func TestCreateStubRecordsObservedResources(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()
	h := &Handler{Client: c}

	containers := []corev1.Container{{
		Name: "worker",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		},
	}}
	initContainers := []corev1.Container{{Name: "prep"}}

	if err := h.createStub(context.Background(), "ns", "Pod", "dag-task", "pol",
		containers, initContainers); err != nil {
		t.Fatalf("createStub: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	worker, ok := got.Status.ObservedResources["worker"]
	if !ok {
		t.Fatal("worker container missing from observedResources: computation cannot run without it")
	}
	if worker.Init {
		t.Error("worker marked Init")
	}
	if worker.CPURequest == nil || worker.CPURequest.String() != "100m" {
		t.Errorf("worker CPURequest = %v, want 100m", worker.CPURequest)
	}
	prep, ok := got.Status.ObservedResources["prep"]
	if !ok {
		t.Fatal("prep init container missing from observedResources")
	}
	if !prep.Init {
		t.Error("prep not marked Init: ExcludeInitContainers cannot be honoured without this")
	}
	// The stub must still carry no recommendation.
	if len(got.Status.Containers) != 0 {
		t.Errorf("containers = %d, want 0", len(got.Status.Containers))
	}
}

func TestCreateStubFillsSnapshotOnAlreadyExistsForPreExistingEmptyStub(t *testing.T) {
	// Reachable state only: an object that already exists with an empty status
	// and no Source — an older webhook binary's stub, or one whose snapshot
	// patch failed after its Create succeeded. A later admission's Create
	// returns AlreadyExists, and this is its only remaining chance to fill it.
	//
	// A nodata WLR is deliberately not modelled: MarkNoData is only reached
	// after computation found a non-empty snapshot, so that state is
	// unreachable here.
	existing := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      wlrcache.Name("Pod", "dag-task"),
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: "pol"},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{Kind: "Pod", Namespace: "ns", Name: "dag-task"},
			Policy:      "pol",
		},
	}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(existing).Build()
	h := &Handler{Client: c}

	if err := h.createStub(context.Background(), "ns", "Pod", "dag-task", "pol",
		[]corev1.Container{{Name: "worker"}}, nil); err != nil {
		t.Fatalf("createStub: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := got.Status.ObservedResources["worker"]; !ok {
		t.Fatal("AlreadyExists path left the snapshot empty: the identity can never be computed")
	}
}

func TestCreateStubNeverOverwritesPopulatedSnapshot(t *testing.T) {
	// Discovery's snapshot comes from the workload's pod template and is
	// authoritative. The webhook sees one pod.
	q := resource.MustParse("500m")
	existing := &sustainv1alpha1.WorkloadRecommendation{
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
			ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{
				"main": {CPURequest: &q},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(existing).Build()
	h := &Handler{Client: c}

	if err := h.createStub(context.Background(), "ns", "Deployment", "api", "pol",
		[]corev1.Container{{Name: "sidecar-only"}}, nil); err != nil {
		t.Fatalf("createStub: %v", err)
	}

	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Deployment", "api")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.ObservedResources) != 1 {
		t.Fatalf("observedResources has %d entries, want 1: the template snapshot must win",
			len(got.Status.ObservedResources))
	}
	if _, ok := got.Status.ObservedResources["main"]; !ok {
		t.Error("template snapshot was overwritten by the webhook's single-pod view")
	}
}

// laggingCacheReader models the one property fake.NewClientBuilder lacks: the
// webhook's client is informer-backed for WorkloadRecommendation and so is NOT
// read-your-writes — a Get right after a Create races the watch event and
// returns NotFound. warm() models the watch event landing.
type laggingCacheReader struct {
	mu   sync.Mutex
	cold map[string]struct{}
}

func newLaggingCacheReader() *laggingCacheReader {
	return &laggingCacheReader{cold: map[string]struct{}{}}
}

func (l *laggingCacheReader) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := cl.Create(ctx, obj, opts...); err != nil {
				return err
			}
			l.mu.Lock()
			l.cold[obj.GetNamespace()+"/"+obj.GetName()] = struct{}{}
			l.mu.Unlock()
			return nil
		},
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			l.mu.Lock()
			_, cold := l.cold[key.String()]
			l.mu.Unlock()
			if cold {
				return apierrors.NewNotFound(
					schema.GroupResource{Group: sustainv1alpha1.GroupVersion.Group, Resource: "workloadrecommendations"},
					key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}
}

func (l *laggingCacheReader) warm() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cold = map[string]struct{}{}
}

// The production failure this pins: createStub re-read the object it had just
// created, the cache had not seen the watch event, and the Get 404'd — leaving
// an empty status.observedResources, which computation skips, so the identity
// stayed inert until some later admission hit AlreadyExists. Every other
// TestCreateStub* case uses a read-your-writes fake client and cannot express
// this.
func TestCreateStubWritesSnapshotWhenTheCacheLagsBehindTheCreate(t *testing.T) {
	lag := newLaggingCacheReader()
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
		WithInterceptorFuncs(lag.funcs()).Build()
	h := &Handler{Client: c}

	if err := h.createStub(context.Background(), "ns", "Pod", "dag-task", "pol",
		[]corev1.Container{{Name: "worker"}}, nil); err != nil {
		t.Fatalf("createStub must not depend on reading back its own create: %v", err)
	}

	lag.warm()
	var got sustainv1alpha1.WorkloadRecommendation
	key := types.NamespacedName{Namespace: "ns", Name: wlrcache.Name("Pod", "dag-task")}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := got.Status.ObservedResources["worker"]; !ok {
		t.Fatalf("observedResources empty after create: the identity can never be computed, got %+v", got.Status)
	}
}

// A claim can expire while its own owner is still parked on a write slot — the
// dedup TTL and the queue budget are the same 30s — so the goroutine that
// eventually gives up may be dropping a claim that belongs to a LATER
// admission, which has already started its own create. An unconditional delete
// there re-opens the identity for a third admission and lets a second
// concurrent create go out for the same object name.
func TestDropStubClaimDoesNotEvictANewerClaim(t *testing.T) {
	h := &Handler{}
	const key = "prod/job-etl"
	t0 := time.Now()

	_, stale, ok := h.beginStubRequest(key, t0)
	if !ok {
		t.Fatal("the first request for an unclaimed identity must be granted")
	}
	h.stubWG.Done() // no goroutine is started here; keep the counter balanced.

	// The claim expires while its owner is still queued, and the next admission
	// wins a fresh one.
	later := t0.Add(stubRequestDedupTTL + time.Millisecond)
	_, fresh, ok := h.beginStubRequest(key, later)
	if !ok {
		t.Fatal("an expired claim must not keep suppressing the identity")
	}
	h.stubWG.Done()

	// Only now does the first goroutine's queue budget fire.
	h.dropStubClaim(key, stale)

	h.stubMu.Lock()
	until, exists := h.stubRequested[key]
	h.stubMu.Unlock()
	if !exists || !until.Equal(fresh) {
		t.Fatalf("the stale dropper erased a newer claim (exists=%v, until=%v, want %v): the "+
			"admission that owns the in-flight create is left unprotected", exists, until, fresh)
	}
	if _, _, ok := h.beginStubRequest(key, later.Add(time.Millisecond)); ok {
		t.Error("a third admission was granted a claim while the second one's create is still " +
			"in flight: two concurrent creates for one identity, one of them guaranteed to be " +
			"rejected with AlreadyExists")
	}
}

// blockingCreateClient parks inside Create until the test releases it, ignoring
// the context on purpose. It models the window the shutdown ordering exists
// for: a detached stub goroutine sitting in an apiserver call — whose read is
// served by the informer cache cmd/webhook cancels right after the HTTP drain —
// at the moment shutdown begins.
type blockingCreateClient struct {
	client.Client
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func (c *blockingCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.release
	return c.Client.Create(ctx, obj, opts...)
}

// The stub create outlives the AdmissionResponse it came from, so the HTTP
// drain finishing says nothing about whether one is still running. cmd/webhook
// cancels the informer cache the instant serve's drain returns; a stub
// goroutine still reading through that client would then be served by a stopped
// store. Shutdown is what closes that window, so it must not return while one
// is in flight.
func TestHandlerShutdownWaitsForAnInFlightStubWrite(t *testing.T) {
	blocking := &blockingCreateClient{
		Client: fake.NewClientBuilder().WithScheme(config.Scheme()).
			WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := &Handler{Client: blocking}

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocking.release) }) }
	defer release() // never leave the goroutine parked, whatever the test does.

	h.requestRecommendation(logr.Discard(), "prod", "Job", "etl", "p1", nil, nil)
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached stub goroutine never reached its apiserver call")
	}

	done := make(chan error, 1)
	go func() { done <- h.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		t.Fatalf("Shutdown returned (%v) while a stub write was still using the client: "+
			"cmd/webhook cancels the informer cache immediately afterwards, so that write "+
			"would read a stopped store", err)
	case <-time.After(200 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned once the in-flight write completed")
	}
}

// Waiting is only half of it: a stub request parked on a write slot has a 30s
// queue budget, and no shutdown may sit through that. Shutdown cancels the
// parked goroutines instead — the write is best-effort and the next admission
// for the identity asks again, so abandoning it is the right trade.
func TestHandlerShutdownReleasesAStubRequestParkedOnAWriteSlot(t *testing.T) {
	counter := &countingCreateClient{Client: fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()}
	h := &Handler{Client: counter}

	// Fill every write slot so the request below has nowhere to go.
	releases := make([]func(), 0, stubRequestMaxInflight)
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()
	for i := range stubRequestMaxInflight {
		rel, err := h.acquireStubSlot(context.Background())
		if err != nil {
			t.Fatalf("filling write slot %d: %v", i, err)
		}
		releases = append(releases, rel)
	}

	h.requestRecommendation(logr.Discard(), "prod", "Job", "etl", "p1", nil, nil)

	start := time.Now()
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown gave up after %s instead of joining the parked goroutine (%v): it "+
			"must cancel it, not wait out its %s queue budget", time.Since(start),
			err, stubRequestQueueTimeout)
	}
	if elapsed := time.Since(start); elapsed > stubDrainTimeout {
		t.Errorf("Shutdown took %s, over its own %s bound", elapsed, stubDrainTimeout)
	}
	if got := counter.count(); got != 0 {
		t.Errorf("%d creates issued after shutdown began: an abandoned queued request must not "+
			"reach the apiserver at all", got)
	}
}

// Once Shutdown has begun there is nothing left to run a stub write for: the
// informer cache is about to be cancelled, and registering another goroutine
// would be an Add racing a Wait already in progress.
func TestRequestRecommendationIsRefusedAfterShutdown(t *testing.T) {
	counter := &countingCreateClient{Client: fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).Build()}
	h := &Handler{Client: counter}

	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on an idle handler: %v", err)
	}
	h.requestRecommendation(logr.Discard(), "prod", "Job", "etl", "p1", nil, nil)

	// beginStubRequest refuses under stubMu, so "no claim recorded" is a
	// synchronous fact, not a race with a goroutine that may or may not exist.
	h.stubMu.Lock()
	claims := len(h.stubRequested)
	h.stubMu.Unlock()
	if claims != 0 {
		t.Fatalf("%d stub claims recorded after shutdown: a goroutine was registered past the "+
			"point where Shutdown stopped waiting for them", claims)
	}
	if got := counter.count(); got != 0 {
		t.Errorf("%d creates issued after shutdown", got)
	}
}
