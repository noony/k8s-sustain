package controller

import (
	"context"
	"slices"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

func (r *PolicyReconciler) lifetimes() wlrcache.Lifetimes {
	return wlrcache.Lifetimes{ReconcileInterval: r.ReconcileInterval, Retention: r.RecommendationRetention}
}

// deleteRecommendations runs del, one of wlrcache's delete paths, and logs
// what it did. Every path judges what it could not delete afresh on its next
// run, so only the release, which must keep its Policy's finalizer meanwhile,
// acts on the error.
func deleteRecommendations(logger logr.Logger, path string, del func() (int, error)) error {
	deleted, err := del()
	if err != nil {
		logger.V(1).Info("deleting WorkloadRecommendations failed", "path", path, "err", err)
	}
	if deleted > 0 {
		logger.Info("deleted WorkloadRecommendations", "path", path, "deleted", deleted)
	}
	return err
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

// recordConflicted records the Conflicted outcome on the stored
// WorkloadRecommendation of every Conflicted identity policyName is party to,
// and nothing else: its spec.policy and Recommendation stay frozen as the
// last governing Policy left them (ADR 0002). A Conflicted identity has live
// members, so the record also clears a departed mark left from before they
// came back. Best-effort.
func (r *PolicyReconciler) recordConflicted(ctx context.Context, policyName string, snap *inventory.Snapshot) {
	logger := log.FromContext(ctx)
	now := r.now()
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
		if err := wlrcache.Record(ctx, r.Client, id.Recommendation, decision, now, r.lifetimes()); err != nil {
			logger.V(1).Info("failed to record the Conflicted outcome", "name", id.Recommendation.Name,
				"namespace", id.Recommendation.Namespace, "err", err)
		}
	}
}
