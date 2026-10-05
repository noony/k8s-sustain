package recommender

import (
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/workload"
)

// MinWorkloadAge gates the first recommendation on a workload's age. The CPU
// rate rule needs a few minutes after container start to stabilize;
// recommending before that produces near-zero percentile values that get
// floored to the hard minimum and trigger an immediate recycle on the next
// reconcile. 10 minutes leaves headroom past the longest fallback window
// (5m) used by k8s_sustain:container_cpu_usage:rate1m.
//
// A signal can excuse an identity's age: a recent OOM kill does, so a
// crash-looping workload still receives a memory recommendation anchored on
// the kill.
const MinWorkloadAge = 10 * time.Minute

// tooYoung reports whether an identity first seen at since is younger than
// MinWorkloadAge at now, with no signal's values excusing it.
//
// since is when the identity was first seen: its earliest member's creation
// or its WorkloadRecommendation's, whichever is older (inventory.Identity).
// The WorkloadRecommendation matters for ephemeral identities: a standalone
// Job's object is always seconds old, so how long the identity has been known
// is the only usable age.
//
// Wall-clock age is a PROXY for sample stability, and the two come apart for
// duty-cycled workloads: a bare pod running ~35s every 2 minutes clears the
// 10-minute gate on ~3 minutes of runtime and lands on the hard floor anyway
// (measurements in hack/scenarios/recurring.yaml). Left as is deliberately —
// the alternative signal is a per-identity Prometheus subquery that cannot be
// sharded, which is exactly what was removed to cut query load. The mitigation
// is the configured window; see docs/guides/standalone-pods-and-grouping.md.
//
// The narrow hole: losing Prometheus data resets first observation while the
// WLR keeps its old age, so the gate can pass an identity whose samples are
// minutes old.
//
// A zero since disables the gate: there is nothing to date the identity by,
// and skipping would only mask the no-data outcome.
func tooYoung(since, now time.Time, in *WorkloadInputs) bool {
	if since.IsZero() || now.Sub(since) >= MinWorkloadAge {
		return false
	}
	for _, s := range signals {
		if s.excusesAge(in) {
			return false
		}
	}
	return true
}

// AgeForLog renders the age at now of something started at start, for the
// too-young skip logs. Returns "none" for the zero time — logging it directly
// would render as a meaningless epoch offset (object age) or a near-MaxInt64
// duration (identity age).
func AgeForLog(start, now time.Time) string {
	if start.IsZero() {
		return "none"
	}
	return now.Sub(start).String()
}

// Outcome says whether a Result is a Recommendation, and why not.
type Outcome int

const (
	// Recommended means Result.Recs is the identity's Recommendation.
	Recommended Outcome = iota
	// TooYoung means the identity is younger than MinWorkloadAge with no
	// signal excusing it, so its samples are not trusted yet.
	TooYoung
	// NoData means no container had a sample or an OOM anchor to recommend
	// from.
	NoData
)

// Request is one identity's recommendation computation, the unit the
// controller and the dashboard's Simulations share. Only Assemble builds one,
// so every Request carries the instant it computes as of: a zero instant would
// make every dated identity Too young.
type Request struct {
	// containers is the set to recommend for, carrying each container's
	// current requests and limits (the limit stages read them).
	containers   []corev1.Container
	resources    sustainv1alpha1.ResourcesConfigs
	coordination sustainv1alpha1.AutoscalerCoordination
	autoInfo     autoscaler.Info
	inputs       *WorkloadInputs
	// since dates the identity for the age gate; see tooYoung. Zero disables
	// the gate.
	since time.Time
	// now is the instant the computation runs as of: the age gate measures
	// since against it.
	now time.Time
}

// Now is the instant the Request computes as of.
func (r Request) Now() time.Time { return r.now }

// Inputs are what the Request computes from.
func (r Request) Inputs() *WorkloadInputs { return r.inputs }

// Resources are the Policy's resources the Request computes with, every
// default explicit (WithDefaults).
func (r Request) Resources() sustainv1alpha1.ResourcesConfigs { return r.resources }

// WithInputs is the Request computing from in instead: what the identity
// would be recommended had it observed in, such as one past percentile.
func (r Request) WithInputs(in *WorkloadInputs) Request {
	r.inputs = in
	return r
}

// Result is Compute's answer for one identity.
type Result struct {
	Outcome Outcome
	// Recs holds the computed requests and limits whatever the Outcome, so a
	// Simulation can show what a Too young identity would get. Only a
	// Recommended result is a Recommendation.
	Recs map[string]workload.ContainerRecommendation
	// Traces records how each container in Recs was computed.
	Traces map[string]sustainv1alpha1.ContainerTrace
}

