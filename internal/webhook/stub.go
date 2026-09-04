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

// stubWriteTimeout bounds the detached stub create. Admission never waits on
// it; it only stops a wedged apiserver call from pinning a goroutine.
const stubWriteTimeout = 5 * time.Second

// stubRequestDedupTTL is how long one issued stub request suppresses further
// requests for the same identity. Create's AlreadyExists is not enough: the
// object is invisible to the informer until watch propagation, so a scale-out
// would otherwise issue one create per pod. The TTL holds after a failed
// create too, so retry latency stays bounded.
const stubRequestDedupTTL = 30 * time.Second

// stubRequestMaxInflight bounds concurrent stub creates. Over capacity,
// requests queue rather than drop: a run-once Job that loses the race has no
// guaranteed retry.
const stubRequestMaxInflight = 16

// stubRequestQueueTimeout bounds how long a stub request waits for a write
// slot. Shutdown cancels parked goroutines outright, so it never delays a drain.
const stubRequestQueueTimeout = 30 * time.Second

// stubRequestPruneInterval throttles the lazy sweep of expired dedup entries.
const stubRequestPruneInterval = 5 * time.Minute

// stubDrainTimeout bounds Shutdown's wait for the detached stub goroutines.
// They are cancelled first, so this is only unwind time, and blocking longer
// would eat into terminationGracePeriodSeconds.
const stubDrainTimeout = 2 * time.Second

// initStubStateLocked lazily builds the stub state so the zero Handler is
// usable. Caller must hold stubMu.
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

// beginStubRequest claims the right to issue a stub create for key and, when
// granted, registers the detached goroutine with stubWG. It returns the parent
// context, the dedup expiry written for this claim, and whether to proceed.
//
// The claim, the shutdown check and the stubWG.Add are one critical section:
// splitting them lets a stub goroutine start after Shutdown concluded there
// were none left.
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

// dropStubClaim forgets a claim that never reached the apiserver so the next
// admission can retry at once. It is a compare-and-delete on the expiry: a
// claim can expire while its owner is still queued, and a later admission may
// already hold a fresh claim for the same key.
func (h *Handler) dropStubClaim(key string, claimed time.Time) {
	h.stubMu.Lock()
	defer h.stubMu.Unlock()
	if until, exists := h.stubRequested[key]; exists && until.Equal(claimed) {
		delete(h.stubRequested, key)
	}
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

// pruneStubRequestsLocked drops expired dedup entries. Caller must hold stubMu.
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
// WorkloadRecommendation stub in a detached goroutine. Without it, a workload
// the controller never catches alive (a short Job, a bare-pod group) would
// never enter its work-list. Errors are logged and dropped; the next pod of
// the same workload retries.
func (h *Handler) requestRecommendation(logger logr.Logger, ns, ownerKind, ownerName, policyName string, containers, initContainers []corev1.Container) {
	key := ns + "/" + wlrcache.Name(ownerKind, ownerName)
	parent, claimed, ok := h.beginStubRequest(key, time.Now())
	if !ok {
		return
	}
	go func() {
		defer h.stubWG.Done()

		queueCtx, queueCancel := context.WithTimeout(parent, stubRequestQueueTimeout)
		defer queueCancel()
		release, err := h.acquireStubSlot(queueCtx)
		if err != nil {
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
// status.observedResources when it is still empty.
//
// It is Create, never Update: an existing object may hold a live
// recommendation. The snapshot is written on the AlreadyExists path too,
// because the webhook is the only component that reliably sees an ephemeral
// identity's containers.
func (h *Handler) createStub(ctx context.Context, ns, ownerKind, ownerName, policyName string, containers, initContainers []corev1.Container) error {
	name := wlrcache.Name(ownerKind, ownerName)
	obj := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				sustainv1alpha1.WLRPolicyLabel: policyName,
				sustainv1alpha1.WLRStubLabel:   "true",
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
	// Create cannot carry status, so the snapshot always needs a follow-up patch.
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
		// Client reads of this kind are cache-backed, so a Get right after
		// Create races the watch event and returns NotFound. Patch off obj.
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
