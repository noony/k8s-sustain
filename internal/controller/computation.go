package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

// computeItem is one unit of computation: a WorkloadRecommendation, and every
// live workload object that reports into its identity.
//
// Targets is empty for a DEPARTED identity — a completed Job, a bare-pod group
// between runs. Those are the whole reason computation is driven by the WLR
// list rather than the target listing: they never appear in a listing, so
// under the previous design nothing could ever recompute them and their
// recommendation froze until the retention window lapsed.
//
// Targets holds more than one entry under owner-name grouping. Computation is
// per ITEM (one Prometheus fetch, one recommendation, one WLR); application is
// per TARGET, because each real workload object owns a different set of pods.
type computeItem struct {
	WLR      *sustainv1alpha1.WorkloadRecommendation
	Targets  []*workloadTarget
	Identity promclient.WorkloadIdentity
	// Observed is THE observed-resources snapshot for this identity this
	// cycle, and the only one anything in the computation phase may use: the
	// merge across every live member (mergedObservedResources) when the
	// identity has any, the WLR's stored snapshot when it has none.
	//
	// It exists so the two writers of status.observedResources cannot
	// disagree. Discovery writes exactly this value via EnsureExists; the
	// computation write path passes exactly this value to wlrcache.Upsert.
	// Taking a single member's own snapshot there instead is what made the
	// members of a group overwrite each other's view on every cycle — a
	// permanent write storm whose surviving content was decided by whichever
	// goroutine finished last.
	//
	// It is also the container set the identity is computed against, so a
	// group's recommendation covers the union of its members' containers
	// rather than whichever member happened to win.
	Observed map[string]sustainv1alpha1.ObservedContainerResources
}

// collectComputeItems builds the computation work-list for one policy from its
// WorkloadRecommendation objects, linking each to a live target when the
// discovery index has one.
//
// The List is reconciled AGAINST the discovery index rather than trusted on its
// own. Both run through the same cache-backed manager client, and discover()
// Created those objects moments earlier in this same Reconcile, so an identity
// it just ensured can be absent from this List purely because the informer's
// watch event has not landed yet — the read-after-write race internal/wlrcache
// documents for Get, at the one site that reads by List. Trusting the List
// alone means a newly discovered workload is neither computed nor applied on
// the cycle it is first seen, and since nothing watches WorkloadRecommendation
// to re-trigger, it waits a full --reconcile-interval for a second chance.
// Before WLR-driven computation the target listing drove computation directly
// and such a workload was recommended in the same pass.
//
// Reconciling costs no API calls: idx holds every identity discovery ensured,
// and each carries its own container set, which is the only thing the missing
// object would have contributed (see synthesizeComputeItem).
//
// Scoped per policy, not cluster-wide, for a hard reason rather than
// convenience: recommender.FetchWorkloadInputsBatch takes a single
// ResourcesConfigs and shard cost is containers x windowMinutes, so a shard
// must be window-homogeneous. Two policies with different windows cannot share
// a shard.
//
// Ordered by object name so repeated cycles build shards identically and a
// failure is reproducible.
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
		// Defensive, mirroring the sweep: a stale label relative to spec.policy
		// must not pull another policy's object into this policy's shards,
		// where it would be queried with the wrong window.
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

	sort.Slice(items, func(a, b int) bool {
		if items[a].WLR.Namespace != items[b].WLR.Namespace {
			return items[a].WLR.Namespace < items[b].WLR.Namespace
		}
		return items[a].WLR.Name < items[b].WLR.Name
	})
	return items, nil
}