// Compute is the recommendation algorithm: the age gate, then each
// container's resources through their chain of stages. Every reader of a
// recommendation goes through here so the number the dashboard shows is the
// number the controller applies.
func Compute(req Request) Result {
	res := Result{
		Recs:   make(map[string]workload.ContainerRecommendation, len(req.containers)),
		Traces: make(map[string]sustainv1alpha1.ContainerTrace, len(req.containers)),
	}
	for _, c := range req.containers {
		rec, tr, ok := computeContainer(c, req)
		if !ok {
			continue
		}
		res.Recs[c.Name] = rec
		res.Traces[c.Name] = tr
	}

	switch {
	case tooYoung(req.since, req.now, req.inputs):
		res.Outcome = TooYoung
	case len(res.Recs) == 0:
		res.Outcome = NoData
	default:
		res.Outcome = Recommended
	}
	return res
}

// computeContainer sizes one container's CPU and memory. False when no
// signal justified a request for either resource: the container is skipped
// rather than recommended at the hard floor.
func computeContainer(c corev1.Container, req Request) (workload.ContainerRecommendation, sustainv1alpha1.ContainerTrace, bool) {
	cpu, cpuOK := size(cpuResource, c, req)
	mem, memOK := size(memoryResource, c, req)
	if !cpuOK && !memOK {
		return workload.ContainerRecommendation{}, sustainv1alpha1.ContainerTrace{}, false
	}

	var rec workload.ContainerRecommendation
	var tr sustainv1alpha1.ContainerTrace
	if cpu != nil {
		rec.CPURequest = &cpu.request
		rec.CPULimit, rec.RemoveCPULimit = cpu.limit.recommendation()
		tr.CPU = &cpu.trace
	}
	if mem != nil {
		rec.MemoryRequest = &mem.request
		rec.MemoryLimit, rec.RemoveMemoryLimit = mem.limit.recommendation()
		tr.Memory = &mem.trace
	}
	return rec, tr, true
}

// sizing is one resource of a container as its chain left it.
type sizing struct {
	request resource.Quantity
	limit   limit
	trace   sustainv1alpha1.ResourceTrace
}

// contributed is what one signal contributed to a container's resource.
type contributed struct {
	signal signal
	value  float64
}

// Stage names of the request side that are not a signal's.
const (
	stageHeadroom = "headroom"
	stageClamp    = "clamp"
)

// size runs one container's resource through its chain: the request's stages
// in their fixed order — the base signals, the adjusters, headroom and the
// min/max clamp, then autoscaler coordination — then the limit's (limitChain).
// justified is false when no signal justified a request; the sizing is nil
// then, and when the Policy keeps the request.
func size(res resourceKind, c corev1.Container, req Request) (_ *sizing, justified bool) {
	var stages []contributed
	var value float64
	winner := -1
	for _, r := range []role{base, adjuster} {
		for _, s := range inSlot(slot{role: r, resource: res}) {
			ct, ok := s.contribute(req.inputs, c.Name)
			if !ok {
				continue
			}
			justified = justified || ct.anchors
			if r == base || ct.value > value {
				value, winner = ct.value, len(stages)
			}
			stages = append(stages, contributed{signal: s, value: ct.value})
		}
	}
	cfg := res.config(req.resources)
	if !justified || cfg.Requests.KeepRequest {
		return nil, justified
	}

	headroom := res.withHeadroom(value, cfg.Requests.Headroom)
	clamped := clamp(headroom, cfg.Requests.MinAllowed, cfg.Requests.MaxAllowed)
	co := coordinate(clamped, res, req.coordination, req.autoInfo, cfg.Requests)
	// Judged on the final request: a clamp on either side of coordination
	// means an operator bound, not the winning signal, set it.
	unbounded := clamped.Cmp(headroom) == 0 && (co == nil || co.value.Cmp(co.scaled) == 0)

	out := &sizing{request: clamped}
	for i, st := range stages {
		out.trace.Request = append(out.trace.Request, st.signal.stage(req.inputs, c.Name, st.value, unbounded && i == winner))
	}
	out.trace.Request = append(out.trace.Request, quantityStage(stageHeadroom, headroom), quantityStage(stageClamp, clamped))
	if co != nil {
		out.request = co.value
		out.trace.Request = append(out.trace.Request, co.stage())
	}
	out.limit, out.trace.Limit = limitChain(res, out.request, c, req.inputs, cfg.Limits)
	return out, true
}

// quantityStage is a stage holding q and nothing else.
func quantityStage(name string, q resource.Quantity) sustainv1alpha1.TraceStage {
	v := q.DeepCopy()
	return sustainv1alpha1.TraceStage{Stage: name, Value: &v}
}

// stageNamed finds the stage named name in stages.
func stageNamed(stages []sustainv1alpha1.TraceStage, name string) (sustainv1alpha1.TraceStage, bool) {
	i := slices.IndexFunc(stages, func(s sustainv1alpha1.TraceStage) bool { return s.Stage == name })
	if i < 0 {
		return sustainv1alpha1.TraceStage{}, false
	}
	return stages[i], true
}
