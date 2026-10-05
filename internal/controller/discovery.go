package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// discover claims, for policyName, the WorkloadRecommendation of every live
// identity on the work-list, carrying the identity's observed-resources
// snapshot, and keeps the object wlrcache.Ensure returns on the item for
// persist to record into. It issues no Prometheus queries. Only governed
// identities reach it, so two Policies never rewrite the same object's
// spec.policy back and forth.
//
// Errors are logged and skipped so one unwritable WLR cannot stop the rest of
// the policy, but the count is returned: a persistent cause (missing RBAC, a
// rejecting admission webhook, a namespace quota) would otherwise leave the
// policy reporting success forever while doing nothing. The identity is still
// computed, from its members' own container sets, but has no object to record
// its decision into; the retry lands on the next reconcile's Ensure.
func (r *PolicyReconciler) discover(ctx context.Context, policyName string, items []computeItem) int {
	logger := log.FromContext(ctx)
	failures := 0
	for i := range items {
		it := &items[i]
		if len(it.Targets) == 0 {
			continue
		}
		known, err := wlrcache.Ensure(ctx, r.Client, it.ref(), policyName, it.Observed)
		it.WLR = known
		if err != nil {
			failures++
			logger.Error(err, "failed to ensure the WorkloadRecommendation is current; "+
				"the identity is still computed this cycle, but its recommendation cannot be stored for the webhook",
				"kind", it.Identity.OwnerKind, "name", it.Identity.OwnerName, "namespace", it.Identity.Namespace)
		}
	}
	return failures
}
