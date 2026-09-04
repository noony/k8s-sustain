package controller

import (
	"context"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
)

// targetIndex maps a workload identity to EVERY live target that reports into
// it. Computation consults it to tell a live identity (has an entry) from a
// departed one.
//
// The value is a slice, not a single target, because owner-name grouping is
// many-to-one: "api-blue" and "api-green" both report as "Deployment/api" and
// share one identity, one WorkloadRecommendation and one Prometheus
// computation — but each owns its own pods and must be recycled or resized
// independently (docs/guides/standalone-pods-and-grouping.md).
type targetIndex map[promclient.WorkloadIdentity][]*workloadTarget

// discover is the first phase of a reconcile: it indexes the listed targets by
// identity and guarantees a WorkloadRecommendation exists for each. It issues
// NO Prometheus queries, which is what lets computation be driven by the WLR
// list instead of this index — an identity is discovered once but computed
// every cycle.
//
// Targets sharing an identity under owner-name grouping collapse to a single
// WLR, written once per IDENTITY from a snapshot merged across the group (see
// mergedObservedResources); all of them stay in the index because the apply
// phase must still reach each one's pods.
//
// Errors are logged and skipped so one unwritable WLR cannot stop the rest of
// the policy, but the count is returned: a persistent cause (missing RBAC, a
// rejecting admission webhook, a namespace quota) would otherwise leave the
// policy reporting success forever while doing nothing. The failure costs only
// the cache write — collectComputeItems reconciles the WLR List against this
// index, so the identity is still computed and applied this cycle. The retry
// lands on the next reconcile's EnsureExists; computeIdentity's follow-up
// Upsert cannot reliably repair it.
func (r *PolicyReconciler) discover(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	targets []workloadTarget,
) (targetIndex, int) {
	logger := log.FromContext(ctx)
	idx := make(targetIndex, len(targets))

	for i := range targets {
		t := &targets[i]
		id := promclient.WorkloadIdentity{
			Namespace: t.Namespace,
			OwnerKind: t.IdentityKind,
			OwnerName: t.IdentityName,
		}
		idx[id] = append(idx[id], t)
	}

	// ONE EnsureExists per identity, not per target: an identity owns exactly
	// one WorkloadRecommendation, so calling it per target made every member of
	// an owner-name group patch status.observedResources back to its own spec
	// on every cycle.
	//
	// Iterated in sorted identity order so a cycle's writes and logs are
	// reproducible; map iteration order is not.
	failures := 0
	for _, id := range sortedIdentities(idx) {
		ref := sustainv1alpha1.WorkloadReference{
			Kind:      id.OwnerKind,
			Namespace: id.Namespace,
			Name:      id.OwnerName,
		}
		if err := wlrcache.EnsureExists(ctx, r.Client, ref, policy.Name,
			mergedObservedResources(idx[id])); err != nil {
			failures++
			logger.Error(err, "failed to ensure the WorkloadRecommendation is current; "+
				"the identity is still computed and applied this cycle from its members' own container sets, "+
				"but its recommendation may not be cached for the webhook",
				"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace)
		}
	}
	return idx, failures
}

// sortedIdentities returns idx's keys in a stable order.
func sortedIdentities(idx targetIndex) []promclient.WorkloadIdentity {
	out := make([]promclient.WorkloadIdentity, 0, len(idx))
	for id := range idx {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].OwnerKind != out[j].OwnerKind {
			return out[i].OwnerKind < out[j].OwnerKind
		}
		return out[i].OwnerName < out[j].OwnerName
	})
	return out
}

// sortedTargets returns a copy of targets ordered by key(), so every decision
// taken across an identity's members — which snapshot entry wins, which
// member's autoscaler shapes the recommendation — depends on the members' own
// names rather than the listing or goroutine-completion order.
func sortedTargets(targets []*workloadTarget) []*workloadTarget {
	ordered := make([]*workloadTarget, len(targets))
	copy(ordered, targets)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key() < ordered[j].key() })
	return ordered
}

// mergedObservedResources builds the ONE observed-resources snapshot an
// identity carries, from every target that reports into it.
//
// Container names are UNIONED across the group: everything downstream reads the
// container set off the identity's single WorkloadRecommendation, so taking one
// member's set would silently drop every container only its siblings declare.
//
// Where two members declare the SAME container name, the whole entry from the
// first member in sorted key() order wins rather than a per-field maximum. One
// entry's requests and limits are read as a pair by recommender.ComputeLimit,
// so mixing fields across members would synthesize a pair no workload ever ran
// with.
//
// Determinism alone is not what stops the members flapping the status; that is
// discovery and the computation phase writing the exact same Observed value for
// the identity (see computeItem.Observed).
func mergedObservedResources(targets []*workloadTarget) map[string]sustainv1alpha1.ObservedContainerResources {
	if len(targets) == 0 {
		return nil
	}
	if len(targets) == 1 {
		return wlrcache.BuildObservedResources(targets[0].Containers, targets[0].InitContainers)
	}

	out := make(map[string]sustainv1alpha1.ObservedContainerResources)
	for _, t := range sortedTargets(targets) {
		for name, obs := range wlrcache.BuildObservedResources(t.Containers, t.InitContainers) {
			if _, seen := out[name]; !seen {
				out[name] = obs
			}
		}
	}
	return out
}
