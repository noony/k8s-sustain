package webhook

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

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
	stale.Spec.Policy = "p1"
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
	nodata.Spec.Policy = "p1"
	nodata.Status.Outcome = sustainv1alpha1.OutcomeNoData

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
	if got.Status.Outcome != sustainv1alpha1.OutcomeNoData {
		t.Fatalf("nodata status was disturbed by admission: %+v", got.Status)
	}
}

// An undecided object — a stub awaiting its first reconcile, or one discovery
// just created — is already in the controller's work-list. Reading it as
// missing fired a Create per dedup window that could only be rejected with
// AlreadyExists, for as long as the controller had not decided.
func TestAdmitDoesNotRequestRecommendationWhenUndecided(t *testing.T) {
	undecided := freshWLR("Deployment", "prod", "api", nil)
	undecided.Status = sustainv1alpha1.WorkloadRecommendationStatus{
		ObservedResources: map[string]sustainv1alpha1.ObservedContainerResources{"app": {}},
	}
	env := newAdmitEnv(t,
		basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate),
		deploymentReplicaSet("prod", "api-rs", "api"),
		undecided,
	)
	counter := &countingCreateClient{Client: env.handler.Client}
	env.handler.Client = counter

	var resp *admissionv1.AdmissionResponse
	delta := recSourceDelta(t, RecSourceUndecided, func() {
		resp = env.handler.admit(context.Background(), admissionRequestFor(t, podWithRSOwner("prod", "api-rs-abc", "api-rs", "p")))
	})
	if !resp.Allowed || len(resp.Patch) != 0 {
		t.Fatalf("an undecided object carries nothing to inject, got allowed=%v patch=%s", resp.Allowed, resp.Patch)
	}
	if delta != 1 {
		t.Errorf("undecided delta = %v, want 1", delta)
	}

	// Let any (incorrectly) spawned stub goroutine land before asserting.
	time.Sleep(100 * time.Millisecond)
	if got := counter.count(); got != 0 {
		t.Errorf("%d stub creates issued for an object that already exists", got)
	}
}

// A departed identity's snapshot is the controller's only container list for
// it, and the webhook the only component that sees its next run: an admission
// whose pod no longer matches the snapshot replaces it, and still gets the
// retained Recommendation.
func TestAdmitRefreshesTheSnapshotOfADepartedIdentity(t *testing.T) {
	departed := freshWLR("Deployment", "prod", "api", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "128Mi"),
	})
	departed.Status.Departed = true
	departed.Status.ObservedResources = map[string]sustainv1alpha1.ObservedContainerResources{"retired": {}}
	env := newAdmitEnv(t,
		basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate),
		deploymentReplicaSet("prod", "api-rs", "api"),
		departed,
	)

	resp := env.handler.admit(context.Background(), admissionRequestFor(t, podWithRSOwner("prod", "api-rs-abc", "api-rs", "p")))
	if len(resp.Patch) == 0 {
		t.Error("the retained Recommendation must still be injected")
	}

	key := types.NamespacedName{Namespace: "prod", Name: wlrcache.Name("Deployment", "api")}
	deadline := time.Now().Add(5 * time.Second)
	var got sustainv1alpha1.WorkloadRecommendation
	for time.Now().Before(deadline) {
		if err := env.handler.Client.Get(context.Background(), key, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got.Status.ObservedResources["app"]; ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := got.Status.ObservedResources["app"]; !ok || len(got.Status.ObservedResources) != 1 {
		t.Fatalf("snapshot = %v, want it replaced by the admitted pod's", got.Status.ObservedResources)
	}
}

// jobRef is the identity of the standalone Job name in namespace prod.
func jobRef(name string) sustainv1alpha1.WorkloadReference {
	return sustainv1alpha1.WorkloadReference{Kind: "Job", Namespace: "prod", Name: name}
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
			h.requestRecommendation(logr.Discard(), nil, jobRef("etl"), "p1", nil)
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
		h.requestRecommendation(logr.Discard(), nil, jobRef(fmt.Sprintf("etl-%03d", i)), "p1", nil)
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

	h.requestRecommendation(logr.Discard(), nil, jobRef("etl"), "p1", nil)
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

	h.requestRecommendation(logr.Discard(), nil, jobRef("etl"), "p1", nil)

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
	h.requestRecommendation(logr.Discard(), nil, jobRef("etl"), "p1", nil)

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