// synthesizeComputeItem builds the in-memory stand-in for a
// WorkloadRecommendation that discover() just ensured but that the cached List
// has not caught up on. It is never written anywhere itself — the real object
// was created by EnsureExists moments earlier in this same Reconcile.
//
// If that EnsureExists failed, the later wlrcache.Upsert in computeIdentity
// does NOT reliably repair it this cycle, despite being a write to the same
// object: Upsert's own Get races the same lagging informer, so it can miss
// too; its Create then hits AlreadyExists; and unlike EnsureExists — which has
// a re-read branch for exactly that race — Upsert's Create does not, so it
// returns an error that computeIdentity logs at V(1) and discards. The
// object only gets written for real on the NEXT reconcile's EnsureExists call.
// So this fix buys pod APPLICATION on the cycle an identity is first seen; the
// webhook's cache still sees nothing new until cycle 2. This stand-in only
// carries the two things the computation phase reads off a WLR.
//
// Those are the observed-resources snapshot, which sizes the Prometheus shard
// and reconstructs the container list, and CreationTimestamp, which the
// workload-age gate reads as "when k8s-sustain first recorded this identity".
// now is the honest value for the latter, but it is also inert: recommender.
// ShouldSkipYoungWorkload takes the EARLIER of the workload object's own
// CreationTimestamp and this one, and every identity that reaches this
// function has a live target with a non-nil Object — a bare-pod target's
// Object is its group's Representative pod (see listBarePodTargets), never
// nil — whose real, already-in-the-past timestamp is always earlier than
// "now" by construction. So the synthesized timestamp can never be the value
// the gate actually compares against; it is set only so the field is
// non-zero, not because the gate reads it.
//
// The snapshot is built with mergedObservedResources — the exact function
// discover() writes with — so this stand-in cannot disagree with the object
// the server actually holds. Under owner-name grouping that is the merge of
// every group member, not any single one of them.
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

// identityObserved picks the ONE observed-resources snapshot an identity is
// computed and cached against this cycle.
//
// A LIVE identity uses the merge across its members' pod templates rather than
// what the WLR happens to hold, for two reasons. It is the value discovery just
// wrote for this same identity, so the computation write path cannot contradict
// it. And the WLR here comes from a cache-backed List that may not have caught
// up with that write yet (see collectComputeItems), so trusting it would mean
// sizing a shard and reconstructing a container set from a snapshot one cycle
// stale — including, for a genuinely new identity, an empty one.
//
// A DEPARTED identity has no members, so its stored snapshot is the only
// evidence left of what it ran with; that is precisely what the snapshot is
// retained for.
func identityObserved(
	wlr *sustainv1alpha1.WorkloadRecommendation,
	targets []*workloadTarget,
) map[string]sustainv1alpha1.ObservedContainerResources {
	if len(targets) > 0 {
		return mergedObservedResources(targets)
	}
	return wlr.Status.ObservedResources
}

// containersFromObserved reconstructs the container list the recommendation
// pipeline needs from the WLR's observed-resources snapshot.
//
// This is what makes a WorkloadRecommendation self-sufficient. A departed
// identity has no workload object left to read a pod template from, and
// re-resolving one from the reference is unreliable, because a reference
// carries an identity rather than an object name. The snapshot discovery
// writes each cycle is both authoritative and always present.
//
// ObservedContainerResources.Init carries the initContainer distinction, so
// the ExcludeInitContainers policy option is honoured without needing the
// original pod template.
func containersFromObserved(
	obs map[string]sustainv1alpha1.ObservedContainerResources,
	excludeInit bool,
) []corev1.Container {
	names := make([]string, 0, len(obs))
	for name, o := range obs {
		if excludeInit && o.Init {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]corev1.Container, 0, len(names))
	for _, name := range names {
		o := obs[name]
		c := corev1.Container{Name: name}
		if req := resourceList(o.CPURequest, o.MemoryRequest); req != nil {
			c.Resources.Requests = req
		}
		if lim := resourceList(o.CPULimit, o.MemoryLimit); lim != nil {
			c.Resources.Limits = lim
		}
		out = append(out, c)
	}
	return out
}

// resourceList builds a corev1.ResourceList from an optional CPU and memory
// quantity, returning nil when neither is set so the caller can leave the
// corresponding requests/limits map absent rather than empty.
func resourceList(cpu, mem *resource.Quantity) corev1.ResourceList {
	if cpu == nil && mem == nil {
		return nil
	}
	rl := corev1.ResourceList{}
	if cpu != nil {
		rl[corev1.ResourceCPU] = *cpu
	}
	if mem != nil {
		rl[corev1.ResourceMemory] = *mem
	}
	return rl
}

