package recommender

import (
	"maps"
	"time"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// oomBumpFactor lifts the OOM floor above the limit the kernel killed at.
// 1.20 matches VPA's MemoryBumpUpRatio: large enough to escape that limit,
// small enough to converge quickly when the workload's real need sits just
// above it.
const oomBumpFactor = 1.20

// LiveOOMWindow is how long a kill the OOM Pod watcher saw counts as recent,
// by when the watcher saw it. It outlasts the recording rules' lag, after
// which their 24h window carries the kill.
const LiveOOMWindow = 30 * time.Minute

// oomRules are the 24h recording rules the OOM floor reads, in one query.
var oomRules = []string{
	promclient.MetricWorkloadOOM24h,
	promclient.MetricContainerPeakMemory24hBytes,
	promclient.MetricContainerOOMLimit24hBytes,
}

// OOM is one container's OOM kills: those the 24h recording rules saw, the
// one the OOM Pod watcher saw, or both. A recently killed container's memory
// request is floored at max(PeakBytes, LimitBytes × 1.20), headroom applied
// once on top.
//
// Two anchors so the floor degrades gracefully:
//   - PeakBytes is the kernel high-water mark when cAdvisor manages to
//     observe it. Unreliable on cgroup v2 (sub-scrape OOM kills) but precise
//     when it works. The peak rule is not OOM-scoped: every container has
//     one, and only a recent kill makes it a floor.
//   - LimitBytes is the cgroup limit at the moment of the kill. Always
//     available (it's the limit the kernel killed at), and the bump pushes
//     the recommendation above it.
//
// The container's current request is deliberately NOT a floor: it would
// compound the previous recommendation's already-headroomed value, growing the
// limit by (1 + headroom) per cycle even after the workload fits. The
// OOM-time-limit anchor only refreshes when a new OOM fires, so it converges.
// For a hard "never go below X", use cfg.MinAllowed.
type OOM struct {
	// Kills counts the container's OOM kills in the recording rules' 24h
	// window.
	Kills float64
	// PeakBytes is the container's 24h memory high-water mark. HasPeak says
	// Prometheus reported one, zero included.
	PeakBytes float64
	HasPeak   bool
	// LimitBytes is the memory limit the container was killed at, the higher
	// of the recording rule's and the watcher's. The two have complementary
	// blind spots and neither can be inflated by k8s-sustain's own resize:
	// Prometheus survives a controller restart but is windowed, so right
	// after a resize-then-OOM it can still report the PREVIOUS limit; the
	// watcher has the exact limit applied at that kill but loses it on
	// restart. Preferring Prometheus anchors on the stale, lower limit and
	// under-bumps a container that is still OOM-looping.
	LimitBytes float64
	// LiveAt is when the OOM Pod watcher's kill happened, zero when it saw
	// none within LiveOOMWindow.
	LiveAt time.Time
}

// recent reports whether the container was OOM-killed recently: in the
// recording rules' window, or seen by the watcher within LiveOOMWindow. This
// is the only place OOM recency is decided.
func (o OOM) recent() bool {
	return o.Kills > 0 || !o.LiveAt.IsZero()
}

func (o OOM) floor() float64 {
	floor := o.PeakBytes
	if o.LimitBytes > 0 {
		floor = max(floor, o.LimitBytes*oomBumpFactor)
	}
	return floor
}

// anchored reports whether the floor rests on an observation: a peak or the
// limit at the kill. Without one, the only request it could produce is the
// hard 1Mi minimum, which would guarantee the next kill.
func (o OOM) anchored() bool {
	return o.HasPeak || o.LimitBytes > 0
}

// oomFloor is the adjuster that raises a recently OOM-killed container's
// memory request to the floor of its kills. A sibling's kill never floors an
// innocent container.
type oomFloor struct{}

func (oomFloor) name() string { return "oom" }

// required: the floor only ever raises a request, so an identity whose OOM
// data cannot be read is computed without it rather than not at all.
func (oomFloor) required() bool { return false }

// samplesPerContainer: the rules are read as an instant vector, one sample per
// rule and container whatever the Policy's windows.
func (oomFloor) samplesPerContainer(sustainv1alpha1.ResourcesConfigs) int {
	return len(oomRules)
}

func (oomFloor) query(_ sustainv1alpha1.ResourcesConfigs, shard promclient.Shard) string {
	return shard.MetricsSelector(oomRules...)
}

// collect sums kill counts and keeps the highest peak and limit of each
// container, collapsing the duplicate series several kube-state-metrics
// replicas produce.
func (oomFloor) collect(in *WorkloadInputs, samples []promclient.ShardSample) {
	ooms := make(map[string]OOM)
	for _, s := range samples {
		o := ooms[s.Container]
		switch s.Metric {
		case promclient.MetricWorkloadOOM24h:
			o.Kills += s.Value
		case promclient.MetricContainerPeakMemory24hBytes:
			if !o.HasPeak || s.Value > o.PeakBytes {
				o.PeakBytes, o.HasPeak = s.Value, true
			}
		case promclient.MetricContainerOOMLimit24hBytes:
			o.LimitBytes = max(o.LimitBytes, s.Value)
		default:
			continue
		}
		ooms[s.Container] = o
	}
	in.OOM = ooms
}

// observed lists the containers that were OOM-killed, so a crash-looping
// container with no usage samples is still recommended for.
func (oomFloor) observed(in *WorkloadInputs) []string {
	var out []string
	for name, o := range in.OOM {
		if o.recent() {
			out = append(out, name)
		}
	}
	return out
}

func (oomFloor) slot() slot { return slot{role: adjuster, resource: memoryResource} }

// contribute is the floor of a recently killed container. An anchored floor
// justifies a memory request on its own, so a crash-looping container, which
// cannot accumulate usage samples, still gets one.
func (oomFloor) contribute(in *WorkloadInputs, container string) (contribution, bool) {
	o := in.OOM[container]
	if !o.recent() {
		return contribution{}, false
	}
	return contribution{value: o.floor(), anchors: o.anchored()}, true
}

func (oomFloor) record(t *sustainv1alpha1.ResourceTrace, value float64, determined bool) {
	t.OOMFloor = &sustainv1alpha1.OOMFloorTrace{Value: *memoryResource.quantity(value), Determined: determined}
}

// withLiveOOMs returns in with the OOM Pod watcher's kills merged into its
// OOMs, so the floor does not wait for the recording rules to surface them.
// Kills the watcher saw longer than LiveOOMWindow ago are ignored. in itself
// is left untouched: callers keep reading their own inputs.
func withLiveOOMs(in *WorkloadInputs, live map[string]*oomwatch.OOMRecord) *WorkloadInputs {
	if len(live) == 0 {
		return in
	}
	merged := *in
	merged.OOM = maps.Clone(in.OOM)
	if merged.OOM == nil {
		merged.OOM = make(map[string]OOM, len(live))
	}
	for container, rec := range live {
		if rec == nil || time.Since(rec.ObservedAt) > LiveOOMWindow {
			continue
		}
		o := merged.OOM[container]
		// FinishedAt dates the kill but the kubelet may leave it unset; the
		// watcher seeing an OOMKilled termination is still a kill.
		o.LiveAt = rec.TerminatedAt
		if o.LiveAt.IsZero() {
			o.LiveAt = rec.ObservedAt
		}
		o.LimitBytes = max(o.LimitBytes, float64(rec.OOMLimitBytes))
		merged.OOM[container] = o
	}
	return &merged
}

// oomExcusesAge reports whether any container was OOM-killed recently, which
// lets a Too young identity through the age gate: a crash-looping workload
// would otherwise wait out the gate while it keeps getting killed. Only the
// gate is identity-wide; the floor applies to the killed container alone.
func oomExcusesAge(in *WorkloadInputs) bool {
	for _, o := range in.OOM {
		if o.recent() {
			return true
		}
	}
	return false
}
