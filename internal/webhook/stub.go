package webhook

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// stubWriteTimeout bounds the detached stub create. It is deliberately
// independent of admissionTimeout: admission never waits on this write, so the
// bound exists only to stop a wedged apiserver call from pinning a goroutine
// indefinitely.
const stubWriteTimeout = 5 * time.Second

// stubRequestDedupTTL is how long one issued stub request suppresses further
// requests for the same identity.
//
// Relying on Create's AlreadyExists to absorb duplicates is not enough. The
// object only becomes visible to this webhook's informer after the create AND
// watch propagation, so within that window every pod of a scaling workload
// still reads "missing" and fires its own create: a 500-replica scale-out
// issues 500 concurrent creates of one object name, 499 rejected. That is
// apiserver write volume driven by pod churn — the exact coupling removing
// Prometheus from admission was meant to eliminate, pointed at a different
// server. It is worst during an outage: while the controller's computation
// phase is failing, ObservedAt stays zero, so the read keeps classifying as
// "missing" and every pod CREATE for that identity writes, for as long as the
// outage lasts.
//
// The TTL holds after a FAILED create too, so a create that errors is not
// retried for up to this long. That is the intended trade: retry latency is
// bounded and small, whereas unbounded retry is what produces the storm.
const stubRequestDedupTTL = 30 * time.Second

// stubRequestMaxInflight bounds CONCURRENT stub creates. Deduplication already
// collapses a scale-out of one workload to a single request, but a first Policy
// install legitimately requests thousands of DISTINCT identities at once; this
// keeps that from hitting the apiserver all at once.
//
// Over capacity, requests QUEUE — they are not dropped. Dropping was the first
// implementation and it was wrong: a distinct identity that loses the race has
// no guaranteed retry, because "the next admission will ask again" assumes the
// workload runs again. A Job that runs once, which is precisely what the stub
// mechanism exists for, would simply never get a recommendation. Queuing costs
// a parked goroutine per pending identity — bounded by workload count, not by
// pod churn, because of the dedup above.
const stubRequestMaxInflight = 16

// stubRequestQueueTimeout bounds how long a stub request waits for a write
// slot before giving up. Generous, because giving up here can lose the request
// entirely for a run-once identity; it exists only so a wedged apiserver cannot
// park goroutines forever. Shutdown does not wait it out: it cancels the parked
// goroutines outright (see Shutdown), so this budget never delays a drain.
const stubRequestQueueTimeout = 30 * time.Second

// stubRequestPruneInterval throttles the sweep that drops expired dedup
// entries. Pruning is lazy — driven by claims — to keep the webhook free of
// background goroutines.
const stubRequestPruneInterval = 5 * time.Minute

// stubDrainTimeout bounds Shutdown's wait for the detached stub goroutines.
// Shutdown cancels them first, so this is only the time they need to unwind an
// already-cancelled context — not the time a write would take. It is short on
// purpose: a stub write is best-effort and the next admission for the identity
// re-requests it, so abandoning one costs a reconcile interval, while blocking
// here eats into terminationGracePeriodSeconds.
const stubDrainTimeout = 2 * time.Second

// initStubStateLocked lazily builds the dedup map, the in-flight semaphore and
// the parent context the detached goroutines derive from. Handler is
// constructed as a plain struct literal (cmd/webhook/serve.go, and every test),
// so the zero value has to be usable. Caller must hold stubMu.
func (h *Handler) initStubStateLocked() {
	if h.stubRequested == nil {
		h.stubRequested = make(map[string]time.Time)
	}
	if h.stubInflight == nil {
		h.stubInflight = make(chan struct{}, stubRequestMaxInflight)
	}
	if h.stubCtx == nil {
		h.stubCtx, h.stubStop = context.WithCancel(context.Background())
	}
}

