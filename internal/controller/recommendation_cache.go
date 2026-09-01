package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// sweepGracePeriod protects freshly CREATED WorkloadRecommendations from the
// sweep, regardless of the retention setting. An identity first written just
// after this cycle's target listing was built is absent from that listing
// through no fault of its own, and would otherwise be deleted moments after
// being created — by the sweep at the end of the very same pass. This assumes
// a reconcile pass (target listing → sweep) completes within the grace
// window; a pass longer than 10 minutes could misclassify a mid-pass write as
// opted out. (The controller is currently the only writer; this also covers
// whatever mechanism eventually replaces the webhook's removed
// ephemeral-identity write path, which raced the sweep the same way.)
//
// Anchored on the object's CreationTimestamp, NOT on status.ObservedAt, and
// that distinction is the whole point. Under WLR-driven computation the
// controller recomputes every identity in its own list each cycle — departed
// ones included — so ObservedAt now means "this controller recently ran a
// query", not "something recently observed this workload alive". Anchoring
// grace on it made the guard self-satisfying: phase 2 rewrote ObservedAt,
// then the sweep at the end of the same pass read the timestamp it had just
// written and always concluded the object was fresh. That made
// retainDepartedWLR unreachable for any identity whose samples were still
// flowing, so a workload that opted out (annotation removed, labels changed,
// namespace excluded) kept its WorkloadRecommendation, and its shard
// membership, forever. A CreationTimestamp cannot be refreshed by the writer
// it is meant to be independent of.
const sweepGracePeriod = 10 * time.Minute

// wlrName delegates to the shared cache package — controller and webhook
// must agree on WLR object names or the read contract breaks.
func wlrName(kind, name string) string { return wlrcache.Name(kind, name) }

// wlrLastSeen returns the anchor for RETENTION decisions — how long a
// departed identity's last-known-good has been sitting in the cache. A WLR
// that was just Created but whose status patch hasn't landed yet has a zero
// ObservedAt, so its CreationTimestamp stands in.
//
// Not used for the grace period: see sweepGracePeriod for why that one must
// be anchored on the CreationTimestamp alone.
func wlrLastSeen(wlr *sustainv1alpha1.WorkloadRecommendation) time.Time {
	seen := wlr.Status.ObservedAt.Time
	if wlr.CreationTimestamp.After(wlr.Status.ObservedAt.Time) {
		seen = wlr.CreationTimestamp.Time
	}
	return seen
}

// wlrRefreshInterval bounds how long an unchanged WorkloadRecommendation
// status may keep its old ObservedAt before the controller rewrites it just
// to bump the timestamp. Must stay well under the webhook's
// DefaultCacheStaleness (30m): without the periodic bump, stable workloads
// would freeze ObservedAt at their last value change and the webhook would
// reject the cache as stale and admit pods with template resources —
// exactly the workloads this refresh keeps injectable.
const wlrRefreshInterval = wlrcache.RefreshInterval

// wlrPolicyLabel labels each WorkloadRecommendation with the Policy that
// produced it. Shared via the exported constant in api/v1alpha1.
const wlrPolicyLabel = sustainv1alpha1.WLRPolicyLabel

// upsertWorkloadRecommendation persists an IDENTITY's recommendation via the
// shared write path. Called once per computeItem, never once per group member.
//
// The observed-resources snapshot it writes is the identity's own
// (computeItem.Observed), which is the same value discovery wrote through
// EnsureExists moments earlier — the two writers of that field agree by
// construction, so a converged identity costs no write here at all. Passing a
// single member's snapshot instead is what used to make the members of an
// owner-name group patch the field back and forth forever, each cycle, with the
// winner decided by goroutine scheduling.
//
// The error is returned rather than swallowed. The recycle path treats a failed
// cache write as non-fatal and drops it (the webhook keeps serving the previous
// value until the next successful write); refreshDepartedRecommendation, where
// the write IS the deliverable, must not.
func (r *PolicyReconciler) upsertWorkloadRecommendation(
	ctx context.Context,
	it computeItem,
	policyName string,
	recs map[string]workload.ContainerRecommendation,
	now metav1.Time,
) error {
	ref := sustainv1alpha1.WorkloadReference{
		Kind:      it.Identity.OwnerKind,
		Namespace: it.Identity.Namespace,
		Name:      it.Identity.OwnerName,
	}
	return wlrcache.Upsert(ctx, r.Client, ref, policyName, recs, it.Observed, now)
}

