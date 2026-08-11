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
// it. Computation consults it to tell a live identity from a departed one: an
// identity with no entry has no workload object in the listing, which is
// exactly the population that used to be uncomputable.
//
// The value is a slice, not a single target, because owner-name grouping is
// many-to-one: Deployments "api-blue" and "api-green" both report as
// "Deployment/api". They share one identity, one WorkloadRecommendation and
// one Prometheus computation — but each owns its own pods and must be recycled
// or resized independently, which is the guarantee documented in
// docs/guides/standalone-pods-and-grouping.md. Collapsing to a single target
// here would silently strip every group member but one from the apply phase.
type targetIndex map[promclient.WorkloadIdentity][]*workloadTarget

// discover is the first phase of a reconcile: it indexes the listed targets by
// identity and guarantees a WorkloadRecommendation exists for each.
//
// It issues NO Prometheus queries. Keeping discovery free of them is what
// allows computation to be driven by the WLR list instead of this one — an
// identity only has to be discovered once, whereas it must be computed on
// every cycle.
//
// Several targets can share one identity when owner-name grouping is in use
// (Deployments "api-blue" and "api-green" both reporting as "Deployment/api").
// They collapse to a single WLR — the same thing the Prometheus queries and
// the cache naming already do — but ALL of them are kept in the index, because
// the apply phase must still reach each one's pods. The WLR write happens once
// per IDENTITY, from a snapshot merged across the group (see
// mergedObservedResources); writing once per target instead had the members
// overwriting each other's snapshot on every cycle, forever.
//
// Errors are logged and skipped rather than aborting the phase: one identity
// whose WLR cannot be written must not stop every other workload in the policy
// from being computed. The count of such failures is returned rather than
// swallowed, because a persistent cause (missing RBAC on
// workloadrecommendations, an admission webhook rejecting the create, a
// namespace quota) would otherwise leave the policy reporting success forever
// while doing nothing.
//
// What a failure costs is bounded by the index this returns. collectComputeItems
// reconciles the WLR List against it, and computeItem.Observed is built from the
// index rather than read back off the object, so an identity here is computed
// and applied this cycle from its members' own container sets whether or not the
// write landed — the failure costs the CACHE WRITE (the webhook keeps serving
// the previous value), not the reconcile, and not the snapshot the computation
// runs against.
//
// The follow-up wlrcache.Upsert in computeIdentity does not reliably repair that
// cache write this same cycle. If the object still does not exist, Upsert's own
// Create hits the same underlying error this EnsureExists did. If it does exist
// — this EnsureExists lost a race with another writer — Upsert's Create gets
// AlreadyExists, and unlike EnsureExists (which re-reads on exactly that race)
// Upsert's Create has no such branch, so it returns an error that is logged at
// V(1) and discarded by its caller. Either way the write is only retried for
// real on the NEXT reconcile's EnsureExists call. A transient cause self-heals
// next cycle; a persistent one (RBAC, quota, a rejecting admission webhook) is
// what the returned count exists to surface.
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

	// ONE EnsureExists per identity, not per target. An identity owns exactly
	// one WorkloadRecommendation, so calling it per target made every member of
	// an owner-name group patch status.observedResources back to its own spec
	// on every cycle whenever the members differed at all — two permanent
	// status writes per group per reconcile, and a snapshot whose content was
	// decided by whichever member the listing happened to return last.
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
// names rather than on the order the API server listed them in or the order
// their goroutines happened to finish.
func sortedTargets(targets []*workloadTarget) []*workloadTarget {
	ordered := make([]*workloadTarget, len(targets))
	copy(ordered, targets)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key() < ordered[j].key() })
	return ordered
}

// mergedObservedResources builds the ONE observed-resources snapshot an
// identity carries, from every target that reports into it.
//
// Container names are UNIONED across the group. The identity has a single
// WorkloadRecommendation, and everything downstream reads the container set off
// it: containersFromObserved sizes the Prometheus shard and reconstructs what
// gets recommended, and the webhook injects the result into the pods of ANY
// member. Taking one member's set instead would silently drop every container
// only its siblings declare — for a group mid-migration, a recommendation
// covering half the group.
//
// Where two members declare the SAME container name, the whole entry from the
// first member in sorted key() order wins, rather than a per-field maximum.
// The requests and limits in one entry are read as a pair by
// recommender.ComputeLimit (limit strategies that preserve the current
// request:limit ratio), so mixing fields across members would synthesize a pair
// no workload ever actually ran with. First-wins keeps every entry internally
// consistent and, because the order is the members' own keys rather than the
// listing order, produces the same snapshot on every cycle regardless of which
// member's goroutine finishes first or how the API server orders the listing.
//
// That determinism is NOT what stops the members from flapping the status
// back and forth forever — a deterministic merge was measured in place on its
// own and still produced three status writes per cycle. What stops the flap
// is that discovery and the computation phase both write the exact same
// Observed value for the identity (see computeItem.Observed), so the two
// writers can never disagree with each other.
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
