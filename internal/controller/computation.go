package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// computeItem is one unit of computation: a WorkloadRecommendation and every
// live workload object reporting into its identity. Targets is empty for a
// departed identity and holds several entries under owner-name grouping.
type computeItem struct {
	WLR      *sustainv1alpha1.WorkloadRecommendation
	Targets  []*workloadTarget
	Identity promclient.WorkloadIdentity
	// Observed is the single snapshot this cycle computes against: the merge
	// across live members, or the stored snapshot when there are none.
	Observed map[string]sustainv1alpha1.ObservedContainerResources
}

// collectComputeItems builds the per-policy work-list from the
// WorkloadRecommendation list, reconciled against the discovery index so an
// identity discover() created moments ago is computed this cycle even when
// the informer has not caught up. Scoped per policy because a shard must be
// window-homogeneous.
func (r *PolicyReconciler) collectComputeItems(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	idx targetIndex,
) ([]computeItem, error) {
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := r.List(ctx, &list, client.MatchingLabels{wlrPolicyLabel: policy.Name}); err != nil {
		return nil, fmt.Errorf("listing WorkloadRecommendations for policy %s: %w", policy.Name, err)
	}

	items := make([]computeItem, 0, len(list.Items))
	listed := make(map[promclient.WorkloadIdentity]bool, len(list.Items))
	for i := range list.Items {
		wlr := &list.Items[i]
		// A stale label must not pull another policy's object into these shards.
		if wlr.Spec.Policy != policy.Name {
			continue
		}
		id := promclient.WorkloadIdentity{
			Namespace: wlr.Spec.WorkloadRef.Namespace,
			OwnerKind: wlr.Spec.WorkloadRef.Kind,
			OwnerName: wlr.Spec.WorkloadRef.Name,
		}
		listed[id] = true
		items = append(items, computeItem{
			WLR:      wlr,
			Targets:  idx[id],
			Identity: id,
			Observed: identityObserved(wlr, idx[id]),
		})
	}

	// Identities discovery ensured that this List cannot see yet.
	logger := log.FromContext(ctx)
	for id, targets := range idx {
		if listed[id] || len(targets) == 0 {
			continue
		}
		logger.V(1).Info("WorkloadRecommendation not visible in the cached list yet; "+
			"computing from the discovered target instead of waiting a full reconcile interval",
			"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace)
		items = append(items, synthesizeComputeItem(policy.Name, id, targets, metav1.Now()))
	}

	slices.SortFunc(items, func(a, b computeItem) int {
		return cmp.Or(
			strings.Compare(a.WLR.Namespace, b.WLR.Namespace),
			strings.Compare(a.WLR.Name, b.WLR.Name),
		)
	})
	return items, nil
}

// synthesizeComputeItem builds an in-memory stand-in for a
// WorkloadRecommendation that discover() ensured but the cached List has not
// caught up on. It is never written; it only carries the snapshot and the
// CreationTimestamp the computation phase reads.
func synthesizeComputeItem(
	policyName string,
	id promclient.WorkloadIdentity,
	targets []*workloadTarget,
	now metav1.Time,
) computeItem {
	ref := sustainv1alpha1.WorkloadReference{
		Kind:      id.OwnerKind,
		Namespace: id.Namespace,
		Name:      id.OwnerName,
	}
	observed := mergedObservedResources(targets)
	return computeItem{
		WLR: &sustainv1alpha1.WorkloadRecommendation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         id.Namespace,
				Name:              wlrcache.Name(id.OwnerKind, id.OwnerName),
				Labels:            map[string]string{wlrPolicyLabel: policyName},
				CreationTimestamp: now,
			},
			Spec: sustainv1alpha1.WorkloadRecommendationSpec{WorkloadRef: ref, Policy: policyName},
			Status: sustainv1alpha1.WorkloadRecommendationStatus{
				ObservedResources: observed,
			},
		},
		Targets:  targets,
		Identity: id,
		Observed: observed,
	}
}

// identityObserved returns the snapshot an identity is computed against: the
// merge across live members (what discovery just wrote), or the stored
// snapshot for a departed identity.
func identityObserved(
	wlr *sustainv1alpha1.WorkloadRecommendation,
	targets []*workloadTarget,
) map[string]sustainv1alpha1.ObservedContainerResources {
	if len(targets) > 0 {
		return mergedObservedResources(targets)
	}
	return wlr.Status.ObservedResources
}