// deleteWLRsWhere lists WorkloadRecommendation objects with the given options
// and deletes every item for which keep returns false.
//
// Every delete is conditioned on the UID and ResourceVersion observed in the
// List, because keep() judges a copy read from the informer cache and that copy
// can be obsolete by the time the Delete lands. With MaxConcurrentReconciles
// > 1, a workload re-annotated from policy P1 to P2 is rewritten by
// Reconcile(P2) — new label, freshly computed recommendation — while
// Reconcile(P1)'s sweep still holds the pre-rewrite copy that says the target
// has left P1's set. Unconditioned, that delete destroys live state and the
// webhook falls back to template resources until P2's next cycle. The
// precondition turns it into a conflict instead.
//
// Two delete outcomes are therefore benign rather than failures, and neither is
// retried here:
//
//   - IsNotFound — the object is already gone, which is the intended end state,
//     so it counts as deleted.
//   - IsConflict — the object changed since it was listed, so the decision to
//     delete it was made against state that no longer exists. It is left alone
//     (not counted as deleted) and re-evaluated on fresh data by the next sweep,
//     with the orphan reaper as the backstop.
//
// Any other delete failure is logged at V(1) via logger and the first such
// error is returned alongside the count of successful deletions. A list failure
// is returned (wrapped) with a zero count so callers can distinguish it from a
// partial delete failure.
//
// This is the shared core of the three WLR cleanup paths (per-cycle sweep,
// per-policy delete, orphan reaper); each wraps it with its own list options,
// keep predicate, error handling and aggregate logging.
func (r *PolicyReconciler) deleteWLRsWhere(
	ctx context.Context,
	logger logr.Logger,
	listOpts []client.ListOption,
	keep func(*sustainv1alpha1.WorkloadRecommendation) bool,
) (deleted int, listErr error, deleteErr error) {
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := r.List(ctx, &list, listOpts...); err != nil {
		return 0, err, nil
	}

	for i := range list.Items {
		wlr := &list.Items[i]
		if keep(wlr) {
			continue
		}
		// Copied out of the listed item: the preconditions must describe the
		// exact revision keep() judged, not whatever the object looks like now.
		uid, resourceVersion := wlr.UID, wlr.ResourceVersion
		err := r.Delete(ctx, wlr, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion})
		switch {
		case err == nil || apierrors.IsNotFound(err):
			deleted++
		case apierrors.IsConflict(err):
			logger.V(1).Info("WorkloadRecommendation changed since it was listed; leaving it to the next sweep",
				"name", wlr.Name, "namespace", wlr.Namespace, "policy", wlr.Spec.Policy)
		default:
			logger.V(1).Info("failed to delete WorkloadRecommendation",
				"name", wlr.Name, "namespace", wlr.Namespace, "policy", wlr.Spec.Policy, "err", err)
			if deleteErr == nil {
				deleteErr = err
			}
		}
	}
	return deleted, nil, deleteErr
}

// sweepWorkloadRecommendations deletes WorkloadRecommendation objects that
// were created by this policy but whose target workload no longer appears in
// the current target set. Called once per Reconcile after a successful pass.
//
// A WLR whose target is absent is not deleted outright: a recent
// CreationTimestamp (within sweepGracePeriod) protects it — this guards
// against an identity first written mid-cycle, after the target list was
// built — and beyond that retainDepartedWLR decides based on whether the
// workload object is actually gone, and if so on RecommendationRetention.
//
// Best-effort: errors are logged, never returned. A missed sweep just leaves
// a stale cache entry until the next cycle.
func (r *PolicyReconciler) sweepWorkloadRecommendations(ctx context.Context, policyName string, targets []workloadTarget) {
	logger := log.FromContext(ctx).WithValues("policy", policyName)

	wanted := make(map[string]struct{}, len(targets))
	for i := range targets {
		t := &targets[i]
		wanted[t.Namespace+"/"+wlrName(t.IdentityKind, t.IdentityName)] = struct{}{}
	}

	now := time.Now()
	deleted, listErr, _ := r.deleteWLRsWhere(ctx, logger,
		[]client.ListOption{client.MatchingLabels{wlrPolicyLabel: policyName}},
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			// Defensive: if the label is stale relative to spec.policy (e.g. on
			// WLRs migrated mid-rename), keep filtering by spec.policy too.
			if wlr.Spec.Policy != policyName {
				return true
			}
			if _, ok := wanted[wlr.Namespace+"/"+wlr.Name]; ok {
				return true
			}
			if now.Sub(wlr.CreationTimestamp.Time) < sweepGracePeriod {
				return true
			}
			return r.retainDepartedWLR(ctx, logger, wlr, now)
		})
	if listErr != nil {
		logger.V(1).Info("failed to list WorkloadRecommendations for sweep", "err", listErr)
		return
	}
	if deleted > 0 {
		logger.V(1).Info("swept stale WorkloadRecommendations", "deleted", deleted)
	}
}

