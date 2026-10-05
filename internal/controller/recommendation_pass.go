package controller

import (
	"context"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/wlrcache"
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

// recommendation is the recommendation pass's verdict on one identity.
type recommendation struct {
	outcome outcome
	// recs is the identity's Recommendation and traces how it was derived,
	// both set only for outcomeRecommended.
	recs   map[string]workload.ContainerRecommendation
	traces map[string]sustainv1alpha1.ContainerTrace
	// inputs is what the identity was computed from, nil unless fetched.
	inputs *recommender.WorkloadInputs
	// err is why the inputs are unavailable, set only for outcomeFetchFailed.
	err error
	computed
}

// computed is what computing an identity found besides its Recommendation.
type computed struct {
	// autoscaler is the one the Recommendation was shaped against, nil
	// unless the identity was computed; mismatch is whether its members
	// disagree on theirs.
	autoscaler *autoscaler.Info
	mismatch   bool
	// oomFloors are the containers whose memory request the OOM floor
	// determined.
	oomFloors []oomFloor
}

// oomFloor is a container whose memory request the OOM floor determined.
type oomFloor struct {
	container string
	// reaction is how long after the live kill that drove the floor it
	// responded; live is false when no live kill did.
	reaction time.Duration
	live     bool
}

// decision is what the verdict records in the identity's
// WorkloadRecommendation; false for an identity that was not fetched, which
// records nothing.
func (rec recommendation) decision(departed bool) (wlrcache.Decision, bool) {
	d := wlrcache.Decision{Departed: departed}
	switch rec.outcome {
	case outcomeRecommended:
		d.Outcome, d.Recs, d.Traces = sustainv1alpha1.OutcomeComputed, rec.recs, rec.traces
	case outcomeTooYoung:
		d.Outcome = sustainv1alpha1.OutcomeTooYoung
	case outcomeNoData:
		d.Outcome = sustainv1alpha1.OutcomeNoData
	case outcomeFetchFailed:
		d.Outcome = sustainv1alpha1.OutcomeFetchFailed
	default:
		return d, false
	}
	return d, true
}

// recommend is the recommendation pass: one verdict per identity on the
// work-list, in its order. An identity whose live members are all in retry
// backoff is not fetched; every other identity is fetched in one batch call
// and then computed, in parallel, before anything is written or applied.
func (r *PolicyReconciler) recommend(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	work []passIdentity,
	autoSnap *autoscaler.NamespacedSnapshot,
) []recommendation {
	out := make([]recommendation, len(work))
	reqs := make([]recommender.InputsRequest, 0, len(work))
	for i, w := range work {
		if !w.fetched() {
			out[i].outcome = outcomeNotFetched
			continue
		}
		reqs = append(reqs, recommender.InputsRequest{
			Identity:   w.id.Key,
			Containers: len(w.containers),
		})
	}

	fetched := r.Inputs.FetchInputs(ctx, policy.Spec.RightSizing.ResourcesConfigs, reqs)
	// Taken after the fetch, which can take minutes: kills it merged in must
	// not postdate the instant they are computed as of.
	now := time.Now()

	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i, w := range work {
		if !w.fetched() {
			continue
		}
		f := fetched[w.id.Key]
		if f.Err != nil {
			out[i] = recommendation{outcome: outcomeFetchFailed, err: f.Err}
			continue
		}
		g.Go(func() error {
			out[i] = r.computeIdentity(ctx, policy, w, f.Inputs, autoSnap, now)
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// computeIdentity computes one fetched identity's verdict as of now. It runs
// once per identity, never per member: members share one Prometheus series
// and one WorkloadRecommendation, so per-member computation produced
// competing answers.
func (r *PolicyReconciler) computeIdentity(
	ctx context.Context,
	policy *sustainv1alpha1.Policy,
	w passIdentity,
	inputs *recommender.WorkloadInputs,
	autoSnap *autoscaler.NamespacedSnapshot,
	now time.Time,
) recommendation {
	id := w.id.Key
	rs := policy.Spec.RightSizing

	// A departed identity has nothing for an autoscaler to scale.
	autoInfo := autoscaler.Info{Kind: autoscaler.KindNone}
	mismatch := false
	if !w.id.Departed() {
		autoInfo, mismatch = r.groupAutoscalerInfo(ctx, id, slices.Concat(w.run, w.backedOff), autoSnap)
	}
	out := recommender.Compute(recommender.Request{
		Containers:   w.containers,
		Resources:    rs.ResourcesConfigs,
		Coordination: rs.AutoscalerCoordination,
		AutoInfo:     autoInfo,
		Inputs:       inputs,
		Since:        w.id.Since,
		Now:          now,
	})
	res := recommendation{inputs: inputs, computed: computed{autoscaler: &autoInfo, mismatch: mismatch}}
	switch out.Outcome {
	case recommender.TooYoung:
		res.outcome = outcomeTooYoung
		log.FromContext(ctx).Info("skipping recommendation: workload too young",
			"kind", id.OwnerKind, "name", id.OwnerName, "namespace", id.Namespace,
			"age", recommender.AgeForLog(w.id.Since, now), "minAge", recommender.MinWorkloadAge)
	case recommender.NoData:
		res.outcome = outcomeNoData
	default:
		res.outcome = outcomeRecommended
		res.recs = out.Recs
		res.traces = out.Traces
		res.oomFloors = oomFloorsOf(out.Traces, now)
	}
	return res
}

// groupAutoscalerInfo resolves the autoscaler an identity's recommendation is
// shaped against from its members' (autoscaler.Group), and reports a
// mixed-autoscaler group, since the one shared recommendation is injected into
// every member's pods.
func (r *PolicyReconciler) groupAutoscalerInfo(
	ctx context.Context,
	identity promclient.WorkloadIdentity,
	targets []*workloadTarget,
	autoSnap *autoscaler.NamespacedSnapshot,
) (autoscaler.Info, bool) {
	logger := log.FromContext(ctx)
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	g, err := autoSnap.ForIdentity(ctx, identity.Namespace, identity.OwnerKind, names)
	if err != nil {
		logger.Error(err, "autoscaler detection failed, proceeding without it",
			"kind", identity.OwnerKind, "name", identity.OwnerName, "namespace", identity.Namespace)
	}
	if len(g.Disagreeing) > 0 {
		logger.V(1).Info("owner-name group members disagree on autoscaler state; "+
			"the shared recommendation is shaped by the first member by name that HAS an autoscaler",
			"namespace", identity.Namespace, "ownerKind", identity.OwnerKind, "ownerName", identity.OwnerName,
			"governingAutoscaler", g.Info.Kind, "disagreeing", g.Disagreeing)
	}
	return g.Info, len(g.Disagreeing) > 0
}

// oomFloorsOf reads, from the OOM floor stage of each container's trace,
// the containers whose memory request the floor determined as of now.
func oomFloorsOf(traces map[string]sustainv1alpha1.ContainerTrace, now time.Time) []oomFloor {
	var out []oomFloor
	for container, tr := range traces {
		floor, ok := recommender.TracedOOMFloor(tr)
		if !ok || !floor.Determined {
			continue
		}
		f := oomFloor{container: container}
		if !floor.LiveKillAt.IsZero() {
			f.reaction, f.live = now.Sub(floor.LiveKillAt), true
		}
		out = append(out, f)
	}
	return out
}