// beginStubRequest claims the right to issue a stub create for key and, when it
// grants that claim, registers the detached goroutine that will issue it.
//
// It returns the parent context every stub goroutine derives from (cancelled by
// Shutdown), the dedup expiry written for this claim, and whether the caller
// should proceed. False means the identity was already requested within
// stubRequestDedupTTL, or that shutdown has begun; nothing was registered, so
// the caller must not start a goroutine.
//
// The claim, the shutdown check and the stubWG.Add are ONE critical section on
// purpose. Splitting them reintroduces the classic WaitGroup misuse — an Add
// racing a Wait already in progress — which here means a stub goroutine
// starting after Shutdown concluded there were none left, and then reading the
// informer cache that cmd/webhook has since cancelled.
//
// The returned expiry is the token dropStubClaim compares against; see there
// for why an unconditional delete is not enough.
func (h *Handler) beginStubRequest(key string, now time.Time) (parent context.Context, until time.Time, ok bool) {
	h.stubMu.Lock()
	defer h.stubMu.Unlock()
	h.initStubStateLocked()

	if h.stubStopping {
		return nil, time.Time{}, false
	}
	if existing, exists := h.stubRequested[key]; exists && now.Before(existing) {
		return nil, time.Time{}, false
	}
	h.pruneStubRequestsLocked(now)
	until = now.Add(stubRequestDedupTTL)
	h.stubRequested[key] = until
	h.stubWG.Add(1)
	return h.stubCtx, until, true
}

// dropStubClaim forgets a claim so the next admission for that identity can
// retry immediately, instead of being suppressed for the rest of the TTL.
// Used when the request never reached the apiserver at all.
//
// It is a compare-and-delete: claimed is the expiry beginStubRequest wrote for
// this caller, and the entry is removed only if it is still that one. A claim
// can expire while its own owner is still queued — the dedup TTL and the queue
// budget are both 30s — so by the time a giving-up goroutine drops "its" claim,
// a later admission may already have won a fresh claim for the same key and
// started its own create. Deleting unconditionally would erase that fresh claim
// and let a third admission issue a second concurrent create for the same
// identity. createStub's AlreadyExists branch absorbs the duplicate, so this is
// dedup strength rather than correctness — but the point of the claim is not to
// issue the write at all.
func (h *Handler) dropStubClaim(key string, claimed time.Time) {
	h.stubMu.Lock()
	defer h.stubMu.Unlock()
	if until, exists := h.stubRequested[key]; exists && until.Equal(claimed) {
		delete(h.stubRequested, key)
	}
}

// Shutdown stops accepting new stub requests, cancels the detached stub
// goroutines already in flight, and waits for them to unwind. It is safe to
// call more than once and on a Handler that never served anything.
//
// The wait ends at ctx or at stubDrainTimeout, whichever comes first. The
// internal cap is not redundant: it is what makes this call unable to hang a
// shutdown no matter what the caller passes — cmd/webhook reaches here with its
// signal context already cancelled, so it hands this a fresh one.
//
// It exists because those goroutines outlive the admission that spawned them:
// one can be parked for up to stubRequestQueueTimeout (30s) on a write slot, or
// stubWriteTimeout (5s) inside an apiserver call whose read is served by the
// informer cache. cmd/webhook cancels that cache as soon as the HTTP drain
// finishes, so without this join a straggler would read a store that had
// already been stopped. Cancelling first is what keeps the wait short: the
// goroutines abandon the write rather than finishing it, which is the right
// trade because the next admission for the identity requests it again.
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

// acquireStubSlot blocks until a write slot is free or ctx expires. Returns a
// release func on success.
//
// The channel is read under stubMu so it cannot race with lazy initialisation,
// but the blocking receive happens outside the lock — holding it across a wait
// would serialise every other admission behind this one.
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

// pruneStubRequestsLocked drops expired entries so the map does not grow one
// key per identity ever admitted. Caller must hold stubMu.
func (h *Handler) pruneStubRequestsLocked(now time.Time) {
	if now.Sub(h.stubLastPrune) < stubRequestPruneInterval {
		return
	}
	h.stubLastPrune = now
	for k, until := range h.stubRequested {
		if now.After(until) {
			delete(h.stubRequested, k)
		}
	}
}

