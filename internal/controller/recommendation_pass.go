package controller

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// outcome is what the recommendation pass decided for one identity.
type outcome int

const (
	// outcomeRecommended: the identity has a Recommendation.
	outcomeRecommended outcome = iota
	// outcomeTooYoung: the identity is Too young to be recommended.
	outcomeTooYoung
	// outcomeNoData: Prometheus answered but had nothing to recommend from.
	outcomeNoData
	// outcomeFetchFailed: the identity's inputs could not be read.
	outcomeFetchFailed
	// outcomeNotFetched: every live member is in retry backoff, so the
	// identity was left out of the fetch.
	outcomeNotFetched
)

// stored is the WorkloadRecommendation outcome an outcome with no new
// Recommendation records; false for the outcomes that record nothing.
func (o outcome) stored() (sustainv1alpha1.RecommendationOutcome, bool) {
	switch o {
	case outcomeTooYoung:
		return sustainv1alpha1.OutcomeTooYoung, true
	case outcomeNoData:
		return sustainv1alpha1.OutcomeNoData, true
	case outcomeFetchFailed:
		return sustainv1alpha1.OutcomeFetchFailed, true
	default:
		return "", false
	}
}

// identityResult is the recommendation pass's verdict on one identity.
type identityResult struct {
	item    computeItem
	outcome outcome
	// recs is the identity's Recommendation and traces how it was derived,
	// both set only for outcomeRecommended.
	recs   map[string]workload.ContainerRecommendation
	traces map[string]sustainv1alpha1.ContainerTrace
	// inputs is what the identity was computed from, nil unless fetched.
	inputs *recommender.WorkloadInputs
	// err is why the inputs are unavailable, set only for outcomeFetchFailed.
	err error
	// apply holds the live members to apply to and backedOff those skipped for
	// retry backoff. The split is decided once, before the fetch: backoff is
	// time-based and the fetch can take minutes, so asking again at apply time
	// would act on members the fetch left out.
	apply     []*workloadTarget
	backedOff []*workloadTarget
}

// recommend is the recommendation pass: one result per identity, in item
// order. An identity whose live members are all in retry backoff is not
// fetched; every other identity is fetched in one batch call and then
// computed, in parallel, before anything is written or applied.
func (r *PolicyReconciler) recommend(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	items []computeItem,
	autoSnap *autoscaler.NamespacedSnapshot,
) []identityResult {
	excludeInit := policy.Spec.RightSizing.ExcludeInitContainers
	results := make([]identityResult, len(items))
	reqs := make([]recommender.InputsRequest, 0, len(items))
	for i, it := range items {
		res := &results[i]
		res.item = it
		for _, t := range it.Targets {
			if r.retries.shouldSkip(t.key()) {
				res.backedOff = append(res.backedOff, t)
			} else {
				res.apply = append(res.apply, t)
			}
		}
		if len(res.backedOff) > 0 && len(res.apply) == 0 {
			res.outcome = outcomeNotFetched
			continue
		}
		reqs = append(reqs, recommender.InputsRequest{
			Identity:   it.Identity,
			Containers: len(containersFromObserved(it.Observed, excludeInit)),
		})
	}

	fetched := r.Inputs.FetchInputs(ctx, policy.Spec.RightSizing.ResourcesConfigs, reqs)

	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i := range results {
		res := &results[i]
		if res.outcome == outcomeNotFetched {
			continue
		}
		f := fetched[res.item.Identity]
		if f.Err != nil {
			res.outcome = outcomeFetchFailed
			res.err = f.Err
			continue
		}
		res.inputs = f.Inputs
		g.Go(func() error {
			r.computeIdentity(ctx, policy, res, autoSnap)
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// computeIdentity computes one fetched identity's outcome. It runs once per
// identity, never per member: members share one Prometheus series and one
// WorkloadRecommendation, so per-member computation produced competing
// answers.
func (r *PolicyReconciler) computeIdentity(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	res *identityResult,
	autoSnap *autoscaler.NamespacedSnapshot,
) {
	it := res.item
	id := it.Identity
	rs := policy.Spec.RightSizing

	// A departed identity has nothing for an autoscaler to scale.
	autoInfo := autoscaler.Info{Kind: autoscaler.KindNone}
	if len(it.Targets) > 0 {
		autoInfo = r.groupAutoscalerInfo(ctx, id, it.Targets, autoSnap)
		EmitAutoscalerPresent(id, string(autoInfo.Kind))
		EmitAutoscalerTargetsConfigured(id, string(autoInfo.Kind), autoInfo.ConfiguredTargets)
	}
	var liveOOMs map[string]*oomwatch.OOMRecord
	if r.LiveOOM.Enabled() {
		liveOOMs = r.LiveOOM.Source.RecentByWorkload(id.Namespace, id.OwnerKind, id.OwnerName, recommender.LiveOOMWindow)
	}
	out := recommender.Compute(recommender.Request{
		Containers:   containersFromObserved(it.Observed, rs.ExcludeInitContainers),
		Resources:    rs.ResourcesConfigs,
		Coordination: rs.AutoscalerCoordination,
		AutoInfo:     autoInfo,
		Inputs:       res.inputs,
		LiveOOMs:     liveOOMs,
		Since:        it.Since,
	})
	switch out.Outcome {
	case recommender.TooYoung:
		res.outcome = outcomeTooYoung
		recommendationSkipped.WithLabelValues(id.Namespace, id.OwnerKind, id.OwnerName, "workload_too_young").Inc()
		log.FromContext(ctx).Info("skipping recommendation: workload too young",
			"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace,
			"age", recommender.AgeForLog(it.Since), "minAge", recommender.MinWorkloadAge)
	case recommender.NoData:
		res.outcome = outcomeNoData
	default:
		res.outcome = outcomeRecommended
		res.recs = out.Recs
		res.traces = out.Traces()
		emitContainerComputation(id, out.Containers)
	}
}

// emitContainerComputation counts the containers whose memory request the OOM
// floor determined, and how long after a live kill each responded.
func emitContainerComputation(id promclient.WorkloadIdentity, containers map[string]recommender.ContainerRecResult) {
	ns, kind, name := id.Namespace, id.OwnerKind, id.OwnerName
	for container, res := range containers {
		if mem := res.Trace.Memory; mem == nil || mem.OOMFloor == nil || !mem.OOMFloor.Determined {
			continue
		}
		oomFloorApplied.WithLabelValues(ns, kind, name, container).Inc()
		if !res.OOM.LiveAt.IsZero() {
			EmitOOMReactionLatency(ns, kind, name, time.Since(res.OOM.LiveAt).Seconds())
		}
	}
}

// passCoverage summarises the pass for the batch metrics: identities fetched,
// those that came back with at least one CPU or memory sample, and those whose
// fetch failed. A young workload on a healthy Prometheus resolves to nothing
// too, so only the failure count tells an outage apart.
func passCoverage(results []identityResult) (requested, resolved, failed int) {
	for _, res := range results {
		switch {
		case res.outcome == outcomeNotFetched:
			continue
		case res.outcome == outcomeFetchFailed:
			failed++
		case len(res.inputs.CPUPerPod) > 0 || len(res.inputs.MemPerPod) > 0:
			resolved++
		}
		requested++
	}
	return requested, resolved, failed
}
