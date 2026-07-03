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

// sweepGracePeriod protects recently written WorkloadRecommendations from
// the sweep, regardless of the retention setting. The webhook writes WLRs at
// pod admission; a pod created after this cycle's target listing would
// otherwise look departed (or, for a Job object that still exists, opted
// out) and be deleted moments after being written. A fresh ObservedAt proves
// something just observed the workload. Matches wlrRefreshInterval so a live
// workload's periodic ObservedAt bump keeps it covered between reconciles.
// This assumes a reconcile pass (target listing → sweep) completes within the
// grace window; a pass longer than 10 minutes could misclassify a mid-pass
// admission as opted out.
const sweepGracePeriod = 10 * time.Minute

// wlrName delegates to the shared cache package — controller and webhook
// must agree on WLR object names or the fallback contract breaks.
func wlrName(kind, name string) string { return wlrcache.Name(kind, name) }

// wlrLastSeen returns the freshness anchor for sweep decisions. A WLR that
// was just Created but whose status patch hasn't landed yet has a zero
// ObservedAt — its CreationTimestamp proves the write is in flight, and the
// grace period must cover that window or the webhook's two-step write races
// the sweep.
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
// reject the cache as stale — exactly the workloads the fallback protects.
const wlrRefreshInterval = wlrcache.RefreshInterval

// wlrPolicyLabel labels each WorkloadRecommendation with the Policy that
// produced it. Shared via the exported constant in api/v1alpha1.
const wlrPolicyLabel = sustainv1alpha1.WLRPolicyLabel

// upsertWorkloadRecommendation persists the recommendation via the shared
// write path, snapshotting the target's current container resources.
//
// Errors are logged but never propagated up — the cache is a fallback path,
// not load-bearing. A failed write only means the webhook may serve a slightly
// older cached value during the next Prometheus outage.
func (r *PolicyReconciler) upsertWorkloadRecommendation(
	ctx context.Context,
	t *workloadTarget,
	policyName string,
	recs map[string]workload.ContainerRecommendation,
	now metav1.Time,
) {
	wlrcache.Upsert(ctx, r.Client,
		sustainv1alpha1.WorkloadReference{Kind: t.IdentityKind, Namespace: t.Namespace, Name: t.IdentityName},
		policyName, recs,
		wlrcache.BuildObservedResources(t.Containers, t.InitContainers),
		now)
}

// deleteWLRsWhere lists WorkloadRecommendation objects with the given options
// and deletes every item for which keep returns false. IsNotFound on delete is
// tolerated (the object is counted as deleted). Each delete failure is logged
// at V(1) via logger and the first such error is returned alongside the count
// of successful deletions. A list failure is returned (wrapped) with a zero
// count so callers can distinguish it from a partial delete failure.
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
		if err := r.Delete(ctx, wlr); err != nil && !apierrors.IsNotFound(err) {
			logger.V(1).Info("failed to delete WorkloadRecommendation",
				"name", wlr.Name, "namespace", wlr.Namespace, "policy", wlr.Spec.Policy, "err", err)
			if deleteErr == nil {
				deleteErr = err
			}
			continue
		}
		deleted++
	}
	return deleted, nil, deleteErr
}

// sweepWorkloadRecommendations deletes WorkloadRecommendation objects that
// were created by this policy but whose target workload no longer appears in
// the current target set. Called once per Reconcile after a successful pass.
//
// A departed WLR is not deleted outright: a fresh ObservedAt (within
// sweepGracePeriod) always protects it — this guards against a pod written
// mid-cycle by the webhook after the target list was built — and beyond that
// retainDepartedWLR decides based on RecommendationRetention and whether the
// workload object itself is actually gone.
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
			if now.Sub(wlrLastSeen(wlr)) < sweepGracePeriod {
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
// left the policy's target set should be kept. Kept when the workload object
// is gone (or a terminal Job) and the recommendation is within the retention
// window — this keeps ephemeral workloads (bare pods, argocd-hook Jobs)
// visible on the dashboard after they end. Deleted when retention is
// disabled, when the workload still exists (it opted out), or when the
// window has lapsed. Fails open: an existence-check error keeps the object
// so a later sweep can decide.
func (r *PolicyReconciler) retainDepartedWLR(ctx context.Context, logger logr.Logger, wlr *sustainv1alpha1.WorkloadRecommendation, now time.Time) bool {
	if r.RecommendationRetention <= 0 {
		return false
	}
	gone, err := r.workloadGone(ctx, wlr.Spec.WorkloadRef)
	if err != nil {
		logger.V(1).Info("workload existence check failed; keeping WorkloadRecommendation",
			"name", wlr.Name, "namespace", wlr.Namespace, "err", err)
		return true
	}
	if !gone {
		return false
	}
	return now.Sub(wlrLastSeen(wlr)) <= r.RecommendationRetention
}

// workloadGone reports whether the referenced workload object no longer
// exists. Jobs in a terminal state count as gone: they leave the target set
// while the object lingers until TTL/hook deletion, and that is not an
// opt-out. Bare-pod identities (kind "Pod") always count as gone — the ref
// name is the owner-name annotation value, not a real object name, so
// existence cannot be checked with a GET. Owner-name-overridden identities
// on other kinds hit the same limit and resolve to NotFound → gone.
func (r *PolicyReconciler) workloadGone(ctx context.Context, ref sustainv1alpha1.WorkloadReference) (bool, error) {
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
		return true, nil
	}
	err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, obj)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if job, ok := obj.(*batchv1.Job); ok && jobIsTerminal(job) {
		return true, nil
	}
	return false, nil
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
