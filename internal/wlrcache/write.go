package wlrcache

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// errNotEnsured is Record's answer when there is no object to record into.
var errNotEnsured = errors.New("no WorkloadRecommendation to record into: it was not ensured this cycle")

// ShouldRequest reports whether Request has anything to write for a pod opting
// into policy and running observed, given the identity's object known (nil
// when absent). Besides an absent object, that is a container the pod runs and
// the snapshot lacks, on an object of policy the controller keeps no live view
// of — departed, or never given a snapshot. Only new names count: pods of one
// group may run different container sets, and rewriting the snapshot to each
// pod's view would flip it on every admission. A live identity's snapshot is
// the governing Policy's union of all its members, pruned by Ensure.
func ShouldRequest(known *sustainv1alpha1.WorkloadRecommendation, policy string, observed map[string]sustainv1alpha1.ObservedContainerResources) bool {
	if known == nil {
		return true
	}
	if known.Spec.Policy != policy || len(observed) == 0 {
		return false
	}
	if !known.Status.Departed && len(known.Status.ObservedResources) > 0 {
		return false
	}
	for name := range observed {
		if _, ok := known.Status.ObservedResources[name]; !ok {
			return true
		}
	}
	return false
}

// Request asks for a Recommendation for an identity the webhook admits a pod
// of, the pod opting into policy and running observed. known is the identity's
// object as the webhook read it, nil when it has none. An absent object is
// created as an empty-status stub, labelled as one, carrying the snapshot: the
// webhook is the only component that reliably sees a short-lived identity's
// containers. An existing one gets the snapshot when ShouldRequest says so.
// Request never touches a Recommendation and never Updates: an existing object
// may hold a live one.
func Request(
	ctx context.Context,
	c client.Client,
	known *sustainv1alpha1.WorkloadRecommendation,
	ref sustainv1alpha1.WorkloadReference,
	policy string,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
) error {
	if known != nil {
		if !ShouldRequest(known, policy, observed) {
			return nil
		}
		merged := make(map[string]sustainv1alpha1.ObservedContainerResources, len(known.Status.ObservedResources)+len(observed))
		maps.Copy(merged, observed)
		maps.Copy(merged, known.Status.ObservedResources)
		return writeSnapshot(ctx, c, known, merged)
	}
	obj := newObject(ref, policy)
	obj.Labels[sustainv1alpha1.WLRStubLabel] = "true"
	if err := c.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another writer won the race; the next admission reads its object.
			return nil
		}
		return fmt.Errorf("creating WorkloadRecommendation stub %s: %w", keyOf(ref), err)
	}
	if len(observed) == 0 {
		return nil
	}
	// Create cannot carry status, so the snapshot always needs a follow-up patch.
	return writeSnapshot(ctx, c, obj, observed)
}

func writeSnapshot(
	ctx context.Context,
	c client.Client,
	known *sustainv1alpha1.WorkloadRecommendation,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
) error {
	patched := known.DeepCopy()
	patched.Status.ObservedResources = observed
	if err := c.Status().Patch(ctx, patched, client.MergeFrom(known)); err != nil {
		return fmt.Errorf("writing WorkloadRecommendation %s/%s observed resources: %w", known.Namespace, known.Name, err)
	}
	return nil
}

// Ensure is the governing Policy's claim on a live identity, once per cycle.
// It creates the object, or adopts it (spec.policy and the policy label become
// policy), replaces the observed snapshot when it differs (nil leaves it), and
// clears departed: an identity with a live member is by definition not
// departed. It never touches the Recommendation, the outcome or computedBy:
// an adopted object keeps the previous Policy's numbers, which Read withholds
// until policy records its own.
//
// It returns the object as written, for Record to record into: nothing later
// re-reads it from a cache that may not have seen the write yet.
//
// Only the Policy governing the identity may call it: two callers would flip
// the object between them every cycle.
func Ensure(
	ctx context.Context,
	c client.Client,
	ref sustainv1alpha1.WorkloadReference,
	policy string,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
) (*sustainv1alpha1.WorkloadRecommendation, error) {
	key := keyOf(ref)
	known := &sustainv1alpha1.WorkloadRecommendation{}
	err := c.Get(ctx, key, known)
	switch {
	case apierrors.IsNotFound(err):
		known, err = create(ctx, c, ref, policy)
	case err != nil:
		err = fmt.Errorf("reading WorkloadRecommendation %s: %w", key, err)
	case known.Spec.WorkloadRef != ref || known.Spec.Policy != policy ||
		known.Labels[sustainv1alpha1.WLRPolicyLabel] != policy:
		known, err = claim(ctx, c, known, ref, policy)
	}
	if err != nil {
		return nil, err
	}

	if !known.Status.Departed && (observed == nil || observedEqual(known.Status.ObservedResources, observed)) {
		return known, nil
	}
	patched := known.DeepCopy()
	patched.Status.Departed = false
	if observed != nil {
		patched.Status.ObservedResources = observed
	}
	if err := c.Status().Patch(ctx, patched, client.MergeFrom(known)); err != nil {
		return nil, fmt.Errorf("patching WorkloadRecommendation %s status: %w", key, err)
	}
	return patched, nil
}

