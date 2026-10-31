package webhook

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/cache"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// stubWriteTimeout bounds the detached stub create. Admission never waits on
// it; it only stops a wedged apiserver call from pinning a goroutine.
const stubWriteTimeout = 5 * time.Second

// stubRequestDedupTTL is how long one issued stub request suppresses further
// requests for the same identity. Create's AlreadyExists is not enough: the
// object is invisible to the informer until watch propagation, so a scale-out
// would otherwise issue one create per pod. The TTL holds after a failed
// create too, so retry latency stays bounded.
const stubRequestDedupTTL = 30 * time.Second

// stubRequestDedupMaxEntries bounds the dedup like the owner caches, sized for
// the identities admitted within one TTL; past it an evicted claim costs only
// a duplicate create, which AlreadyExists absorbs.
const stubRequestDedupMaxEntries = 4096

// stubRequestMaxInflight bounds concurrent stub creates. Over capacity,
// requests queue rather than drop: a run-once Job that loses the race has no
// guaranteed retry.
const stubRequestMaxInflight = 16

// stubRequestQueueTimeout bounds how long a stub request waits for a write
// slot. Shutdown cancels parked goroutines outright, so it never delays a drain.
const stubRequestQueueTimeout = 30 * time.Second

// stubDrainTimeout bounds Shutdown's wait for the detached stub goroutines.
// They are cancelled first, so this is only unwind time, and blocking longer
// would eat into terminationGracePeriodSeconds.
const stubDrainTimeout = 2 * time.Second

// initStubStateLocked lazily builds the stub state so the zero Handler is
// usable. Caller must hold stubMu.
func (h *Handler) initStubStateLocked() {
	if h.stubClaims == nil {
		h.stubClaims = newStubClaims(nil)
	}
	if h.stubInflight == nil {
		h.stubInflight = make(chan struct{}, stubRequestMaxInflight)
	}
	if h.stubCtx == nil {
		h.stubCtx, h.stubStop = context.WithCancel(context.Background())
	}
}

// newStubClaims returns the stub dedup, one claim per identity key, read on
// clk; a nil clk is the wall clock.
func newStubClaims(clk clock.PassiveClock) *cache.LRU[string, uint64] {
	return cache.NewLRU[string, uint64](stubRequestDedupMaxEntries, stubRequestDedupTTL, clk)
}

// beginStubRequest claims the right to issue a stub create for key and, when
// granted, registers the detached goroutine with stubWG. It returns the parent
// context, the claim, and whether to proceed.
//
// The claim, the shutdown check and the stubWG.Add are one critical section:
// splitting them lets a stub goroutine start after Shutdown concluded there
// were none left.
func (h *Handler) beginStubRequest(key string) (parent context.Context, claim uint64, ok bool) {
	h.stubMu.Lock()
	defer h.stubMu.Unlock()
	h.initStubStateLocked()

	if h.stubStopping {
		return nil, 0, false
	}
	h.stubClaimSeq++
	if !h.stubClaims.SetIfAbsent(key, h.stubClaimSeq) {
		return nil, 0, false
	}
	h.stubWG.Add(1)
	return h.stubCtx, h.stubClaimSeq, true
}

// dropStubClaim forgets a claim that never reached the apiserver so the next
// admission can retry at once. It deletes only this claim: a claim can expire
// while its owner is still queued, and a later admission may already hold a
// fresh claim for the same key.
func (h *Handler) dropStubClaim(key string, claim uint64) {
	h.stubClaims.DeleteIf(key, func(held uint64) bool { return held == claim })
}

// Shutdown stops accepting new stub requests, cancels the detached stub
// goroutines in flight, and waits for them to unwind, bounded by ctx and
// stubDrainTimeout. It must complete before cmd/webhook stops the informer
// cache those goroutines read from. Safe to call more than once.
func (h *Handler) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, stubDrainTimeout)
	defer cancel()

	h.stubMu.Lock()
	h.initStubStateLocked()
	if !h.stubStopping {
		h.stubStopping = true
		h.stubStop()
	}
	h.stubMu.Unlock()

	drained := make(chan struct{})
	go func() {
		h.stubWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquireStubSlot blocks until a write slot is free or ctx expires. The
// blocking receive happens outside stubMu so other admissions are not
// serialised behind it.
func (h *Handler) acquireStubSlot(ctx context.Context) (release func(), err error) {
	h.stubMu.Lock()
	h.initStubStateLocked()
	sem := h.stubInflight
	h.stubMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// stubRequest is the one effect an admission can call for: wlrcache.Request
// for identity, whose object the admission read as known, the pod opting into
// policy and running observed.
type stubRequest struct {
	known    *sustainv1alpha1.WorkloadRecommendation
	identity promclient.WorkloadIdentity
	policy   string
	observed map[string]sustainv1alpha1.ObservedContainerResources
}

// requestRecommendation runs r in a detached goroutine: it creates an
// empty-status stub for an identity that has no object, or refreshes the
// container snapshot of one the controller keeps no live view of. Without it,
// a workload the controller never catches alive (a short Job, a bare-pod
// group) would never enter its work-list, or be computed against its first
// run's containers forever. Errors are logged and dropped; the next pod of the
// same workload retries.
func (h *Handler) requestRecommendation(logger logr.Logger, r stubRequest) {
	key := wlrcache.Key(r.identity).String()
	parent, claim, ok := h.beginStubRequest(key)
	if !ok {
		return
	}
	go func() {
		defer h.stubWG.Done()
		// No recover runs above a detached goroutine: a panic here would end
		// the webhook process.
		defer func() {
			if rec := recover(); rec != nil {
				reportPanic(logger, panicLabelStub, rec, "key", key)
			}
		}()

		queueCtx, queueCancel := context.WithTimeout(parent, stubRequestQueueTimeout)
		defer queueCancel()
		release, err := h.acquireStubSlot(queueCtx)
		if err != nil {
			h.dropStubClaim(key, claim)
			logger.V(1).Info("gave up waiting for a stub write slot", "err", err)
			return
		}
		defer release()

		ctx, cancel := context.WithTimeout(parent, stubWriteTimeout)
		defer cancel()
		ref := wlrcache.WorkloadRef(r.identity)
		if err := wlrcache.Request(log.IntoContext(ctx, logger), h.Client, r.known, ref, r.policy, r.observed); err != nil {
			logger.V(1).Info("failed to request a recommendation", "err", err)
		}
	}()
}