// containersFromObserved rebuilds the container list from the WLR's
// observed-resources snapshot, the only source left for a departed identity.
func containersFromObserved(
	obs map[string]sustainv1alpha1.ObservedContainerResources,
	excludeInit bool,
) []corev1.Container {
	containers, initContainers := wlrcache.ContainersFromObserved(obs)
	if excludeInit {
		return containers
	}
	return append(containers, initContainers...)
}

// computeIdentity produces the one recommendation an identity carries this
// cycle and writes it to its WorkloadRecommendation. Called once per
// computeItem, never per group member: members share a Prometheus series and
// a WLR, so per-member computation produced competing answers. A departed
// identity is refreshed but never applied.
func (r *PolicyReconciler) computeIdentity(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	it computeItem,
	autoSnap *autoscaler.NamespacedSnapshot,
	inputs *recommender.WorkloadInputs,
	fetchErr error,
	snapshotPending bool,
) (map[string]workload.ContainerRecommendation, error) {
	// Bail out on a cancelled context so shutdown does not fan out doomed queries.
	if err := ctx.Err(); err != nil {
		return nil, nil //nolint:nilerr // ctx-cancel is graceful shutdown, not a workload error
	}

	if len(it.Targets) == 0 {
		return nil, r.refreshDepartedRecommendation(ctx, policy, it, inputs, fetchErr)
	}

	recs, err := r.buildRecommendations(ctx, recRequest{
		Policy:            policy,
		Identity:          it.Identity,
		Containers:        containersFromObserved(it.Observed, policy.Spec.RightSizing.ExcludeInitContainers),
		AutoInfo:          r.groupAutoscalerInfo(ctx, it.Identity, it.Targets, autoSnap),
		WorkloadCreated:   earliestTargetCreation(it.Targets),
		IdentityFirstSeen: it.WLR.CreationTimestamp.Time,
		Inputs:            inputs,
		FetchErr:          fetchErr,
		SnapshotPending:   snapshotPending,
	})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		// Record the absence: a zero ObservedAt reads as "missing" to the webhook
		// and costs a stub Create/Get per admission. MarkNoData no-ops once
		// Containers is populated, so last-known-good survives an empty query.
		_ = wlrcache.MarkNoData(ctx, r.Client, it.WLR.Spec.WorkloadRef, metav1.Now())
		return nil, nil
	}

	// Cache before applying so the webhook serves the new value by the time
	// replacement pods are admitted. Best-effort.
	_ = r.upsertWorkloadRecommendation(ctx, it, policy.Name, recs, metav1.Now())
	return recs, nil
}

// groupAutoscalerInfo resolves the autoscaler an identity's recommendation is
// shaped against: the first member in sorted key() order that has one, or
// KindNone. It also flags a mixed-autoscaler group, since the one shared
// recommendation is injected into every member's pods.
func (r *PolicyReconciler) groupAutoscalerInfo(
	ctx context.Context,
	identity promclient.WorkloadIdentity,
	targets []*workloadTarget,
	autoSnap *autoscaler.NamespacedSnapshot,
) autoscaler.Info {
	logger := log.FromContext(ctx)
	winner := autoscaler.Info{Kind: autoscaler.KindNone}
	haveWinner := false
	var baseline autoscaler.Kind
	haveBaseline := false
	var disagreeing []string

	for _, t := range sortedTargets(targets) {
		info, err := autoSnap.Lookup(ctx, t.Namespace, t.Kind, t.Name)
		if err != nil {
			logger.Error(err, "autoscaler detection failed, proceeding without it",
				"kind", t.Kind, "name", t.Name, "namespace", t.Namespace)
			continue
		}
		if !haveWinner && info.Kind != autoscaler.KindNone {
			winner, haveWinner = info, true
		}
		if !haveBaseline {
			baseline, haveBaseline = info.Kind, true
		} else if info.Kind != baseline {
			disagreeing = append(disagreeing, t.key()+"="+string(info.Kind))
		}
	}

	if len(disagreeing) > 0 {
		logger.V(1).Info("owner-name group members disagree on autoscaler state; "+
			"the shared recommendation is shaped by the first sorted member that HAS an autoscaler "+
			"(see governingAutoscaler), not by the first sorted member",
			"namespace", identity.Namespace, "ownerKind", identity.OwnerKind, "ownerName", identity.OwnerName,
			"governingAutoscaler", winner.Kind, "baseline", baseline, "disagreeing", disagreeing)
		EmitGroupAutoscalerMismatch(identity.Namespace, identity.OwnerKind, identity.OwnerName)
	}

	return winner
}

