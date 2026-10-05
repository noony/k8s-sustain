package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/go-logr/logr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// wlrPolicyLabel labels each WorkloadRecommendation with its Policy.
const wlrPolicyLabel = sustainv1alpha1.WLRPolicyLabel

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

// sweepWorkloadRecommendations deletes the policy's WorkloadRecommendations
// wlrcache.Expired judges expired against snap: one whose identity the policy
// no longer governs (its members opted out, or it fell out of the Policy's
// namespaces or kinds), or a Departed or Conflicted identity's past the
// retention window. One now governed by another Policy is left for that
// Policy to adopt. Best-effort.
func (r *PolicyReconciler) sweepWorkloadRecommendations(ctx context.Context, policyName string, snap *inventory.Snapshot) {
	logger := log.FromContext(ctx).WithValues("policy", policyName)

	now := time.Now()
	deleted, listErr, _ := r.deleteWLRsWhere(ctx, logger, deleteIfUnchanged,
		[]client.ListOption{client.MatchingLabels{wlrPolicyLabel: policyName}},
		func(wlr *sustainv1alpha1.WorkloadRecommendation) bool {
			return !wlrcache.Expired(wlr, policyName, standing(snap, wlr), now, r.RecommendationRetention)
		})
	if listErr != nil {
		logger.V(1).Info("failed to list WorkloadRecommendations for sweep", "err", listErr)
		return
	}
	if deleted > 0 {
		logger.V(1).Info("swept stale WorkloadRecommendations", "deleted", deleted)
	}
}

// standing is where wlr's identity stands in snap.
func standing(snap *inventory.Snapshot, wlr *sustainv1alpha1.WorkloadRecommendation) wlrcache.Standing {
	ref := wlr.Spec.WorkloadRef
	id, ok := snap.Lookup(promclient.WorkloadIdentity{Namespace: ref.Namespace, OwnerKind: ref.Kind, OwnerName: ref.Name})
	switch {
	case !ok:
		return wlrcache.Ungoverned
	case id.Departed():
		return wlrcache.Departed
	case id.Conflicted:
		return wlrcache.Conflicted
	case id.Policy != "":
		return wlrcache.Governed
	default:
		return wlrcache.Ungoverned
	}
}

// recordConflicted records the Conflicted outcome on the stored
// WorkloadRecommendation of every Conflicted identity policyName is party to,
// and nothing else: its spec.policy and Recommendation stay frozen as the
// last governing Policy left them (ADR 0002). A Conflicted identity has live
// members, so the record also clears a departed mark left from before they
// came back. Best-effort.
func (r *PolicyReconciler) recordConflicted(ctx context.Context, policyName string, snap *inventory.Snapshot) {
	logger := log.FromContext(ctx)
	for i := range snap.Identities {
		id := &snap.Identities[i]
		policies := id.MemberPolicies()
		if !id.Conflicted || !slices.Contains(policies, policyName) {
			continue
		}
		logger.Info("identity is Conflicted: its members opt into different Policies, so none governs it "+
			"and its recommendation stays frozen until they agree",
			"kind", id.Key.OwnerKind, "name", id.Key.OwnerName, "namespace", id.Key.Namespace,
			"policies", policies)
		if id.Recommendation == nil {
			continue
		}
		decision := wlrcache.Decision{Outcome: sustainv1alpha1.OutcomeConflicted}
		if err := wlrcache.Record(ctx, r.Client, id.Recommendation, decision, time.Now()); err != nil {
			logger.V(1).Info("failed to record the Conflicted outcome", "name", id.Recommendation.Name,
				"namespace", id.Recommendation.Namespace, "err", err)
		}
	}
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
