package controller

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/noony/k8s-sustain/internal/autoscaler"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

const (
	containerKindRegular = "regular"
	containerKindInit    = "init"
)

// publication is every series one pass of a Policy desires: its rollup and
// fetch coverage, and what the pass found about each identity it included.
type publication struct {
	live, blocked                    int
	requested, resolved, fetchFailed int
	identities                       []identitySeries
}

type identitySeries struct {
	key    promclient.WorkloadIdentity
	health healthState
	// computed is what computing the identity found. Its autoscaler is nil
	// when the pass did not compute it, which leaves the one last published
	// in place.
	computed
	// attempts is how many of its members failed this pass.
	attempts int
	tooYoung bool
	members  []memberSeries
}

// memberSeries is one live member: the Recommendation it holds per
// container, and the decreases the downsize threshold held back on its pods
// this pass, per resource.
type memberSeries struct {
	kind, name string
	containers []containerSeries
	suppressed map[string]int
}

// containerSeries is the Recommendation a member's container holds, next to
// the requests its pod template declares.
type containerSeries struct {
	name, kind string
	rec        workload.ContainerRecommendation
	template   corev1.ResourceList
}

// publisher owns every series the controller publishes about a Policy, the
// identities it governs and their members: nothing else writes or deletes
// them. Each pass publishes what it desires, and the publisher deletes what
// it published before that nothing desires any more, so no series outlives
// what it describes.
//
// A Policy's own series and its members' carry its name, and live while its
// passes keep desiring them, or until it is forgotten. An identity's carry
// no Policy: its gauges are replaced by every pass that includes it, its
// counters accumulate, and all of them are deleted as soon as no Policy's
// last pass includes it. A counter restarts from zero if the identity comes
// back. Passes of different Policies run in any order, so an identity moving
// between two keeps its series only when the new Policy's pass includes it
// before the old one's leaves it out; in the other order its counters
// restart from zero and the new Policy's pass publishes its gauges again.
type publisher struct {
	mu         sync.Mutex
	policies   map[string]*policyPublished
	identities map[promclient.WorkloadIdentity]*published
}

// published is what one owner published: gauges, which each publication
// replaces, and counters and histograms, which accumulate.
type published struct {
	gauges, counters seriesSet
}

type policyPublished struct {
	published
	// identities are the identities the Policy's last pass included, and
	// those it was told an OOM kill of since.
	identities map[promclient.WorkloadIdentity]struct{}
}

// publish replaces what policy's last pass published with pub.
func (p *publisher) publish(policy string, pub publication) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pol := p.policyLocked(policy)
	gauges := seriesSet{}
	gauges.set(policyWorkloadCount, float64(pub.live), policy)
	gauges.set(policyBlockedCount, float64(pub.blocked), policy)
	gauges.set(policyBatchRequested, float64(pub.requested), policy)
	gauges.set(policyBatchResolved, float64(pub.resolved), policy)
	// Never a zero-valued series: the coverage gauges already show a healthy
	// fetch.
	if pub.fetchFailed > 0 {
		pol.counters.add(policyBatchFailuresTotal, float64(pub.fetchFailed), policy)
	}
	included := make(map[promclient.WorkloadIdentity]struct{}, len(pub.identities))
	for _, s := range pub.identities {
		included[s.key] = struct{}{}
		for _, m := range s.members {
			for _, c := range m.containers {
				lv := []string{s.key.Namespace, m.kind, m.name, c.name, c.kind, policy}
				if q := c.rec.CPURequest; q != nil {
					gauges.set(recommendedCPUCores, cores(q), lv...)
				}
				if q := c.rec.MemoryRequest; q != nil {
					gauges.set(recommendedMemoryBytes, float64(q.Value()), lv...)
				}
				if q := c.template.Cpu(); !q.IsZero() {
					gauges.set(templateCPUCores, cores(q), lv...)
				}
				if q := c.template.Memory(); !q.IsZero() {
					gauges.set(templateMemoryBytes, float64(q.Value()), lv...)
				}
			}
		}
		p.identityLocked(s.key).publishIdentity(s)
	}
	gauges.replace(&pol.gauges)
	prev := pol.identities
	pol.identities = included
	p.dropLocked(prev)
}

// publishIdentity replaces the identity's gauges with s's and adds what s
// counted.
func (id *published) publishIdentity(s identitySeries) {
	ns, kind, name := s.key.Namespace, s.key.OwnerKind, s.key.OwnerName
	gauges := seriesSet{}
	if s.health.measured {
		gauges.set(workloadPods, float64(s.health.pods.Total), ns, kind, name)
		gauges.set(workloadStalePods, float64(s.health.pods.Stale), ns, kind, name)
	}
	if s.health.blockedOn != "" {
		gauges.set(workloadRetryState, 1, ns, kind, name, string(s.health.blockedOn))
	}
	switch a := s.autoscaler; {
	case a == nil:
		for x := range id.gauges {
			if x.vec == autoscalerPresent.MetricVec || x.vec == autoscalerTargetConfigured.MetricVec {
				gauges[x] = struct{}{}
			}
		}
	case a.Kind != autoscaler.KindNone:
		gauges.set(autoscalerPresent, 1, ns, kind, name, string(a.Kind))
		for res, v := range a.ConfiguredTargets {
			gauges.set(autoscalerTargetConfigured, float64(v), ns, kind, name, string(a.Kind), res)
		}
	}
	gauges.replace(&id.gauges)

	if s.attempts > 0 {
		id.counters.add(workloadRetryAttempts, float64(s.attempts), ns, kind, name)
	}
	for _, m := range s.members {
		for res, n := range m.suppressed {
			id.counters.add(recycleSuppressedTotal, float64(n), ns, kind, name, res)
		}
	}
	if s.tooYoung {
		id.counters.add(recommendationSkipped, 1, ns, kind, name, "workload_too_young")
	}
	for _, f := range s.oomFloors {
		id.counters.add(oomFloorApplied, 1, ns, kind, name, f.container)
		if f.live {
			id.counters.observe(oomReactionLatencySeconds, f.reaction.Seconds(), ns, kind, name)
		}
	}
	if s.mismatch {
		id.counters.add(groupAutoscalerMismatchTotal, 1, ns, kind, name)
	}
}

