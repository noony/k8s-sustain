package controller

import (
	"context"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/wlrcache"
	"github.com/noony/k8s-sustain/internal/workload"
)

// passReport is what one pass did with each identity its Policy governs, in
// identity order.
type passReport []identityReport

type identityReport struct {
	key      promclient.WorkloadIdentity
	departed bool
	// fetched is false for an identity the pass left out of the fetch, which
	// records nothing and has no outcome.
	fetched  bool
	outcome  sustainv1alpha1.RecommendationOutcome
	resolved bool
	// failed is whether a departed identity failed: its inputs could not be
	// read or its decision stored. settle counts it, and wlr_refresh_total
	// counts its error, from this one field. A live identity fails through
	// its members.
	failed bool
	// members is one result per live member, backed-off ones included.
	members []applyResult
	// held is the Recommendation a live identity holds for the Policy after
	// the pass: the one it recorded, or else the one its
	// WorkloadRecommendation kept from the Policy's last computation.
	held map[string]workload.ContainerRecommendation
	computed
}

type passIdentity struct {
	id *inventory.Identity
	// run are the live members to apply to and backedOff those in retry
	// backoff. The split is taken once, before the fetch: backoff is
	// time-based and the fetch can take minutes, so asking again at apply
	// time would act on members the fetch left out.
	run, backedOff []*workloadTarget
}

// fetched reports whether the pass fetches the identity's inputs: not when
// every live member is backed off, nor for a departed identity without an
// observed-resources snapshot, which has no container set to compute against.
func (w passIdentity) fetched() bool {
	if w.id.Departed() {
		return len(w.id.Containers) > 0 || len(w.id.InitContainers) > 0
	}
	return len(w.run) > 0
}

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
	work := r.workList(policy, snap.GovernedBy(policy.Name))
	claims := r.claim(ctx, policy.Name, work)
	recs := r.recommend(ctx, policy, work)
	stored := r.record(ctx, work, claims, recs)
	applied := r.apply(ctx, policy, work, recs, stored)
	// Only the reconcile's own context ending is shutdown: an error that
	// merely wraps a context error comes from a call that ran out of its own
	// timeout. A pass cut short by shutdown counts no failure, departed or
	// live, and leaves every member's state as it was.
	shutdown := ctx.Err() != nil

	report := make(passReport, len(work))
	for i, w := range work {
		rep := identityReport{
			key: w.id.Key, departed: w.id.Departed(), fetched: w.fetched(), outcome: recs[i].outcome,
			members: applied[i], computed: recs[i].computed,
		}
		if shutdown {
			rep.members = leftAlone(rep.members)
		}
		if in := recs[i].inputs; in != nil {
			rep.resolved = len(in.CPUPerPod) > 0 || len(in.MemPerPod) > 0
		}
		switch {
		case rep.departed:
			if !shutdown {
				rep.failed = recs[i].err != nil || stored[i] != nil
				emitDepartedRefresh(ctx, w.id, rep)
			}
		case recs[i].outcome == sustainv1alpha1.OutcomeComputed && stored[i] == nil:
			rep.held = recs[i].recs
		default:
			rep.held = recommendedOf(wlrcache.Held(w.id.Recommendation, policy.Name), recommender.ContainersOf(policy.Spec.RightSizing, w.id))
		}
		report[i] = rep
	}
	return report
}