// computeIdentity produces the ONE recommendation an identity carries this
// cycle and writes it to the identity's WorkloadRecommendation. It is called
// once per computeItem — never once per group member — and its result is what
// the apply phase then aligns each member's pods against.
//
// One computation per identity, not one per member, because the members of an
// owner-name group are one workload as far as the data is concerned: they share
// a Prometheus series (the batch fetches it once, keyed by identity), one
// WorkloadRecommendation, and therefore one recommendation. Computing per
// member produced N competing answers for a single object, differing only in
// the three per-member inputs — container set, autoscaler, creation timestamp —
// and let whichever goroutine finished last decide which one the group actually
// got. Those three are resolved deterministically here instead:
//
//   - containers: the identity's snapshot (see computeItem.Observed), the union
//     across the group, so a container only one member declares is still sized.
//   - autoscaler: groupAutoscalerInfo — the first member in sorted key() order
//     that has one.
//   - creation: earliestTargetCreation — the identity is as old as its oldest
//     member, which is also how far back its Prometheus history reaches.
//
// A departed identity (no live member) takes the refresh path, which owns its
// own no-data semantics; it is computed and cached but never applied, since
// there are no pods to align. It returns no recommendation to the apply phase
// for the same reason.
func (r *PolicyReconciler) computeIdentity(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	it computeItem,
	autoSnap *autoscaler.NamespacedSnapshot,
	inputs *recommender.WorkloadInputs,
	fetchErr error,
	snapshotPending bool,
) (map[string]workload.ContainerRecommendation, error) {
	// Early bail-out if the parent reconcile context has already been cancelled
	// (typically a manager shutdown mid-batch). Without this, queued work still
	// kicks off Prometheus queries that will each fail through their own
	// timeout path, slowing graceful shutdown.
	if err := ctx.Err(); err != nil {
		return nil, nil //nolint:nilerr // ctx-cancel is graceful shutdown, not a workload error
	}

	if len(it.Targets) == 0 {
		// snapshotPending is not threaded to refreshDepartedRecommendation: it
		// already bails out whenever containersFromObserved(it.Observed) is
		// empty (see its own doc comment) -- the identical condition Reconcile's
		// candidate loop uses to set pendingSnapshot true for this same item. So
		// a departed item only ever reaches buildRecommendations with containers
		// already non-empty, meaning snapshotPending would always be false by
		// the time it got there.
		return nil, r.refreshDepartedRecommendation(ctx, policy, it, inputs, fetchErr)
	}

	containers := containersFromObserved(it.Observed, policy.Spec.RightSizing.ExcludeInitContainers)
	recs, err := r.buildRecommendations(ctx, policy,
		it.Identity.Namespace, it.Identity.OwnerKind, it.Identity.OwnerName,
		containers, r.groupAutoscalerInfo(ctx, it.Identity, it.Targets, autoSnap),
		earliestTargetCreation(it.Targets), it.WLR.CreationTimestamp.Time,
		inputs, fetchErr, snapshotPending)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		// Record the absence, exactly as the departed path does. Without this
		// the WLR keeps a zero status.observedAt indefinitely, and a zero
		// ObservedAt is what the webhook reads as "no recommendation exists
		// yet" (RecSourceMissing) — so every admission of this workload
		// answers with a stub Create/Get for an object discovery already
		// created, once per identity per stubRequestDedupTTL, for as long as
		// the identity has no data. That is the steady state for a workload
		// whose series never appear at all (a quiet container, an owner_name
		// that does not match the recording rules), and metrics.md documents
		// "missing" as a transient state operators may alert on.
		//
		// MarkNoData is a no-op once Containers is populated, so this cannot
		// overwrite a live workload's last-known-good on a single empty
		// query — the same guard the departed path relies on. Best-effort for
		// the same reason as the Upsert below: a failed status write must not
		// take down the apply phase.
		_ = wlrcache.MarkNoData(ctx, r.Client, it.WLR.Spec.WorkloadRef, metav1.Now())
		return nil, nil
	}

	// Persist last-known-good recommendation as a WorkloadRecommendation,
	// BEFORE anything is applied to pods. This is the webhook's only source of
	// recommendations at admission (it never queries Prometheus itself) and
	// gives operators a `kubectl get wlrec` audit surface. Best-effort: the
	// error is deliberately dropped so a failed cache write cannot block the
	// apply phase that follows.
	_ = r.upsertWorkloadRecommendation(ctx, it, policy.Name, recs, metav1.Now())
	return recs, nil
}