func (p *publisher) reconciled(policy, result string, took time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pol := p.policyLocked(policy)
	pol.counters.add(reconcileTotal, 1, policy, result)
	pol.counters.observe(reconcileDuration, took.Seconds(), policy)
}

// observedOOM counts an OOM kill of container, in a pod of id that policy
// governs. Until policy's next pass publishes, policy counts as including id,
// so a kill of an identity that pass leaves out is deleted with the rest of
// its series. That pass may be one already in flight, whose snapshot predates
// the kill: the kill of an identity it did not see is dropped with it, even
// though the pass the kill triggers includes the identity.
func (p *publisher) observedOOM(policy string, id promclient.WorkloadIdentity, container string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policyLocked(policy).identities[id] = struct{}{}
	p.identityLocked(id).counters.add(oomObservedTotal, 1, id.Namespace, id.OwnerKind, id.OwnerName, container)
}

// forget deletes every series of policy, and those of the identities it
// included that no other Policy includes.
func (p *publisher) forget(policy string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pol := p.policies[policy]
	if pol == nil {
		return
	}
	delete(p.policies, policy)
	pol.gauges.deleteAll()
	pol.counters.deleteAll()
	p.dropLocked(pol.identities)
}

// dropLocked deletes the series of every identity of ids that no Policy
// includes any more.
func (p *publisher) dropLocked(ids map[promclient.WorkloadIdentity]struct{}) {
	for key := range ids {
		if p.includedLocked(key) {
			continue
		}
		if id := p.identities[key]; id != nil {
			id.gauges.deleteAll()
			id.counters.deleteAll()
			delete(p.identities, key)
		}
	}
}

func (p *publisher) includedLocked(key promclient.WorkloadIdentity) bool {
	for _, pol := range p.policies {
		if _, ok := pol.identities[key]; ok {
			return true
		}
	}
	return false
}

func (p *publisher) policyLocked(policy string) *policyPublished {
	if p.policies == nil {
		p.policies = make(map[string]*policyPublished)
	}
	pol := p.policies[policy]
	if pol == nil {
		pol = &policyPublished{
			published:  published{gauges: seriesSet{}, counters: seriesSet{}},
			identities: map[promclient.WorkloadIdentity]struct{}{},
		}
		p.policies[policy] = pol
	}
	return pol
}

func (p *publisher) identityLocked(key promclient.WorkloadIdentity) *published {
	if p.identities == nil {
		p.identities = make(map[promclient.WorkloadIdentity]*published)
	}
	id := p.identities[key]
	if id == nil {
		id = &published{gauges: seriesSet{}, counters: seriesSet{}}
		p.identities[key] = id
	}
	return id
}

// maxLabels is the most labels any vector the publisher writes carries.
const maxLabels = 6

// series is one child of a metric vector, named by its label values.
type series struct {
	vec    *prometheus.MetricVec
	n      int
	values [maxLabels]string
}

func (s series) delete() { s.vec.DeleteLabelValues(s.values[:s.n]...) }

// seriesSet is a set of series, written through it.
type seriesSet map[series]struct{}

func (s seriesSet) track(vec *prometheus.MetricVec, lv []string) {
	x := series{vec: vec, n: len(lv)}
	copy(x.values[:], lv)
	s[x] = struct{}{}
}

func (s seriesSet) set(g *prometheus.GaugeVec, v float64, lv ...string) {
	s.track(g.MetricVec, lv)
	g.WithLabelValues(lv...).Set(v)
}

func (s seriesSet) add(c *prometheus.CounterVec, v float64, lv ...string) {
	s.track(c.MetricVec, lv)
	c.WithLabelValues(lv...).Add(v)
}

func (s seriesSet) observe(h *prometheus.HistogramVec, v float64, lv ...string) {
	s.track(h.MetricVec, lv)
	h.WithLabelValues(lv...).Observe(v)
}

// replace deletes every series of *prev that s does not hold, and makes s
// the published set.
func (s seriesSet) replace(prev *seriesSet) {
	for x := range *prev {
		if _, ok := s[x]; !ok {
			x.delete()
		}
	}
	*prev = s
}

func (s seriesSet) deleteAll() {
	for x := range s {
		x.delete()
	}
}

func cores(q *resource.Quantity) float64 {
	return float64(q.MilliValue()) / 1000.0
}