// retainDepartedWLR decides whether a WorkloadRecommendation whose target
// left the policy's target set should be kept.
//
// A target can be absent for three different reasons, and the object's own
// existence is what tells them apart:
//
//   - The workload is GONE (deleted, or a terminal Job): departed. Kept while
//     within the retention window, which is what keeps ephemeral workloads
//     (bare pods, argocd-hook Jobs) on the dashboard after they end.
//   - The workload still EXISTS and is older than the grace period: it opted
//     out — annotation removed, labels changed, namespace excluded. Deleted,
//     and this is the only path that ever removes such an entry.
//   - The workload still EXISTS but was created within the grace period: it
//     simply postdates this cycle's target listing. Kept.
//
// Fails open: an existence-check error keeps the object so a later sweep can
// decide.
//
// Note the retention window is measured from ObservedAt, which the
// computation phase refreshes for as long as the identity's samples remain
// inside the query window. A departed identity is therefore retained for
// roughly (window + retention), not retention alone — see
// docs/reference/cli.md.
func (r *PolicyReconciler) retainDepartedWLR(ctx context.Context, logger logr.Logger, wlr *sustainv1alpha1.WorkloadRecommendation, now time.Time) bool {
	gone, created, err := r.workloadGone(ctx, wlr.Spec.WorkloadRef)
	if err != nil {
		// Deliberately NOT marked departed. This branch keeps the object on an
		// inconclusive check, not a confirmed departure, and the mark waives the
		// webhook's freshness gate — applying it here would let a live workload
		// whose existence check merely errored be served arbitrarily old data.
		logger.V(1).Info("workload existence check failed; keeping WorkloadRecommendation",
			"name", wlr.Name, "namespace", wlr.Namespace, "err", err)
		return true
	}
	if !gone {
		// The workload is still running but is no longer matched: it opted
		// out, and its cache entry goes. Unless the OBJECT is itself younger
		// than the grace period — then it was created after this cycle's
		// target listing was built, and the next listing will pick it up.
		// The WLR's own age cannot answer this: a workload recreated under a
		// name k8s-sustain already knows reuses the existing object.
		return now.Sub(created) < sweepGracePeriod
	}
	// Retention is only consulted for genuinely departed identities. Checked
	// here rather than at the top of the function so that disabling retention
	// cannot short-circuit the opt-out branch above into deleting a workload
	// that merely raced the listing.
	if r.RecommendationRetention <= 0 {
		return false
	}
	if now.Sub(wlrLastSeen(wlr)) > r.RecommendationRetention {
		return false
	}
	r.markDeparted(ctx, logger, wlr)
	return true
}

// markDeparted records that this recommendation is being retained for a
// workload identity confirmed gone, so the webhook serves it instead of
// rejecting it as stale — see WorkloadRecommendationStatus.Departed for why
// that distinction exists at all.
//
// Idempotent by design: the sweep runs every reconcile, but the patch is
// issued only on the transition, so a departed identity costs one write for
// its whole retention window rather than one per cycle.
//
// Best-effort, like the rest of the sweep. A failed patch leaves the object
// unmarked and the next sweep retries; the only consequence in between is that
// the webhook keeps treating it as stale, which is the pre-existing behaviour.
func (r *PolicyReconciler) markDeparted(ctx context.Context, logger logr.Logger, wlr *sustainv1alpha1.WorkloadRecommendation) {
	if wlr.Status.Departed {
		return
	}
	patched := wlr.DeepCopy()
	patched.Status.Departed = true
	if err := r.Status().Patch(ctx, patched, client.MergeFrom(wlr)); err != nil {
		logger.V(1).Info("failed to mark WorkloadRecommendation departed; will retry next sweep",
			"name", wlr.Name, "namespace", wlr.Namespace, "err", err)
		return
	}
	logger.V(1).Info("retaining recommendation for departed workload",
		"name", wlr.Name, "namespace", wlr.Namespace)
}

