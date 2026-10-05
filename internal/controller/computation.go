package controller

import (
	"context"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// computeItem is one identity the Policy governs, as the recommendation pass
// sees it. Targets is empty for a departed identity and holds several entries
// under owner-name grouping.
type computeItem struct {
	Identity promclient.WorkloadIdentity
	// WLR is the identity's WorkloadRecommendation, the one persist records
	// into: as discovery's Ensure wrote it for a live identity (nil when that
	// failed), as the inventory read it for a departed one.
	WLR     *sustainv1alpha1.WorkloadRecommendation
	Targets []*workloadTarget
	// Observed is the snapshot this cycle computes against: the union of the
	// governed members' containers, or the stored snapshot when none is live.
	Observed map[string]sustainv1alpha1.ObservedContainerResources
	// Since dates the identity for the age gate.
	Since time.Time
}

func (it computeItem) ref() sustainv1alpha1.WorkloadReference {
	return sustainv1alpha1.WorkloadReference{
		Kind:      it.Identity.OwnerKind,
		Namespace: it.Identity.Namespace,
		Name:      it.Identity.OwnerName,
	}
}

// computeItems turns the identities policy governs into the pass's work-list,
// in identity order. A departed identity with no observed-resources snapshot
// is left out: there is no container set to compute against.
func computeItems(ctx context.Context, policy *sustainv1alpha1.Policy, governed []*inventory.Identity) []computeItem {
	logger := log.FromContext(ctx)
	excludeInit := policy.Spec.RightSizing.ExcludeInitContainers
	items := make([]computeItem, 0, len(governed))
	for _, id := range governed {
		it := computeItem{Identity: id.Key, WLR: id.Recommendation, Since: id.Since}
		if id.Departed() {
			it.Observed = id.Recommendation.Status.ObservedResources
			if len(containersFromObserved(it.Observed, excludeInit)) == 0 {
				// Never silent: this state once hid a read-after-write bug that
				// stranded every new identity.
				EmitWLRRefresh(id.Key.Namespace, id.Key.OwnerKind, WLRRefreshNoSnapshot)
				logger.V(1).Info("departed identity has no observed-resources snapshot; skipping computation",
					"kind", id.Key.OwnerKind, "name", id.Key.OwnerName, "namespace", id.Key.Namespace)
				continue
			}
		} else {
			// The snapshot read its own copy of the Policy; one edited since
			// may no longer manage the kind.
			mode := policy.Spec.RightSizing.Update.Types.ModeForKind(id.Key.OwnerKind)
			if mode == nil {
				continue
			}
			it.Targets = targetsOf(id, policy.Name, *mode)
			it.Observed = wlrcache.BuildObservedResources(id.Containers, id.InitContainers)
		}
		items = append(items, it)
	}
	return items
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

// persist records every identity's decision in its WorkloadRecommendation
// before anything is applied, so the webhook serves the new value by the time
// replacement pods are admitted. Departed identities are never applied, so
// persist accounts for them: it returns how many there were and how many
// failed. A live identity's write is best-effort; its apply step reports for
// it.
func (r *PolicyReconciler) persist(ctx context.Context, results []identityResult) (departed, failed int) {
	var failures atomic.Int32
	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range results {
		res := &results[i]
		if len(res.item.Targets) > 0 {
			g.Go(func() error {
				r.persistLive(ctx, res)
				return nil
			})
			continue
		}
		departed++
		g.Go(func() error {
			if err := r.persistDeparted(ctx, res); err != nil {
				failures.Add(1)
			}
			return nil
		})
	}
	_ = g.Wait()
	return departed, int(failures.Load())
}

// persistLive records a live identity's decision. Every outcome is recorded,
// not only a Recommendation, so the webhook reads "nothing to inject" instead
// of "undecided".
func (r *PolicyReconciler) persistLive(ctx context.Context, res *identityResult) {
	if d, ok := res.decision(false); ok {
		_ = wlrcache.Record(ctx, r.Client, res.item.WLR, d, time.Now())
	}
}

// persistDeparted records the decision for an identity with no live workload
// object, such as a completed Job or a bare-pod group between runs, marking it
// departed so the webhook keeps serving its retained recommendation.
func (r *PolicyReconciler) persistDeparted(ctx context.Context, res *identityResult) error {
	it := res.item
	ns, kind := it.Identity.Namespace, it.Identity.OwnerKind
	d, ok := res.decision(true)
	if !ok {
		return nil
	}
	err := wlrcache.Record(ctx, r.Client, it.WLR, d, time.Now())
	switch res.outcome {
	case outcomeRecommended:
		if err != nil {
			EmitWLRRefresh(ns, kind, WLRRefreshError)
			return err
		}
		EmitWLRRefresh(ns, kind, WLRRefreshComputed)
		return nil
	case outcomeFetchFailed:
		EmitWLRRefresh(ns, kind, WLRRefreshError)
		return res.err
	default:
		// A cold start and a recommendation whose samples aged out share this
		// branch; only the second is worth an alert. Record keeps the
		// containers.
		refresh := WLRRefreshNoData
		if len(it.WLR.Status.Containers) > 0 {
			refresh = WLRRefreshRetainedEmpty
			log.FromContext(ctx).V(1).Info("departed identity produced no recommendation; retaining last known good",
				"kind", it.Identity.OwnerKind, "name", it.Identity.OwnerName, "namespace", ns)
		}
		EmitWLRRefresh(ns, kind, refresh)
		return err
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
