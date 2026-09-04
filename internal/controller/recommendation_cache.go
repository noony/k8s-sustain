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

// sweepGracePeriod protects freshly created WorkloadRecommendations from the
// sweep: an identity first written after this cycle's target listing was
// built would otherwise be deleted by the same pass. Anchored on
// CreationTimestamp, not ObservedAt, which the computation phase rewrites
// every cycle and would make the guard self-satisfying.
const sweepGracePeriod = 10 * time.Minute

// wlrName delegates to the shared cache package so controller and webhook
// agree on names.
func wlrName(kind, name string) string { return wlrcache.Name(kind, name) }

// wlrLastSeen returns the retention anchor: ObservedAt, or CreationTimestamp
// when the status patch has not landed yet.
func wlrLastSeen(wlr *sustainv1alpha1.WorkloadRecommendation) time.Time {
	seen := wlr.Status.ObservedAt.Time
	if wlr.CreationTimestamp.After(wlr.Status.ObservedAt.Time) {
		seen = wlr.CreationTimestamp.Time
	}
	return seen
}

// wlrRefreshInterval bounds how long an unchanged status keeps its old
// ObservedAt. Must stay well under the webhook's DefaultCacheStaleness or
// stable workloads are rejected as stale.
const wlrRefreshInterval = wlrcache.RefreshInterval

// wlrPolicyLabel labels each WorkloadRecommendation with its Policy.
const wlrPolicyLabel = sustainv1alpha1.WLRPolicyLabel

// upsertWorkloadRecommendation persists an identity's recommendation. Called
// once per computeItem; it writes the identity's own snapshot so the two
// writers of status.observedResources agree.
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

// wlrDeleteGuard says how strongly a cleanup path conditions its deletes, and
// what a conflict means on that path.
type wlrDeleteGuard int

const (
	// deleteIfUnchanged conditions on UID and ResourceVersion, for decisions
	// that depend on mutable contents. A conflict is benign: the object is
	// re-judged next pass.
	deleteIfUnchanged wlrDeleteGuard = iota

	// deleteIfSameObject conditions on the UID alone, for decisions that do not
	// depend on the revision at all — "this object belongs to the policy being
	// deleted" stays true however often it is rewritten. A conflict is
	// unexpected and returned.
	deleteIfSameObject
)

// deleteWLRsWhere lists WorkloadRecommendations and deletes those keep
// rejects. NotFound counts as deleted; Conflict is benign only under
// deleteIfUnchanged. A list failure returns a zero count.
func (r *PolicyReconciler) deleteWLRsWhere(
	ctx context.Context,
	logger logr.Logger,
	guard wlrDeleteGuard,
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
		// Preconditions must describe the object keep() judged.
		uid := wlr.UID
		preconditions := client.Preconditions{UID: &uid}
		if guard == deleteIfUnchanged {
			resourceVersion := wlr.ResourceVersion
			preconditions.ResourceVersion = &resourceVersion
		}
		err := r.Delete(ctx, wlr, preconditions)
		switch {
		case err == nil || apierrors.IsNotFound(err):
			deleted++
		case apierrors.IsConflict(err) && guard == deleteIfUnchanged:
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

// sweepWorkloadRecommendations deletes this policy's WorkloadRecommendations
// whose target is absent from the current set, subject to the grace period
// and retainDepartedWLR. Best-effort.
func (r *PolicyReconciler) sweepWorkloadRecommendations(ctx context.Context, policyName string, targets []workloadTarget) {
	logger := log.FromContext(ctx).WithValues("policy", policyName)

	wanted := make(map[string]struct{}, len(targets))
	for i := range targets {
		t := &targets[i]
		wanted[t.Namespace+"/"+wlrName(t.IdentityKind, t.IdentityName)] = struct{}{}
	}

	now := time.Now()
	deleted, listErr, _ := r.deleteWLRsWhere(ctx, logger, deleteIfUnchanged,
		[]client.ListOption{client.MatchingLabels{wlrPolicyLabel: policyName}},
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			// Guard against a label stale relative to spec.policy.
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

// retainDepartedWLR decides whether a WorkloadRecommendation whose target left
// the target set is kept: a gone workload is retained for the retention
// window, an existing one older than the grace period has opted out and is
// deleted, and a check error fails open.
func (r *PolicyReconciler) retainDepartedWLR(ctx context.Context, logger logr.Logger, wlr *sustainv1alpha1.WorkloadRecommendation, now time.Time) bool {
	gone, created, err := r.workloadGone(ctx, wlr.Spec.WorkloadRef)
	if err != nil {
		// Not marked departed: the check was inconclusive, and the mark waives the
		// webhook's freshness gate.
		logger.V(1).Info("workload existence check failed; keeping WorkloadRecommendation",
			"name", wlr.Name, "namespace", wlr.Namespace, "err", err)
		return true
	}
	if !gone {
		// Still running but unmatched: opted out, unless the object postdates this
		// cycle's listing.
		return now.Sub(created) < sweepGracePeriod
	}
	// Checked after the opt-out branch so disabling retention cannot delete a
	// workload that merely raced the listing.
	if r.RecommendationRetention <= 0 {
		return false
	}
	if now.Sub(wlrLastSeen(wlr)) > r.RecommendationRetention {
		return false
	}
	r.markDeparted(ctx, logger, wlr)
	return true
}

// markDeparted flags a retained recommendation as departed so the webhook
// serves it instead of rejecting it as stale. Patched only on the
// transition; best-effort.
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
// exists, and its CreationTimestamp when it does. Terminal Jobs and bare-pod
// identities count as gone.
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

// deleteAllRecommendationsForPolicy removes every WorkloadRecommendation for
// the policy before the finalizer is dropped. Uses deleteIfSameObject and
// returns conflicts so the finalizer only goes once cleanup finished.
func (r *PolicyReconciler) deleteAllRecommendationsForPolicy(ctx context.Context, policyName string) error {
	logger := log.FromContext(ctx).WithValues("policy", policyName)

	deleted, listErr, deleteErr := r.deleteWLRsWhere(ctx, logger, deleteIfSameObject,
		[]client.ListOption{client.MatchingLabels{wlrPolicyLabel: policyName}},
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
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

// reapOrphanedRecommendations deletes every WorkloadRecommendation whose
// spec.policy names no existing Policy, catching force deletes and crashes
// mid-delete. Runs on a tick with the deleteIfUnchanged guard, since a stale
// copy can call a just-adopted object an orphan.
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

	deleted, listErr, _ := r.deleteWLRsWhere(ctx, logger, deleteIfUnchanged, nil,
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			if wlr.Spec.Policy == "" {
				// Untracked entry; some other writer may own it.
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