// earliestTargetCreation returns the oldest creation timestamp among live
// members, which is how far back the identity's Prometheus history reaches.
func earliestTargetCreation(targets []*workloadTarget) time.Time {
	var earliest time.Time
	for _, t := range targets {
		if t.Object == nil {
			continue
		}
		created := t.Object.GetCreationTimestamp().Time
		if created.IsZero() {
			continue
		}
		if earliest.IsZero() || created.Before(earliest) {
			earliest = created
		}
	}
	return earliest
}

// recsForTarget narrows an identity's recommendation to the containers a
// member declares, so changedContainers does not report containers the member
// does not have.
func recsForTarget(
	recs map[string]workload.ContainerRecommendation,
	containers []corev1.Container,
) map[string]workload.ContainerRecommendation {
	out := make(map[string]workload.ContainerRecommendation, len(containers))
	for _, c := range containers {
		if rec, ok := recs[c.Name]; ok {
			out[c.Name] = rec
		}
	}
	return out
}

// refreshDepartedRecommendation recomputes an identity with no live workload
// object, such as a completed Job or a bare-pod group between runs. Departed
// stays set on the empty-result path so the webhook keeps serving the retained
// recommendation; Upsert clears it once fresh samples appear.
func (r *PolicyReconciler) refreshDepartedRecommendation(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	it computeItem,
	inputs *recommender.WorkloadInputs,
	fetchErr error,
) error {
	logger := log.FromContext(ctx).WithValues(
		"kind", it.Identity.OwnerKind, "name", it.Identity.OwnerName, "namespace", it.Identity.Namespace,
	)

	containers := containersFromObserved(it.Observed, policy.Spec.RightSizing.ExcludeInitContainers)
	if len(containers) == 0 {
		// Nothing to compute against and nothing here can fix it. Never silent: this
		// state once hid a read-after-write bug that stranded every new identity.
		EmitWLRRefresh(it.Identity.Namespace, it.Identity.OwnerKind, WLRRefreshNoSnapshot)
		logger.V(1).Info("departed identity has no observed-resources snapshot; skipping computation",
			"wlr", it.WLR.Name)
		return nil
	}

	// A departed identity has no workload object, so the age gate rests on the
	// WLR's own CreationTimestamp, and there is nothing for an HPA to scale.
	recs, err := r.buildRecommendations(ctx, recRequest{
		Policy:            policy,
		Identity:          it.Identity,
		Containers:        containers,
		AutoInfo:          autoscaler.Info{Kind: autoscaler.KindNone},
		IdentityFirstSeen: it.WLR.CreationTimestamp.Time,
		Inputs:            inputs,
		FetchErr:          fetchErr,
	})
	if err != nil {
		EmitWLRRefresh(it.Identity.Namespace, it.Identity.OwnerKind, WLRRefreshError)
		return err
	}

	if len(recs) == 0 {
		// A cold start and a recommendation whose samples aged out share this branch;
		// only the second is worth an alert. MarkNoData no-ops once Containers is set.
		outcome := WLRRefreshNoData
		if len(it.WLR.Status.Containers) > 0 {
			outcome = WLRRefreshRetainedEmpty
			logger.V(1).Info("departed identity produced no recommendation; retaining last known good")
		}
		EmitWLRRefresh(it.Identity.Namespace, it.Identity.OwnerKind, outcome)
		return wlrcache.MarkNoData(ctx, r.Client, it.WLR.Spec.WorkloadRef, metav1.Now())
	}

	if err := r.upsertWorkloadRecommendation(ctx, it, policy.Name, recs, metav1.Now()); err != nil {
		EmitWLRRefresh(it.Identity.Namespace, it.Identity.OwnerKind, WLRRefreshError)
		return err
	}
	EmitWLRRefresh(it.Identity.Namespace, it.Identity.OwnerKind, WLRRefreshComputed)
	return nil
}
