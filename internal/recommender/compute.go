package recommender

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	"github.com/noony/k8s-sustain/internal/workload"
)

// MinWorkloadAge gates the first recommendation on a workload's age. The CPU
// rate rule needs a few minutes after container start to stabilize;
// recommending before that produces near-zero percentile values that get
// floored to the hard minimum and trigger an immediate recycle on the next
// reconcile. 10 minutes leaves headroom past the longest fallback window
// (5m) used by k8s_sustain:container_cpu_usage:rate1m.
//
// A recent OOM event bypasses the gate so crash-looping workloads can still
// receive a memory recommendation anchored on the OOM peak.
const MinWorkloadAge = 10 * time.Minute

// shouldSkipYoungWorkload reports whether a workload is too young to have
// produced stable rate samples and has no recent OOM to bypass the gate.
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
func shouldSkipYoungWorkload(since time.Time, recentOOM bool) bool {
	if recentOOM || since.IsZero() {
		return false
	}
	return time.Since(since) < MinWorkloadAge
}

// AgeForLog renders an age for the too-young skip logs. Returns "none" for
// the zero time — logging it directly would render as a meaningless epoch
// offset (object age) or a near-MaxInt64 duration (identity age).
func AgeForLog(start time.Time) string {
	if start.IsZero() {
		return "none"
	}
	return time.Since(start).String()
}

// ContainerRecResult is one container's recommendation, the trace of how it
// was derived, and the OOM kills its memory request was floored against.
type ContainerRecResult struct {
	Rec   workload.ContainerRecommendation
	Trace sustainv1alpha1.ContainerTrace
	OOM   OOM
}

// Outcome says whether a Result is a Recommendation, and why not.
type Outcome int

const (
	// Recommended means Result.Recs is the identity's Recommendation.
	Recommended Outcome = iota
	// TooYoung means the identity is younger than MinWorkloadAge with no
	// recent OOM to excuse it, so its samples are not trusted yet.
	TooYoung
	// NoData means no container had a sample or an OOM anchor to recommend
	// from.
	NoData
)

// Request is one identity's recommendation computation, the unit the
// controller, the dashboard's recommendation endpoint and its simulator all
// share. Resources and Coordination usually come from a Policy; the simulator
// substitutes the configuration under test.
type Request struct {
	// Containers is the set to recommend for, carrying each container's current
	// requests and limits (limit derivation reads them). Empty means "every
	// container the inputs report", which is all a departed or unknown
	// workload has left.
	Containers   []corev1.Container
	Resources    sustainv1alpha1.ResourcesConfigs
	Coordination sustainv1alpha1.AutoscalerCoordination
	AutoInfo     autoscaler.Info
	Inputs       *WorkloadInputs
	// LiveOOMs are the kills the OOM Pod watcher saw, by container. One seen
	// within LiveOOMWindow counts as a recent OOM before the recording rules
	// surface it.
	LiveOOMs map[string]*oomwatch.OOMRecord
	// Since dates the identity for the age gate; see shouldSkipYoungWorkload.
	// Zero disables the gate.
	Since time.Time
}

// Result is Compute's answer for one identity.
type Result struct {
	Outcome Outcome
	// Recs holds the computed requests and limits whatever the Outcome, so a
	// Simulation can show what a Too young identity would get. Only a
	// Recommended result is a Recommendation.
	Recs map[string]workload.ContainerRecommendation
	// Containers details how each container in Recs was computed.
	Containers map[string]ContainerRecResult
}

// Traces returns the trace of each container in Recs.
func (r Result) Traces() map[string]sustainv1alpha1.ContainerTrace {
	out := make(map[string]sustainv1alpha1.ContainerTrace, len(r.Containers))
	for name, c := range r.Containers {
		out[name] = c.Trace
	}
	return out
}

// Compute is the recommendation algorithm: the age gate, then each
// container's resources through the stages of every signal. Every reader of a
// recommendation goes through here so the number the dashboard shows is the
// number the controller applies.
func Compute(req Request) Result {
	containers := req.Containers
	if len(containers) == 0 {
		containers = observedContainers(req.Inputs)
	}
	in := withLiveOOMs(req.Inputs, req.LiveOOMs)
	res := Result{
		Recs:       make(map[string]workload.ContainerRecommendation, len(containers)),
		Containers: make(map[string]ContainerRecResult, len(containers)),
	}
	for _, c := range containers {
		cr, ok := computeContainer(c, in, req)
		if !ok {
			continue
		}
		res.Recs[c.Name] = cr.Rec
		res.Containers[c.Name] = cr
	}

	switch {
	case shouldSkipYoungWorkload(req.Since, oomExcusesAge(in)):
		res.Outcome = TooYoung
	case len(res.Recs) == 0:
		res.Outcome = NoData
	default:
		res.Outcome = Recommended
	}
	return res
}

