package controller

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/inventory"
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
	// autoscalers is what targets each of the identity's members, read for
	// every identity the pass fetched.
	autoscalers autoscaler.Group
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
// backoff is not fetched; every other identity is assembled in one batch
// (recommender.Assemble) and then computed, in parallel, before anything is
// written or applied.
func (r *PolicyReconciler) recommend(ctx context.Context, policy *sustainv1alpha1.Policy, work []passIdentity) []recommendation {
	out := make([]recommendation, len(work))
	var ids []*inventory.Identity
	var at []int
	for i, w := range work {
		if !w.fetched() {
			out[i].outcome = outcomeNotFetched
			continue
		}
		ids = append(ids, w.id)
		at = append(at, i)
	}

	// One autoscaler snapshot per pass; it lists each namespace once, lazily.
	assembled := recommender.Assemble(ctx, r.Inputs, autoscaler.NewNamespacedSnapshot(r.Client), policy.Spec.RightSizing, ids)

	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for k, a := range assembled {
		i := at[k]
		if a.Err != nil {
			out[i] = recommendation{outcome: outcomeFetchFailed, autoscalers: a.Autoscaler, err: a.Err}
			continue
		}
		g.Go(func() error {
			out[i] = computeIdentity(ctx, ids[k], a)
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// computeIdentity computes one assembled identity's verdict. It runs once per
// identity, never per member: members share one Prometheus series and one
// WorkloadRecommendation, so per-member computation produced competing
// answers.
func computeIdentity(ctx context.Context, id *inventory.Identity, a recommender.Assembled) recommendation {
	key := id.Key
	// A departed identity's Group holds no autoscaler: no member is left to scale.
	info := a.Autoscaler.Info
	res := recommendation{
		inputs:      a.Request.Inputs(),
		autoscalers: a.Autoscaler,
		computed:    computed{autoscaler: &info, mismatch: len(a.Autoscaler.Disagreeing) > 0},
	}
	if res.mismatch {
		log.FromContext(ctx).V(1).Info("owner-name group members disagree on autoscaler state; "+
			"the shared recommendation is shaped by the first member by name that HAS an autoscaler",
			"namespace", key.Namespace, "ownerKind", key.OwnerKind, "ownerName", key.OwnerName,
			"governingAutoscaler", info.Kind, "disagreeing", a.Autoscaler.Disagreeing)
	}
	out := recommender.Compute(a.Request)
	switch out.Outcome {
	case recommender.TooYoung:
		res.outcome = outcomeTooYoung
		log.FromContext(ctx).Info("skipping recommendation: workload too young",
			"kind", key.OwnerKind, "name", key.OwnerName, "namespace", key.Namespace,
			"age", recommender.AgeForLog(id.Since, a.Request.Now()), "minAge", recommender.MinWorkloadAge)
	case recommender.NoData:
		res.outcome = outcomeNoData
	default:
		res.outcome = outcomeRecommended
		res.recs = out.Recs
		res.traces = out.Traces
		res.oomFloors = oomFloorsOf(out.Traces, a.Request.Now())
	}
	return res
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
