package webhook

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// decide only calls for the stub request; admit must run it, or a workload the
// controller never catches alive (a short-lived Job, a bare-pod group) would
// start every one of its pods on template resources forever.
func TestAdmitRunsTheStubRequestItDecided(t *testing.T) {
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

type panickingCreateClient struct{ client.Client }

func (panickingCreateClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	panic("boom")
}

// The stub write runs on a detached goroutine, past every recover the
// request path has: unguarded, one panic there ends the webhook process.
func TestRequestRecommendationRecoversAPanickingWrite(t *testing.T) {
	h := &Handler{Client: panickingCreateClient{Client: fake.NewClientBuilder().WithScheme(config.Scheme()).Build()}}
	defer func() {
		if err := h.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()

	before := testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelStub))
	h.requestRecommendation(logr.Discard(), stubRequest{identity: jobIdentity("etl"), policy: "p1"})

	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelStub)) == before {
		if time.Now().After(deadline) {
			t.Fatal("the stub goroutine's panic was never counted")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// jobIdentity is the identity of the standalone Job name in namespace prod.
func jobIdentity(name string) promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: "prod", OwnerKind: "Job", OwnerName: name}
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
			h.requestRecommendation(logr.Discard(), stubRequest{identity: jobIdentity("etl"), policy: "p1"})
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
		h.requestRecommendation(logr.Discard(), stubRequest{identity: jobIdentity(fmt.Sprintf("etl-%03d", i)), policy: "p1"})
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
	t0 := time.Now()
	clk := clocktesting.NewFakePassiveClock(t0)
	h := &Handler{stubClaims: newStubClaims(clk)}
	const key = "prod/job-etl"

	_, stale, ok := h.beginStubRequest(key)
	if !ok {
		t.Fatal("the first request for an unclaimed identity must be granted")
	}
	h.stubWG.Done() // no goroutine is started here; keep the counter balanced.

	// The claim expires while its owner is still queued, and the next admission
	// wins a fresh one.
	clk.SetTime(t0.Add(stubRequestDedupTTL + time.Millisecond))
	if _, _, ok := h.beginStubRequest(key); !ok {
		t.Fatal("an expired claim must not keep suppressing the identity")
	}
	h.stubWG.Done()

	// Only now does the first goroutine's queue budget fire.
	h.dropStubClaim(key, stale)

	if _, _, ok := h.beginStubRequest(key); ok {
		t.Error("the stale dropper erased a newer claim: a third admission was granted one while " +
			"the second one's create is still in flight, so two concurrent creates go out for one " +
			"identity, one of them guaranteed to be rejected with AlreadyExists")
	}
}

type failingCreateClient struct{ countingCreateClient }

func (c *failingCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	_ = c.countingCreateClient.Create(ctx, obj, opts...)
	return errors.New("apiserver unavailable")
}

// Only a claim that never reached the apiserver is dropped. A failed create
// keeps its claim for the TTL, so an outage costs one create per identity per
// TTL, not one per admission.
func TestRequestRecommendationKeepsTheClaimAfterAFailedCreate(t *testing.T) {
	failing := &failingCreateClient{countingCreateClient{Client: fake.NewClientBuilder().WithScheme(config.Scheme()).Build()}}
	h := &Handler{Client: failing}
	r := stubRequest{identity: jobIdentity("etl"), policy: "p1"}

	h.requestRecommendation(logr.Discard(), r)
	h.stubWG.Wait()
	if got := failing.count(); got != 1 {
		t.Fatalf("%d creates issued for one request, want 1", got)
	}

	h.requestRecommendation(logr.Discard(), r)
	h.stubWG.Wait()
	if got := failing.count(); got != 1 {
		t.Errorf("%d creates issued: a second admission within the TTL retried a failed create", got)
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

	h.requestRecommendation(logr.Discard(), stubRequest{identity: jobIdentity("etl"), policy: "p1"})
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

	h.requestRecommendation(logr.Discard(), stubRequest{identity: jobIdentity("etl"), policy: "p1"})

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
	h.requestRecommendation(logr.Discard(), stubRequest{identity: jobIdentity("etl"), policy: "p1"})

	// beginStubRequest refuses under stubMu, so "no claim recorded" is a
	// synchronous fact, not a race with a goroutine that may or may not exist.
	if _, claimed := h.stubClaims.Get(wlrcache.Key(jobIdentity("etl")).String()); claimed {
		t.Fatal("a stub claim was recorded after shutdown: a goroutine was registered past the " +
			"point where Shutdown stopped waiting for them")
	}
	if got := counter.count(); got != 0 {
		t.Errorf("%d creates issued after shutdown", got)
	}
}