// computeContainer runs one container's CPU and memory requests through their
// stages and derives the limits. False when no signal justified a request for
// either resource: the container is skipped rather than recommended at the
// hard floor.
func computeContainer(c corev1.Container, in *WorkloadInputs, req Request) (ContainerRecResult, bool) {
	cpu, cpuOK := requestTrace(cpuResource, c.Name, in, req)
	mem, memOK := requestTrace(memoryResource, c.Name, in, req)
	if !cpuOK && !memOK {
		return ContainerRecResult{}, false
	}

	res := ContainerRecResult{Trace: sustainv1alpha1.ContainerTrace{CPU: cpu, Memory: mem}, OOM: in.OOM[c.Name]}
	resources := c.Resources
	if cpu != nil {
		var lr LimitResult
		res.Rec.CPURequest, lr = finish(cpu, resources.Requests.Cpu(), resources.Limits.Cpu(), req.Resources.CPU.Limits)
		res.Rec.CPULimit, res.Rec.RemoveCPULimit = lr.Quantity, lr.Remove
	}
	if mem != nil {
		var lr LimitResult
		res.Rec.MemoryRequest, lr = finish(mem, resources.Requests.Memory(), resources.Limits.Memory(), req.Resources.Memory.Limits)
		res.Rec.MemoryLimit, res.Rec.RemoveMemoryLimit = lr.Quantity, lr.Remove
	}
	return res, true
}

// stageValue is what one signal contributed to a request.
type stageValue struct {
	signal signal
	value  float64
}

// requestTrace runs one container's request for a resource through its stages
// in their fixed order: the base signals, the adjusters, headroom and the
// min/max clamp, then autoscaler coordination. justified is false when no
// signal justified a request; the trace is nil then, and when the Policy keeps
// the request.
func requestTrace(res resourceKind, container string, in *WorkloadInputs, req Request) (t *sustainv1alpha1.ResourceTrace, justified bool) {
	var stages []stageValue
	var value float64
	winner := -1
	for _, r := range []role{base, adjuster} {
		for _, s := range signals {
			if s.slot() != (slot{role: r, resource: res}) {
				continue
			}
			c, ok := s.contribute(in, container)
			if !ok {
				continue
			}
			justified = justified || c.anchors
			if r == base || c.value > value {
				value, winner = c.value, len(stages)
			}
			stages = append(stages, stageValue{signal: s, value: c.value})
		}
	}
	cfg := res.config(req.Resources).Requests
	if !justified || cfg.KeepRequest {
		return nil, justified
	}

	t = &sustainv1alpha1.ResourceTrace{WithHeadroom: res.withHeadroom(value, cfg.Headroom)}
	t.Clamped = clamp(t.WithHeadroom, cfg.MinAllowed, cfg.MaxAllowed)
	t.Coordination = coordinate(t.Clamped, res, req.Coordination, req.AutoInfo, cfg)
	// Judged on the final request: a clamp on either side of coordination
	// means an operator bound, not the winning signal, set it.
	unbounded := t.Clamped.Cmp(t.WithHeadroom) == 0 &&
		(t.Coordination == nil || t.Coordination.Value.Cmp(t.Coordination.Scaled) == 0)
	for i, st := range stages {
		st.signal.record(t, st.value, unbounded && i == winner)
	}
	return t, true
}

// finish derives the request a trace arrives at and the limit from it, and
// records the limit in the trace.
func finish(t *sustainv1alpha1.ResourceTrace, currentRequest, currentLimit *resource.Quantity, cfg sustainv1alpha1.ResourceLimitsConfig) (*resource.Quantity, LimitResult) {
	final := t.Clamped
	if t.Coordination != nil {
		final = t.Coordination.Value
	}
	request := final.DeepCopy()
	lr := ComputeLimit(&request, currentRequest, currentLimit, cfg)
	if lr.Quantity != nil {
		limit := lr.Quantity.DeepCopy()
		t.Limit = &limit
	}
	t.RemoveLimit = lr.Remove
	return &request, lr
}
