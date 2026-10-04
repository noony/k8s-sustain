package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// discover guarantees a WorkloadRecommendation exists, governed by
// policyName, for every live identity the Policy governs, carrying the
// identity's observed-resources snapshot. It issues no Prometheus queries.
// Only governed identities reach it, so two Policies never rewrite the same
// object's spec.policy back and forth.
//
// Errors are logged and skipped so one unwritable WLR cannot stop the rest of
// the policy, but the count is returned: a persistent cause (missing RBAC, a
// rejecting admission webhook, a namespace quota) would otherwise leave the
// policy reporting success forever while doing nothing. The failure costs only
// the cache write: the identity is still computed and applied this cycle, and
// the retry lands on the next reconcile's EnsureExists.
func (r *PolicyReconciler) discover(ctx context.Context, policyName string, items []computeItem) int {
	logger := log.FromContext(ctx)
	failures := 0
	for _, it := range items {
		if len(it.Targets) == 0 {
			continue
		}
		if err := wlrcache.EnsureExists(ctx, r.Client, it.ref(), policyName, it.Observed); err != nil {
			failures++
			logger.Error(err, "failed to ensure the WorkloadRecommendation is current; "+
				"the identity is still computed and applied this cycle from its members' own container sets, "+
				"but its recommendation may not be cached for the webhook",
				"kind", it.Identity.OwnerKind, "name", it.Identity.OwnerName, "namespace", it.Identity.Namespace)
		}
	}
	return failures
}