// requestRecommendation asks the controller to compute a recommendation for a
// workload identity that has none yet, by creating an empty-status
// WorkloadRecommendation "stub". The controller's computation phase is driven
// by the WorkloadRecommendation list, so creating the object is what puts the
// identity into that work-list; the next reconcile fills it in.
//
// This exists because the controller's discovery phase can only create a WLR
// for a workload it catches alive during a reconcile. A Job that runs for
// ninety seconds and is TTL-cleaned, or a briefly-up bare-pod group, may never
// be alive when the reconcile fires — without this path those workloads would
// never enter the work-list at all, and every one of their pods would start on
// template resources forever.
//
// The create runs in a goroutine, on a context detached from the admission's:
// the AdmissionResponse must never block on an apiserver write, and the pod
// this admission is for cannot benefit from the stub anyway (the recommendation
// lands later). Errors are logged and dropped — the next pod of the same
// workload retries implicitly.
//
// Detached from the ADMISSION, not from the process: the goroutine is
// registered with h.stubWG and its context descends from h.stubCtx, so
// Shutdown can cancel and join it before cmd/webhook stops the informer cache
// it reads from.
//
// Requests are deduplicated per identity and bounded in flight, so the write
// volume this generates scales with the number of DISTINCT unrecommended
// identities rather than with pod churn — see stubRequestDedupTTL.
//
// containers and initContainers are the admitted pod's own spec. They are
// threaded through to createStub so it can record the workload's container
// set on the stub — see createStub's doc comment for why that write belongs
// here.
func (h *Handler) requestRecommendation(logger logr.Logger, ns, ownerKind, ownerName, policyName string, containers, initContainers []corev1.Container) {
	key := ns + "/" + wlrcache.Name(ownerKind, ownerName)
	parent, claimed, ok := h.beginStubRequest(key, time.Now())
	if !ok {
		return
	}
	go func() {
		// Registered by beginStubRequest, under the same lock that decides
		// whether shutdown has begun — see Shutdown.
		defer h.stubWG.Done()

		// Both budgets hang off the handler's stub context, not Background:
		// Shutdown cancels it, so neither the queue wait nor the write can
		// outlive the drain and touch a stopped informer cache.
		queueCtx, queueCancel := context.WithTimeout(parent, stubRequestQueueTimeout)
		defer queueCancel()
		release, err := h.acquireStubSlot(queueCtx)
		if err != nil {
			// Never reached the apiserver, so forget the claim rather than
			// suppressing this identity for the rest of the TTL.
			h.dropStubClaim(key, claimed)
			logger.V(1).Info("gave up waiting for a stub write slot", "err", err)
			return
		}
		defer release()

		ctx, cancel := context.WithTimeout(parent, stubWriteTimeout)
		defer cancel()
		if err := h.createStub(log.IntoContext(ctx, logger), ns, ownerKind, ownerName, policyName, containers, initContainers); err != nil {
			logger.V(1).Info("failed to create recommendation stub", "err", err)
		}
	}()
}

