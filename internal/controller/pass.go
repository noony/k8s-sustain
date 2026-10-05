package controller

import (
	"context"
	"errors"
	"slices"
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

// passReport is what one pass did with each identity its Policy governs, in
// identity order. A departed identity with no observed-resources snapshot has
// no container set to compute against and is not in it.
type passReport []identityReport

// identityReport is what one pass did with one identity.
type identityReport struct {
	key      promclient.WorkloadIdentity
	departed bool
	outcome  outcome
	// resolved is set when the fetch returned a CPU or memory sample.
	resolved bool
	// err is why a departed identity failed: its inputs could not be read or
	// its decision stored. A live identity fails through its members.
	err error
	// members is one result per live member, backed-off ones included.
	members []applyResult
}

// passIdentity is one identity on the pass's work-list.
type passIdentity struct {
	id *inventory.Identity
	// containers are what the identity is recommended for: its containers,
	// and its init containers unless the Policy excludes them.
	containers []corev1.Container
	// run are the live members to apply to and backedOff those in retry
	// backoff. The split is taken once, before the fetch: backoff is
	// time-based and the fetch can take minutes, so asking again at apply
	// time would act on members the fetch left out.
	run, backedOff []*workloadTarget
}

// fetched reports whether the pass fetches the identity's inputs: not when
// every live member is backed off.
func (w passIdentity) fetched() bool { return w.id.Departed() || len(w.run) > 0 }

// claimed is the WorkloadRecommendation an identity's decision is recorded
// into, or why the governing Policy could not claim it.
type claimed struct {
	wlr *sustainv1alpha1.WorkloadRecommendation
	err error
}

// runPass is one pass of policy over snap. It claims the
// WorkloadRecommendation of every live identity policy governs, recommends
// and records every identity it governs, and only then applies the
// Recommendations, so the webhook serves the new value before the
// replacement of an evicted pod is admitted. A Conflicted identity policy is
// party to only has its outcome recorded (ADR 0002).
func (r *PolicyReconciler) runPass(ctx context.Context, policy *sustainv1alpha1.Policy, snap *inventory.Snapshot) passReport {
	r.recordConflicted(ctx, policy.Name, snap)
	work := r.workList(ctx, policy, snap.GovernedBy(policy.Name))
	claims := r.claim(ctx, policy.Name, work)
	// One autoscaler snapshot per pass; it lists each namespace once, lazily.
	autoSnap := autoscaler.NewNamespacedSnapshot(r.Client)
	recs := r.recommend(ctx, policy, work, autoSnap)
	stored := r.record(ctx, work, claims, recs)
	applied := r.apply(ctx, policy, work, recs, stored, autoSnap)

	report := make(passReport, len(work))
	for i, w := range work {
		rep := identityReport{key: w.id.Key, departed: w.id.Departed(), outcome: recs[i].outcome, members: applied[i]}
		if in := recs[i].inputs; in != nil {
			rep.resolved = len(in.CPUPerPod) > 0 || len(in.MemPerPod) > 0
		}
		if rep.departed {
			rep.err = errors.Join(recs[i].err, stored[i])
		}
		report[i] = rep
	}
	return report
}

// workList turns the identities policy governs into the pass's work-list,
// taking the backoff split of every live member.
func (r *PolicyReconciler) workList(ctx context.Context, policy *sustainv1alpha1.Policy, governed []*inventory.Identity) []passIdentity {
	logger := log.FromContext(ctx)
	excludeInit := policy.Spec.RightSizing.ExcludeInitContainers
	work := make([]passIdentity, 0, len(governed))
	for _, id := range governed {
		w := passIdentity{id: id}
		w.containers, _ = workload.MergeContainersForRecommendation(id.Containers, id.InitContainers, excludeInit)
		if id.Departed() {
			if len(w.containers) == 0 {
				// Never silent: this state once hid a read-after-write bug that
				// stranded every new identity.
				EmitWLRRefresh(id.Key.Namespace, id.Key.OwnerKind, WLRRefreshNoSnapshot)
				logger.V(1).Info("departed identity has no observed-resources snapshot; skipping computation",
					"kind", id.Key.OwnerKind, "name", id.Key.OwnerName, "namespace", id.Key.Namespace)
				continue
			}
			work = append(work, w)
			continue
		}
		// inventory.TakeForPolicy reads only the kinds policy manages.
		mode := policy.Spec.RightSizing.Update.Types.ModeForKind(id.Key.OwnerKind)
		for _, t := range targetsOf(id, policy.Name, *mode) {
			if r.ledger.mayRun(t.key()) {
				w.run = append(w.run, t)
			} else {
				w.backedOff = append(w.backedOff, t)
			}
		}
		work = append(work, w)
	}
	return work
}

// claim is policyName's claim on the WorkloadRecommendation of every live
// identity on the work-list (wlrcache.Ensure), carrying its members'
// containers. It returns, per identity, the object its decision is recorded
// into: the one Ensure wrote, never a re-read the cache may not have caught up
// on, or the one the inventory read for a departed identity. Only governed
// identities reach it, so two Policies never rewrite one object's spec.policy
// back and forth. It issues no Prometheus queries.
//
// A failed claim is logged and kept per identity so one unwritable object
// cannot stop the rest: the identity is still computed but, with nowhere to
// record its decision, not applied. Its members fail on the store step with
// the Ensure error, which is the one count the failure gets.
func (r *PolicyReconciler) claim(ctx context.Context, policyName string, work []passIdentity) []claimed {
	logger := log.FromContext(ctx)
	out := make([]claimed, len(work))
	for i, w := range work {
		id := w.id
		if id.Departed() {
			out[i] = claimed{wlr: id.Recommendation}
			continue
		}
		ref := sustainv1alpha1.WorkloadReference{Kind: id.Key.OwnerKind, Namespace: id.Key.Namespace, Name: id.Key.OwnerName}
		wlr, err := wlrcache.Ensure(ctx, r.Client, ref, policyName, wlrcache.BuildObservedResources(id.Containers, id.InitContainers))
		out[i] = claimed{wlr: wlr, err: err}
		if err != nil {
			logger.Error(err, "failed to ensure the WorkloadRecommendation is current; "+
				"the identity is still computed this cycle, but its recommendation cannot be stored for the webhook, so it is not applied",
				"kind", id.Key.OwnerKind, "name", id.Key.OwnerName, "namespace", id.Key.Namespace)
		}
	}
	return out
}

// record stores every fetched identity's decision in the object claim
// returned, before anything is applied. Every outcome is recorded, not only a
// Recommendation, so the webhook reads "nothing to inject" instead of
// "undecided"; a departed identity's record also marks it departed, so the
// webhook keeps serving its retained Recommendation. It returns, per
// identity, why its decision could not be stored.
func (r *PolicyReconciler) record(ctx context.Context, work []passIdentity, claims []claimed, recs []recommendation) []error {
	errs := make([]error, len(work))
	var g errgroup.Group
	g.SetLimit(r.WorkloadConcurrencyLimit)
	for i, w := range work {
		d, ok := recs[i].decision(w.id.Departed())
		switch {
		case !ok:
		case claims[i].err != nil:
			errs[i] = claims[i].err
		default:
			g.Go(func() error {
				errs[i] = wlrcache.Record(ctx, r.Client, claims[i].wlr, d, time.Now())
				if w.id.Departed() {
					emitDepartedRefresh(ctx, w.id, recs[i].outcome, errs[i])
				}
				return nil
			})
		}
	}
	_ = g.Wait()
	return errs
}

// emitDepartedRefresh counts what a departed identity's pass decided, and
// whether a Recommendation failed to be stored.
func emitDepartedRefresh(ctx context.Context, id *inventory.Identity, o outcome, recordErr error) {
	ns, kind := id.Key.Namespace, id.Key.OwnerKind
	refresh := WLRRefreshNoData
	switch {
	case o == outcomeFetchFailed, o == outcomeRecommended && recordErr != nil:
		refresh = WLRRefreshError
	case o == outcomeRecommended:
		refresh = WLRRefreshComputed
	case len(id.Recommendation.Status.Containers) > 0:
		// A cold start and a recommendation whose samples aged out both
		// produce nothing; only the second is worth an alert. Record kept
		// the containers.
		refresh = WLRRefreshRetainedEmpty
		log.FromContext(ctx).V(1).Info("departed identity produced no recommendation; retaining last known good",
			"kind", kind, "name", id.Key.OwnerName, "namespace", ns)
	}
	EmitWLRRefresh(ns, kind, refresh)
}

// processed counts the identities the pass worked on: every departed one,
// and every live one with a member it did not skip.
func (p passReport) processed() int {
	n := 0
	for _, rep := range p {
		if rep.departed || slices.ContainsFunc(rep.members, func(res applyResult) bool { return res.kind != applySkipped }) {
			n++
		}
	}
	return n
}

// liveMembers is the key of every live member, by identity: one entry per
// identity with a live member, which is what
// k8s_sustain_policy_workload_count counts.
func (p passReport) liveMembers() map[promclient.WorkloadIdentity][]string {
	out := make(map[promclient.WorkloadIdentity][]string, len(p))
	for _, rep := range p {
		if rep.departed {
			continue
		}
		keys := make([]string, len(rep.members))
		for i, res := range rep.members {
			keys[i] = res.member.key()
		}
		out[rep.key] = keys
	}
	return out
}

// coverage summarises the fetch for the batch metrics: identities fetched,
// those that came back with at least one CPU or memory sample, and those
// whose fetch failed. A young workload on a healthy Prometheus resolves to
// nothing too, so only the failure count tells an outage apart.
func (p passReport) coverage() (requested, resolved, failed int) {
	for _, rep := range p {
		switch {
		case rep.outcome == outcomeNotFetched:
			continue
		case rep.outcome == outcomeFetchFailed:
			failed++
		case rep.resolved:
			resolved++
		}
		requested++
	}
	return requested, resolved, failed
}
