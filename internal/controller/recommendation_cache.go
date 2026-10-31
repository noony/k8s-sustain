package controller

import (
	"context"
	"slices"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

func (r *PolicyReconciler) lifetimes() wlrcache.Lifetimes {
	return wlrcache.Lifetimes{ReconcileInterval: r.ReconcileInterval, Retention: r.RecommendationRetention}
}

// sweep deletes the policy's WorkloadRecommendations that snap says are
// expired at the instant it was taken (wlrcache.Sweep). Best-effort: what it
// leaves is judged again next pass.
func (r *PolicyReconciler) sweep(ctx context.Context, policyName string, snap *inventory.Snapshot) {
	logger := log.FromContext(ctx).WithValues("policy", policyName)
	deleted, err := wlrcache.Sweep(ctx, r.Client, policyName, standingIn(snap), snap.TakenAt, r.lifetimes())
	if err != nil {
		logger.V(1).Info("failed to sweep WorkloadRecommendations", "err", err)
	}
	if deleted > 0 {
		logger.V(1).Info("swept stale WorkloadRecommendations", "deleted", deleted)
	}
}

// standingIn is where each identity stands in snap.
func standingIn(snap *inventory.Snapshot) wlrcache.Standings {
	return func(key promclient.WorkloadIdentity) wlrcache.Standing {
		id, ok := snap.Lookup(key)
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
}

// release deletes the WorkloadRecommendations of policyName, which is being
// deleted (wlrcache.Release). A failure keeps the finalizer, so a retry lists
// and judges afresh.
func (r *PolicyReconciler) release(ctx context.Context, policyName string) error {
	deleted, err := wlrcache.Release(ctx, r.Client, policyName)
	if deleted > 0 {
		log.FromContext(ctx).Info("deleted WorkloadRecommendations for removed policy", "policy", policyName, "deleted", deleted)
	}
	return err
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
		if err := wlrcache.Record(ctx, r.Client, id.Recommendation, decision, time.Now(), r.lifetimes()); err != nil {
			logger.V(1).Info("failed to record the Conflicted outcome", "name", id.Recommendation.Name,
				"namespace", id.Recommendation.Namespace, "err", err)
		}
	}
}