// createStub creates the empty-status WorkloadRecommendation, treating
// AlreadyExists as success, and records the admitted pod's container set in
// status.observedResources.
//
// AlreadyExists is treated as success because two admissions can still race
// past the dedup window (the caller's TTL bounds the rate, not the count), and
// because the controller may have created the object first. It is a backstop
// for the rare duplicate, NOT the debouncing mechanism — "one rejected write
// each" is only cheap when duplicates are rare, and pod churn is precisely what
// makes them not rare. The rate limiting lives in beginStubRequest.
//
// Create — never Update — is deliberate. An existing object may hold a live
// recommendation, and overwriting it with an empty status would erase a
// working recommendation and make the next admission inject nothing.
//
// The snapshot write happens on the AlreadyExists path too, not only on
// create: the webhook is the only component that reliably sees an ephemeral
// identity's containers (a completed Job, a between-runs bare-pod group never
// appears in a target listing), so skipping it here would leave some
// snapshot-less objects stuck that way forever. Two real cases land on this
// path: an object created by a webhook binary that predates this change (its
// Create raced past this code entirely, so it has no snapshot and never
// will unless a later admission fills it in here), and an object whose
// snapshot patch below failed after its Create already succeeded (the next
// admission's Create returns AlreadyExists, and this is its only remaining
// chance). A nodata identity is NOT such a case: MarkNoData
// (wlrcache.go) is only ever reached after containersFromObserved found a
// non-empty snapshot, so "nodata" implies the snapshot was already there —
// there is nothing for this path to rescue for it, and in practice
// fetchRecommendations' ErrRecommendationNoData branch in handler.go never
// even calls requestRecommendation for that state.
//
// The write happens only when status.observedResources is still empty:
// discovery's snapshot comes from the workload's pod template and is
// authoritative for a live workload, while the webhook sees a single
// admitted pod, so where both exist the template must win. For the same
// reason this path never touches Containers, Source or ObservedAt — only the
// empty-status object created here is missing that data, and overwriting it
// on a live recommendation would be exactly the destructive write the
// Create-not-Update choice above avoids.
//
// The Get-then-Patch below is not optimistically locked, so two admissions
// racing past the dedup TTL can both read an empty snapshot and both patch.
// That is intentionally left as-is, matching the rest of internal/wlrcache:
// it is harmless here because every writer derives the same snapshot from
// the same admitted pod, so a redundant patch is a no-op in effect, not a
// correctness bug.
//
// A Get is issued ONLY on the AlreadyExists path, never after a successful
// Create. h.Client is cache-backed for WorkloadRecommendation (internal/k8s's
// NewCached caches this kind deliberately, and DisableFor covers the
// owner-chain kinds — ReplicaSet, Job, Deployment, StatefulSet, DaemonSet,
// CronJob and Rollout, see k8s.OwnerChainDisableFor — not
// WorkloadRecommendation), so a read immediately after a create races the
// watch event and reliably returns NotFound. That failure mode was observed in
// production: the create succeeded, the re-read 404'd, the snapshot patch
// never happened, and the identity sat inert with an empty
// status.observedResources until a later admission — at least one dedup TTL
// away, up to a full day for a daily Airflow pod — hit AlreadyExists and
// patched off a by-then-warm cache. Create populates obj in place with the
// server-assigned UID and resourceVersion, so the status patch is issued
// straight off it. On the AlreadyExists path the object was written by
// somebody else and a cache read is the only way to see what it holds.
func (h *Handler) createStub(ctx context.Context, ns, ownerKind, ownerName, policyName string, containers, initContainers []corev1.Container) error {
	name := wlrcache.Name(ownerKind, ownerName)
	obj := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				sustainv1alpha1.WLRPolicyLabel: policyName,
				// Provenance only: marks this object as a cold-start request
				// rather than a controller write, which an empty status alone
				// cannot say (a controller-created WLR is transiently
				// empty-status too). Nothing branches on it — see
				// WLRStubLabel.
				sustainv1alpha1.WLRStubLabel: "true",
			},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{
			WorkloadRef: sustainv1alpha1.WorkloadReference{
				Kind:      ownerKind,
				Namespace: ns,
				Name:      ownerName,
			},
			Policy: policyName,
		},
	}
	// A Create cannot carry status: the status subresource discards it. The
	// snapshot therefore always needs a follow-up patch, and `existing` is the
	// object that patch is computed against.
	var existing sustainv1alpha1.WorkloadRecommendation
	if err := h.Client.Create(ctx, obj); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating recommendation stub %s/%s: %w", ns, obj.Name, err)
		}
		key := client.ObjectKey{Namespace: ns, Name: name}
		if gErr := h.Client.Get(ctx, key, &existing); gErr != nil {
			return fmt.Errorf("reading existing recommendation stub %s/%s: %w", ns, name, gErr)
		}
	} else {
		// Just created by us. obj now carries the server's UID and
		// resourceVersion; re-reading it through the cache would race the watch
		// event and 404. Its status is empty by construction.
		existing = *obj
	}
	if len(existing.Status.ObservedResources) > 0 {
		return nil
	}

	patched := existing.DeepCopy()
	patched.Status.ObservedResources = wlrcache.BuildObservedResources(containers, initContainers)
	if err := h.Client.Status().Patch(ctx, patched, client.MergeFrom(&existing)); err != nil {
		return fmt.Errorf("patching recommendation stub %s/%s observed resources: %w", ns, name, err)
	}
	return nil
}