func (r *PolicyReconciler) workList(policy *sustainv1alpha1.Policy, governed []*inventory.Identity) []passIdentity {
	work := make([]passIdentity, 0, len(governed))
	for _, id := range governed {
		w := passIdentity{id: id}
		if !id.Departed() {
			// inventory.TakeForPolicy reads only the kinds policy manages.
			mode := policy.Spec.RightSizing.Update.Types.ModeForKind(id.Key.OwnerKind)
			for _, t := range targetsOf(id, policy.Name, *mode) {
				if r.ledger.mayRun(t.key()) {
					w.run = append(w.run, t)
				} else {
					w.backedOff = append(w.backedOff, t)
				}
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
		wlr, err := wlrcache.Ensure(ctx, r.Client, wlrcache.WorkloadRef(id.Key), policyName, wlrcache.BuildObservedResources(id.Containers, id.InitContainers))
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
		switch {
		case !w.fetched():
		case claims[i].err != nil:
			errs[i] = claims[i].err
		default:
			d := recs[i].decision(w.id.Departed())
			g.Go(func() error {
				errs[i] = wlrcache.Record(ctx, r.Client, claims[i].wlr, d, time.Now())
				return nil
			})
		}
	}
	_ = g.Wait()
	return errs
}

// leftAlone turns the failures among results into members left alone, which
// keep their state.
func leftAlone(results []applyResult) []applyResult {
	for i, res := range results {
		if res.kind == applyFailed {
			results[i] = applyResult{member: res.member, kind: applySkipped}
		}
	}
	return results
}

func emitDepartedRefresh(ctx context.Context, id *inventory.Identity, rep identityReport) {
	ns, kind := id.Key.Namespace, id.Key.OwnerKind
	refresh := WLRRefreshNoData
	switch {
	case rep.failed:
		refresh = WLRRefreshError
	case !rep.fetched:
		// Never silent: this state once hid a read-after-write bug that
		// stranded every new identity.
		refresh = WLRRefreshNoSnapshot
		log.FromContext(ctx).V(1).Info("departed identity has no observed-resources snapshot; skipping computation",
			"kind", kind, "name", id.Key.OwnerName, "namespace", ns)
	case rep.outcome == sustainv1alpha1.OutcomeComputed:
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

func (p passReport) processed() int {
	n := 0
	for _, rep := range p {
		if rep.departed && rep.fetched ||
			slices.ContainsFunc(rep.members, func(res applyResult) bool { return res.kind != applySkipped }) {
			n++
		}
	}
	return n
}

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

// coverage counts the fetch failures apart from what resolved: a young
// workload on a healthy Prometheus resolves to nothing too, so only the
// failure count tells an outage apart.
func (p passReport) coverage() (requested, resolved, failed int) {
	for _, rep := range p {
		switch {
		case !rep.fetched:
			continue
		case rep.outcome == sustainv1alpha1.OutcomeFetchFailed:
			failed++
		case rep.resolved:
			resolved++
		}
		requested++
	}
	return requested, resolved, failed
}

func (p passReport) publication(health healthReport, attempts map[promclient.WorkloadIdentity]int) publication {
	pub := publication{blocked: health.blocked(), identities: make([]identitySeries, len(p))}
	pub.requested, pub.resolved, pub.fetchFailed = p.coverage()
	for i, rep := range p {
		s := identitySeries{
			key:      rep.key,
			health:   health[rep.key],
			computed: rep.computed,
			attempts: attempts[rep.key],
			tooYoung: rep.outcome == sustainv1alpha1.OutcomeTooYoung,
		}
		if !rep.departed {
			pub.live++
		}
		for _, res := range rep.members {
			s.members = append(s.members, memberSeriesOf(res, rep.held))
		}
		pub.identities[i] = s
	}
	return pub
}

func memberSeriesOf(res applyResult, held map[string]workload.ContainerRecommendation) memberSeries {
	t := res.member
	m := memberSeries{kind: t.Kind, name: t.Name, suppressed: res.suppressed}
	for i, c := range slices.Concat(t.Containers, t.InitContainers) {
		rec, ok := held[c.Name]
		if !ok {
			continue
		}
		kind := containerKindRegular
		if i >= len(t.Containers) {
			kind = containerKindInit
		}
		m.containers = append(m.containers, containerSeries{name: c.Name, kind: kind, rec: rec, template: c.Resources.Requests})
	}
	return m
}

// recommendedOf is held narrowed to the containers the identity is
// recommended for: a Recommendation kept from before its Policy excluded init
// containers still names them.
func recommendedOf(held map[string]workload.ContainerRecommendation, containers []corev1.Container) map[string]workload.ContainerRecommendation {
	out := make(map[string]workload.ContainerRecommendation, len(held))
	for _, c := range containers {
		if rec, ok := held[c.Name]; ok {
			out[c.Name] = rec
		}
	}
	return out
}
