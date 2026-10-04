package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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
// the informer has not caught up. Scoped per policy because one fetch serves
// one ResourcesConfigs.
//
// A departed identity with no observed-resources snapshot is left out: there
// is no container set to compute against and nothing here can provide one.
func (r *PolicyReconciler) collectComputeItems(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	idx targetIndex,
) ([]computeItem, error) {
	var list sustainv1alpha1.WorkloadRecommendationList
	if err := r.List(ctx, &list, client.MatchingLabels{wlrPolicyLabel: policy.Name}); err != nil {
		return nil, fmt.Errorf("listing WorkloadRecommendations for policy %s: %w", policy.Name, err)
	}

	logger := log.FromContext(ctx)
	items := make([]computeItem, 0, len(list.Items))
	listed := make(map[promclient.WorkloadIdentity]bool, len(list.Items))
	for i := range list.Items {
		wlr := &list.Items[i]
		// A stale label must not pull another policy's object into this fetch.
		if wlr.Spec.Policy != policy.Name {
			continue
		}
		id := promclient.WorkloadIdentity{
			Namespace: wlr.Spec.WorkloadRef.Namespace,
			OwnerKind: wlr.Spec.WorkloadRef.Kind,
			OwnerName: wlr.Spec.WorkloadRef.Name,
		}
		listed[id] = true
		it := computeItem{
			WLR:      wlr,
			Targets:  idx[id],
			Identity: id,
			Observed: identityObserved(wlr, idx[id]),
		}
		if len(it.Targets) == 0 && len(containersFromObserved(it.Observed, policy.Spec.RightSizing.ExcludeInitContainers)) == 0 {
			// Never silent: this state once hid a read-after-write bug that
			// stranded every new identity.
			EmitWLRRefresh(id.Namespace, id.OwnerKind, WLRRefreshNoSnapshot)
			logger.V(1).Info("departed identity has no observed-resources snapshot; skipping computation",
				"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace, "wlr", wlr.Name)
			continue
		}
		items = append(items, it)
	}

	// Identities discovery ensured that this List cannot see yet.
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

// persist writes every identity's outcome to its WorkloadRecommendation
// before anything is applied, so the webhook serves the new value by the time
// replacement pods are admitted. Departed identities are never applied, so
// persist accounts for them: it returns how many there were and how many
// failed. A live identity's write is best-effort; its apply step reports for
// it.
func (r *PolicyReconciler) persist(ctx context.Context, policyName string, results []identityResult) (departed, failed int) {
	var failures atomic.Int32
	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range results {
		res := &results[i]
		if len(res.item.Targets) > 0 {
			g.Go(func() error {
				r.persistLive(ctx, policyName, res)
				return nil
			})
			continue
		}
		departed++
		g.Go(func() error {
			if err := r.persistDeparted(ctx, policyName, res); err != nil {
				failures.Add(1)
			}
			return nil
		})
	}
	_ = g.Wait()
	return departed, int(failures.Load())
}

func (r *PolicyReconciler) persistLive(ctx context.Context, policyName string, res *identityResult) {
	switch res.outcome {
	case outcomeRecommended:
		_ = r.upsertWorkloadRecommendation(ctx, res.item, policyName, res.recs, metav1.Now())
	case outcomeTooYoung, outcomeNoData:
		// Record the absence: a zero ObservedAt reads as "missing" to the webhook
		// and costs a stub Create/Get per admission. MarkNoData no-ops once
		// Containers is populated, so last-known-good survives an empty query.
		_ = wlrcache.MarkNoData(ctx, r.Client, res.item.WLR.Spec.WorkloadRef, metav1.Now())
	}
}

// persistDeparted refreshes an identity with no live workload object, such as
// a completed Job or a bare-pod group between runs. Departed stays set on the
// empty-result path so the webhook keeps serving the retained recommendation;
// Upsert clears it once fresh samples appear.
func (r *PolicyReconciler) persistDeparted(ctx context.Context, policyName string, res *identityResult) error {
	it := res.item
	ns, kind := it.Identity.Namespace, it.Identity.OwnerKind
	switch res.outcome {
	case outcomeRecommended:
		if err := r.upsertWorkloadRecommendation(ctx, it, policyName, res.recs, metav1.Now()); err != nil {
			EmitWLRRefresh(ns, kind, WLRRefreshError)
			return err
		}
		EmitWLRRefresh(ns, kind, WLRRefreshComputed)
		return nil
	case outcomeTooYoung, outcomeNoData:
		// A cold start and a recommendation whose samples aged out share this branch;
		// only the second is worth an alert. MarkNoData no-ops once Containers is set.
		refresh := WLRRefreshNoData
		if len(it.WLR.Status.Containers) > 0 {
			refresh = WLRRefreshRetainedEmpty
			log.FromContext(ctx).V(1).Info("departed identity produced no recommendation; retaining last known good",
				"kind", it.Identity.OwnerKind, "name", it.Identity.OwnerName, "namespace", ns)
		}
		EmitWLRRefresh(ns, kind, refresh)
		return wlrcache.MarkNoData(ctx, r.Client, it.WLR.Spec.WorkloadRef, metav1.Now())
	case outcomeFetchFailed:
		EmitWLRRefresh(ns, kind, WLRRefreshError)
		return res.err
	default:
		return nil
	}
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