// groupAutoscalerInfo resolves the ONE autoscaler an identity's recommendation
// is shaped against: the first member in sorted key() order that has one, or
// KindNone when no member does.
//
// Members of an owner-name group are usually the same application (blue/green,
// canary), so in practice either all of them are autoscaled or none are, and
// the choice is only ever exercised mid-migration. "Any member has one" rather
// than "the first member's answer" is the conservative reading of that window:
// autoscaler coordination exists to stop the recommender fighting an HPA, and
// it is safer to coordinate with an HPA that governs part of the group than to
// ignore it because the alphabetically-first member has none.
//
// Per-member detection still happens in the apply phase, where it drives the
// AutoscalerDetected event and the per-object metrics. Both go through the same
// NamespacedSnapshot, which lists each namespace once and caches it, so asking
// twice costs a map lookup.
//
// This visits every member, not just the winner, so it can also tell whether
// the group is COHERENT. A mixed-autoscaler group -- one member HPA-governed,
// a sibling with none, or governed by a different autoscaler kind entirely --
// is a misconfiguration: the single recommendation computed here is the only
// one the webhook has to inject into ANY member's pods, so an un-autoscaled
// (or differently-autoscaled) sibling silently inherits a value shaped by an
// autoscaler that does not govern it, over- or under-provisioning it depending
// on the coordination factor. That was previously undetectable from outside
// the reconcile. The selection above is unchanged; this only adds a V(1) log
// and a counter (k8s_sustain_group_autoscaler_mismatch_total) so an operator
// can find and fix the group -- a log line is easy to miss and this condition
// is rare and per-identity, exactly the shape the other identity-scoped
// counters in this package (e.g. recycleSuppressedTotal, oomFloorApplied) are
// used for, so a counter costs nothing extra to add and makes the signal
// alertable instead of grep-only.
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

// earliestTargetCreation returns the oldest creation timestamp among an
// identity's live members, or the zero time when none of them carries an
// object.
//
// The oldest rather than any single member's, because the value feeds
// recommender.ShouldSkipYoungWorkload and the question it answers is how far
// back the identity's Prometheus history plausibly reaches. A group whose
// oldest member has been running for a week has a week of history under the
// shared identity, whatever a member added five minutes ago would say on its
// own — and the gate already takes the earlier of this and the WLR's own
// CreationTimestamp for the same reason.
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
// specific member actually runs.
//
// The identity's recommendation covers the UNION of its members' containers, so
// a member mid-migration can be handed a recommendation for a container it does
// not declare. Nothing would be applied to it — no pod has that container — but
// changedContainers reports a recommended name with no matching container as
// changed, which would put a container the Deployment does not have into its
// ResourcesUpdated event on every cycle.
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