func newObject(ref sustainv1alpha1.WorkloadReference, policy string) *sustainv1alpha1.WorkloadRecommendation {
	key := keyOf(ref)
	return &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
			Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: policy},
		},
		Spec: sustainv1alpha1.WorkloadRecommendationSpec{WorkloadRef: ref, Policy: policy},
	}
}

// create creates the object for ref. One that already exists was created by
// another writer (the webhook's stub) moments ago, so the cache may not have
// it: create claims it blind instead of reading it.
func create(
	ctx context.Context,
	c client.Client,
	ref sustainv1alpha1.WorkloadReference,
	policy string,
) (*sustainv1alpha1.WorkloadRecommendation, error) {
	obj := newObject(ref, policy)
	err := c.Create(ctx, obj)
	switch {
	case err == nil:
		return obj, nil
	case apierrors.IsAlreadyExists(err):
		key := keyOf(ref)
		return claim(ctx, c, &sustainv1alpha1.WorkloadRecommendation{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		}, ref, policy)
	default:
		return nil, fmt.Errorf("creating WorkloadRecommendation %s: %w", keyOf(ref), err)
	}
}

// claim points known's spec and policy label at policy. The patch is computed
// against known, which for a blind claim carries nothing but the name, and its
// response is the whole stored object, status included.
func claim(
	ctx context.Context,
	c client.Client,
	known *sustainv1alpha1.WorkloadRecommendation,
	ref sustainv1alpha1.WorkloadReference,
	policy string,
) (*sustainv1alpha1.WorkloadRecommendation, error) {
	patched := known.DeepCopy()
	patched.Spec.WorkloadRef = ref
	patched.Spec.Policy = policy
	if patched.Labels == nil {
		patched.Labels = map[string]string{}
	}
	patched.Labels[sustainv1alpha1.WLRPolicyLabel] = policy
	if err := c.Patch(ctx, patched, client.MergeFrom(known)); err != nil {
		return nil, fmt.Errorf("claiming WorkloadRecommendation %s for policy %s: %w", keyOf(ref), policy, err)
	}
	return patched, nil
}

// Decision is what one pass decided for an identity, as Record stores it.
type Decision struct {
	// Outcome is what the pass decided. Only Computed carries a Recommendation.
	Outcome sustainv1alpha1.RecommendationOutcome
	// Recs is the Recommendation and Traces how it was derived, for Computed.
	Recs   map[string]workload.ContainerRecommendation
	Traces map[string]sustainv1alpha1.ContainerTrace
	// Departed says the identity has no live member left.
	Departed bool
}

// Record stores d into known: the object Ensure returned or, for an identity
// Ensure is not called for (Departed, Conflicted), the one the inventory read.
// It is the one status write per identity per cycle and the only writer of
// departed.
//
// A Computed decision is attributed to the Policy the object names
// (status.computedBy). Every other outcome keeps the last Recommendation, its
// trace, observedAt and computedBy: a departed identity is recomputed until its
// samples age out of the query window, and clearing the Recommendation then
// would strip exactly what retention exists to preserve.
//
// An unchanged decision costs no write, except that a Computed one rewrites
// observedAt once it is RefreshInterval old, so a stable Recommendation never
// reads as stale. A missing object is an error, never a silent no-op: the
// caller must not act as if the decision were stored.
func Record(ctx context.Context, c client.Client, known *sustainv1alpha1.WorkloadRecommendation, d Decision, now time.Time) error {
	if known == nil {
		return errNotEnsured
	}
	desired := *known.Status.DeepCopy()
	desired.Outcome = d.Outcome
	desired.Departed = d.Departed
	computed := d.Outcome == sustainv1alpha1.OutcomeComputed
	if computed {
		// Only the governing Policy records, and Ensure or the departed rule
		// made it the one the object names.
		desired.ComputedBy = known.Spec.Policy
		desired.ObservedAt = metav1.NewTime(now)
		desired.Trace = d.Traces
		desired.Containers = make(map[string]sustainv1alpha1.ContainerRecommendation, len(d.Recs))
		for name, rec := range d.Recs {
			desired.Containers[name] = sustainv1alpha1.ContainerRecommendation(rec)
		}
	}
	if statusEquivalent(known.Status, desired) &&
		(!computed || now.Sub(known.Status.ObservedAt.Time) < RefreshInterval) {
		return nil
	}
	patched := known.DeepCopy()
	patched.Status = desired
	if err := c.Status().Patch(ctx, patched, client.MergeFrom(known)); err != nil {
		return fmt.Errorf("recording %s into WorkloadRecommendation %s/%s: %w", d.Outcome, known.Namespace, known.Name, err)
	}
	return nil
}