// workloadGone reports whether the referenced workload object no longer
// exists. Jobs in a terminal state count as gone: they leave the target set
// while the object lingers until TTL/hook deletion, and that is not an
// opt-out. Bare-pod identities (kind "Pod") always count as gone — the ref
// name is the owner-name annotation value, not a real object name, so
// existence cannot be checked with a GET. Owner-name-overridden identities
// on other kinds hit the same limit and resolve to NotFound → gone.
//
// The second return is the surviving object's CreationTimestamp, zero when
// the object is gone. retainDepartedWLR needs it to tell the two reasons a
// live workload can be absent from the target set apart: it opted out, or it
// was created after this cycle's listing was built. Only the object's own age
// separates them, and nothing on the WLR can stand in for it.
func (r *PolicyReconciler) workloadGone(
	ctx context.Context,
	ref sustainv1alpha1.WorkloadReference,
) (bool, time.Time, error) {
	var obj client.Object
	switch ref.Kind {
	case "Deployment":
		obj = &appsv1.Deployment{}
	case "StatefulSet":
		obj = &appsv1.StatefulSet{}
	case "DaemonSet":
		obj = &appsv1.DaemonSet{}
	case "Rollout":
		obj = &rolloutsv1alpha1.Rollout{}
	case "CronJob":
		obj = &batchv1.CronJob{}
	case "Job":
		obj = &batchv1.Job{}
	default: // "Pod" (bare-pod identity) or unknown future kind
		return true, time.Time{}, nil
	}
	err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, obj)
	if apierrors.IsNotFound(err) {
		return true, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	if job, ok := obj.(*batchv1.Job); ok && jobIsTerminal(job) {
		return true, time.Time{}, nil
	}
	return false, obj.GetCreationTimestamp().Time, nil
}

// deleteAllRecommendationsForPolicy removes every WorkloadRecommendation tied
// to the named policy. Called from the deletion branch of Reconcile before
// the cleanup finalizer is dropped — guarantees the cache doesn't outlive
// the parent Policy on the normal `kubectl delete policy` path.
//
// Returns an error so the finalizer is only removed once cleanup finishes;
// otherwise a transient API failure mid-delete would orphan WLRs.
func (r *PolicyReconciler) deleteAllRecommendationsForPolicy(ctx context.Context, policyName string) error {
	logger := log.FromContext(ctx).WithValues("policy", policyName)

	deleted, listErr, deleteErr := r.deleteWLRsWhere(ctx, logger,
		[]client.ListOption{client.MatchingLabels{wlrPolicyLabel: policyName}},
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			// Defensive: belt-and-braces against label drift on legacy WLRs.
			return wlr.Spec.Policy != policyName
		})
	if listErr != nil {
		return fmt.Errorf("listing WorkloadRecommendations for policy delete: %w", listErr)
	}
	if deleted > 0 {
		logger.Info("deleted WorkloadRecommendations for removed policy", "deleted", deleted)
	}
	return deleteErr
}

// reapOrphanedRecommendations is the belt-and-braces sweeper: it lists every
// WorkloadRecommendation in the cluster and deletes any whose spec.policy
// does not reference an existing Policy. Catches:
//   - WLRs left behind by `kubectl delete policy --grace-period=0 --force`
//     (which skips finalizers entirely).
//   - WLRs from a controller crash mid-delete.
//   - WLRs orphaned by a Policy renamed before the per-policy sweep ran.
//
// It no longer ages out "nodata" recommendations. Under WLR-driven refresh a
// nodata status means "nothing computed YET" and is retried on every cycle, so
// there is no terminal state to collect. An identity that never materialises is
// removed by the per-policy sweep once its target is absent and the retention
// window lapses.
//
// Best-effort and idempotent. Safe to run on a tick.
func (r *PolicyReconciler) reapOrphanedRecommendations(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("orphan-reaper")

	var policies sustainv1alpha1.PolicyList
	if err := r.List(ctx, &policies); err != nil {
		return fmt.Errorf("listing policies: %w", err)
	}
	known := make(map[string]struct{}, len(policies.Items))
	for i := range policies.Items {
		known[policies.Items[i].Name] = struct{}{}
	}

	deleted, listErr, _ := r.deleteWLRsWhere(ctx, logger, nil,
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			if wlr.Spec.Policy == "" {
				// Untracked entry — leave it; some other writer may own it.
				return true
			}
			_, ok := known[wlr.Spec.Policy]
			return ok
		})
	if listErr != nil {
		return fmt.Errorf("listing workloadrecommendations: %w", listErr)
	}
	if deleted > 0 {
		logger.Info("reaped orphan WorkloadRecommendations", "deleted", deleted)
	}
	return nil
}
