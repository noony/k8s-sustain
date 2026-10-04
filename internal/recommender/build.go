package recommender

import (
	"time"

	corev1 "k8s.io/api/core/v1"

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

// ContainerInputs is the per-container slice of WorkloadInputs plus the
// OOM/autoscaler/config context needed to compute one recommendation. CPUPerPod
// and MemPerPod are already per-pod percentiles (busiest replica) — they feed
// the request computation directly without replica division.
type ContainerInputs struct {
	Container   corev1.Container
	CPUPerPod   float64
	HasCPU      bool
	MemPerPod   float64
	HasMemUsage bool
	// OOM is the per-container memory floor signal. HasOOMPeak gates memory
	// emission when usage samples are absent — see ComputeContainerRec.
	OOM        OOMSignal
	HasOOMPeak bool
	AutoInfo   autoscaler.Info
	RsCfg      sustainv1alpha1.ResourcesConfigs
	CoordCfg   sustainv1alpha1.AutoscalerCoordination
}

// ContainerRecResult is the output of ComputeContainerRec: the container's
// recommendation, the trace of how it was derived, and the OOM signal its
// memory request was floored against.
type ContainerRecResult struct {
	Rec     workload.ContainerRecommendation
	Trace   sustainv1alpha1.ContainerTrace
	OOM     OOMSignal
	HasData bool
}

// ComputeContainerRec runs the shared per-container compute pipeline, each
// resource through its stages (see sustainv1alpha1.ResourceTrace), then
// derives the limits. HasData=false means neither CPU nor memory had enough
// signal to emit a recommendation — the caller should skip the container.
//
// Memory is emitted when EITHER usage samples are present OR a recent/live
// OOM comes with a positive anchor (kernel-observed peak or OOM-time limit).
// This lets crash-looping containers — which can't accumulate usage samples
// — still receive a recommendation anchored on real data. An OOM event with
// no anchor at all emits nothing: the only possible output would be the hard
// 1Mi minimum, which would guarantee the next OOM.
func ComputeContainerRec(in ContainerInputs) ContainerRecResult {
	emitMem := in.HasMemUsage || (in.OOM.recent() && (in.HasOOMPeak || in.OOM.OOMTimeLimitBytes > 0))
	if !in.HasCPU && !emitMem {
		return ContainerRecResult{}
	}

	res := ContainerRecResult{OOM: in.OOM, HasData: true}
	if in.HasCPU {
		res.Trace.CPU = cpuTrace(in)
	}
	if emitMem {
		res.Trace.Memory = memoryTrace(in)
	}

	resources := in.Container.Resources
	if t := res.Trace.CPU; t != nil {
		var lr LimitResult
		res.Rec.CPURequest, lr = finish(t, resources.Requests.Cpu(), resources.Limits.Cpu(), in.RsCfg.CPU.Limits)
		res.Rec.CPULimit, res.Rec.RemoveCPULimit = lr.Quantity, lr.Remove
	}
	if t := res.Trace.Memory; t != nil {
		var lr LimitResult
		res.Rec.MemoryRequest, lr = finish(t, resources.Requests.Memory(), resources.Limits.Memory(), in.RsCfg.Memory.Limits)
		res.Rec.MemoryLimit, res.Rec.RemoveMemoryLimit = lr.Quantity, lr.Remove
	}
	return res
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
	// LiveOOMs are the kills the OOM Pod watcher saw, by container. Each counts
	// as a recent OOM before the recording rules surface it.
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

// Compute is the recommendation algorithm: the age gate, then the
// per-container pipeline of ComputeContainerRec. Every reader of a
// recommendation goes through here so the number the dashboard shows is the
// number the controller applies.
func Compute(req Request) Result {
	containers := req.Containers
	if len(containers) == 0 {
		containers = req.Inputs.ObservedContainers()
	}
	res := Result{
		Recs:       make(map[string]workload.ContainerRecommendation, len(containers)),
		Containers: make(map[string]ContainerRecResult, len(containers)),
	}
	for _, c := range containers {
		cr := computeContainer(c, req)
		if !cr.HasData {
			continue
		}
		res.Recs[c.Name] = cr.Rec
		res.Containers[c.Name] = cr
	}

	// Workload-level recency only excuses the age gate. The memory floor uses
	// per-container recency, so a sibling's OOM never floors an innocent
	// container.
	recentOOM := req.Inputs.HasRecentOOM() || len(req.LiveOOMs) > 0
	switch {
	case shouldSkipYoungWorkload(req.Since, recentOOM):
		res.Outcome = TooYoung
	case len(res.Recs) == 0:
		res.Outcome = NoData
	default:
		res.Outcome = Recommended
	}
	return res
}

// computeContainer runs ComputeContainerRec for one container, folding the
// live OOM watcher's record into the Prometheus OOM signal.
//
// The OOM-time limit anchors on whichever source reports the HIGHER value. The
// two have complementary blind spots and neither can be inflated by
// k8s-sustain's own resize: Prometheus survives a controller restart but is
// windowed, so right after a resize-then-OOM it can still report the PREVIOUS
// limit; the live record has the exact limit applied at that kill but is lost
// on restart. Preferring Prometheus anchors on the stale, lower limit and
// under-bumps a container that is still OOM-looping.
func computeContainer(c corev1.Container, req Request) ContainerRecResult {
	in := req.Inputs
	cpuPerPod, hasCPU := in.CPUPerPod[c.Name]
	memPerPod, hasMem := in.MemPerPod[c.Name]
	_, hasPeak := in.OOM.PeakMemoryBytes[c.Name]

	oom := NewOOMSignal(in.OOM.OOMCounts[c.Name] > 0, in.OOM.PeakMemoryBytes[c.Name], in.OOM.OOMLimitBytes[c.Name])
	if live := req.LiveOOMs[c.Name]; live != nil {
		oom.LiveEventAt = live.TerminatedAt
		oom.OOMTimeLimitBytes = max(oom.OOMTimeLimitBytes, float64(live.OOMLimitBytes))
	}

	return ComputeContainerRec(ContainerInputs{
		Container:   c,
		CPUPerPod:   cpuPerPod,
		HasCPU:      hasCPU,
		MemPerPod:   memPerPod,
		HasMemUsage: hasMem,
		OOM:         oom,
		HasOOMPeak:  hasPeak,
		AutoInfo:    req.AutoInfo,
		RsCfg:       req.Resources,
		CoordCfg:    req.Coordination,
	})
}