// refreshDepartedRecommendation recomputes an identity that has no live
// workload object — a completed Job, a bare-pod group between runs.
//
// This is the regression fix. Nothing previously wrote these: they are absent
// from every target listing, so their ObservedAt froze at the moment the
// workload last ran and the only thing that ever recomputed them was the
// retention window lapsing, the sweep deleting the object, and a later
// admission re-creating it. That made the recompute cadence equal to
// --recommendation-retention.
//
// Departed is deliberately NOT cleared on the empty-result path below: the
// identity still has no workload object, so nothing has changed about its
// absence, and keeping the flag set is what lets the webhook serve the
// retained recommendation without tripping the staleness gate between runs.
// On the non-empty path wlrcache.Upsert DOES clear it, via buildStatus, and
// that is correct rather than a contradiction — an identity that produced
// fresh samples has run again since the sweep marked it departed, so the next
// discovery would clear the flag anyway.
func (r *PolicyReconciler) refreshDepartedRecommendation(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	it computeItem,
	inputs *recommender.WorkloadInputs,
	fetchErr error,
) error {
	logger := log.FromContext(ctx).WithValues(
		"kind", it.Identity.OwnerKind, "name", it.Identity.OwnerName, "namespace", it.Identity.Namespace)

	containers := containersFromObserved(it.Observed, policy.Spec.RightSizing.ExcludeInitContainers)
	if len(containers) == 0 {
		// Nothing to compute against, and nothing here can fix it — the
		// snapshot is written by discovery (from the pod template) or by the
		// webhook (from an admitted pod), both of which need the workload to
		// exist. Departed identities have no workload.
		//
		// This must never be silent again. It used to return nil with no log
		// and no metric, which is precisely what hid a read-after-write bug
		// that left every genuinely new identity stranded in exactly this
		// state: the object existed, looked healthy, and was skipped here on
		// every cycle without a single signal.
		EmitWLRRefresh(it.Identity.Namespace, it.Identity.OwnerKind, WLRRefreshNoSnapshot)
		logger.V(1).Info("departed identity has no observed-resources snapshot; skipping computation",
			"wlr", it.WLR.Name)
		return nil
	}

	// workloadCreated is zero: a departed identity has no workload object left
	// to read a creation timestamp from. The age gate therefore rests entirely
	// on the WLR's own CreationTimestamp — how long k8s-sustain has known the
	// identity — which is exactly the signal that survives the object. A
	// departed identity is by definition one that has already run, so its WLR
	// is usually well past MinWorkloadAge and the gate does nothing here.
	//
	// It is no longer unconditionally inert, though: this path used to pass two
	// zero signals, which disabled the gate outright, so an identity whose WLR
	// is under MinWorkloadAge old is newly gated. The consequence is benign and
	// self-healing within 10 minutes — the gate returns empty recs, which takes
	// the MarkNoData branch below, and MarkNoData no-ops once Containers is
	// populated, so a retained last-known-good is preserved. The only visible
	// effect is one WLRRefreshRetainedEmpty in place of WLRRefreshComputed.
	//
	// autoscaler.KindNone rather than a snapshot lookup: coordination shapes
	// requests against an HPA's replica behaviour, and an identity with no
	// workload object has nothing for an HPA to scale.
	recs, err := buildRecommendations(ctx,
		recDeps{Prom: r.PrometheusClient, LiveOOM: r.LiveOOM},
		policy, it.Identity.Namespace, it.Identity.OwnerKind, it.Identity.OwnerName,
		containers, autoscaler.Info{Kind: autoscaler.KindNone},
		// snapshotPending is always false here: the empty-containers return
		// above already excludes the one case (this identity was marked
		// pendingSnapshot by the candidate loop) where a caller-supplied true
		// would ever have been meaningful -- see this function's call site in
		// computeAndApply.
		time.Time{}, it.WLR.CreationTimestamp.Time, inputs, fetchErr, false)
	if err != nil {
		EmitWLRRefresh(it.Identity.Namespace, it.Identity.OwnerKind, WLRRefreshError)
		return err
	}

	if len(recs) == 0 {
		// Two different situations share this branch, and the metric must tell
		// them apart: an identity that has never produced anything (healthy
		// cold start, converges once history accumulates) versus one whose
		// existing recommendation just lost its underlying data because the
		// samples aged out of the query window. Only the second is worth an
		// operator's attention.
		//
		// MarkNoData is a no-op when Containers is already populated, which is
		// what preserves the retained last-known-good in that second case.
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
