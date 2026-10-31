package recommender

import (
	"context"
	"maps"
	"strconv"
	"time"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/oomwatch"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

const (
	stageOOMFloor = "oomFloor"

	// factDetermined says the floor set the final request: it beat the
	// percentile and no min/max clamp, before or after coordination, replaced
	// it.
	factDetermined = "determined"
	// factAnchor names the observation the floor rests on: anchorPeak or
	// anchorKilledAtLimit.
	factAnchor          = "anchor"
	factKilledAtLimit   = "killedAtLimit"
	factLiveKillAt      = "liveKillAt"
	anchorPeak          = "peak"
	anchorKilledAtLimit = "killedAtLimit"
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
	// Prometheus survives a controller restart but records a kill's limit
	// minutes after the kill, so right after a resize-then-OOM it still
	// reports the PREVIOUS limit; the watcher has the exact limit applied at
	// that kill but loses it on restart. Preferring Prometheus anchors on the
	// stale, lower limit and under-bumps a container that is still
	// OOM-looping.
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

// floor is the higher of the peak and the limit at the kill bumped by
// oomBumpFactor, and the anchor that set it: anchorPeak, anchorKilledAtLimit,
// or none when there was neither.
func (o OOM) floor() (float64, string) {
	if bumped := o.LimitBytes * oomBumpFactor; o.LimitBytes > 0 && bumped > o.PeakBytes {
		return bumped, anchorKilledAtLimit
	}
	if o.HasPeak {
		return o.PeakBytes, anchorPeak
	}
	return 0, ""
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

// contract: the rules are read as an instant vector, so the cost does not
// depend on their evaluation interval.
func (oomFloor) contract() PromContract {
	return PromContract{
		Rules:  oomRules,
		Labels: shardLabels,
		CAdvisor: []string{
			"container_memory_max_usage_bytes",
			"container_memory_peak_working_set_bytes",
			"container_memory_working_set_bytes",
			"container_spec_memory_limit_bytes",
		},
		KubeStateMetrics: append([]string{
			"kube_pod_container_status_restarts_total",
			"kube_pod_container_status_last_terminated_reason",
			"kube_pod_container_status_terminated_reason",
			"kube_pod_restart_policy",
		}, podWorkloadMetrics...),
		CAdvisorPerInstance: []string{"container_spec_memory_limit_bytes"},
	}
}

// oomValues are the OOM floor's values: each container's OOM kills.
type oomValues map[string]OOM

// WithOOMKills sets each container's OOM kills.
func WithOOMKills(kills map[string]OOM) InputsOption {
	return setting(oomFloor{}, oomValues(maps.Clone(kills)))
}

// observed lists the containers that were OOM-killed, so a crash-looping
// container with no usage samples is still recommended for.
func (v oomValues) observed() []string {
	var out []string
	for name, o := range v {
		if o.recent() {
			out = append(out, name)
		}
	}
	return out
}

// excusesAge: a crash-looping workload would otherwise wait out the age gate
// while it keeps getting killed. Only the gate is identity-wide; the floor
// applies to the killed container alone.
func (v oomValues) excusesAge() bool {
	for _, o := range v {
		if o.recent() {
			return true
		}
	}
	return false
}

// collect sums kill counts and keeps the highest peak and limit of each
// container, collapsing the duplicate series several kube-state-metrics
// replicas produce.
func (oomFloor) collect(samples []promclient.ShardSample) signalValues {
	ooms := make(oomValues)
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
	return ooms
}

func (oomFloor) slot() slot { return slot{role: adjuster, resource: memoryResource} }

// contribute is the floor of a recently killed container. An anchored floor
// justifies a memory request on its own, so a crash-looping container, which
// cannot accumulate usage samples, still gets one. An unanchored floor does
// not: the only request it could produce is the hard 1Mi minimum, which would
// guarantee the next kill.
func (f oomFloor) contribute(sc stageContext) (contribution, bool) {
	o := valuesOf[oomValues](sc.inputs, f)[sc.container.Name]
	if !o.recent() {
		return contribution{}, false
	}
	floor, anchor := o.floor()
	return contribution{value: floor, anchors: anchor != ""}, true
}

func (f oomFloor) stage(sc stageContext, value float64, determined bool) sustainv1alpha1.TraceStage {
	o := valuesOf[oomValues](sc.inputs, f)[sc.container.Name]
	facts := map[string]string{factDetermined: strconv.FormatBool(determined)}
	if _, anchor := o.floor(); anchor != "" {
		facts[factAnchor] = anchor
	}
	if o.LimitBytes > 0 {
		facts[factKilledAtLimit] = memoryResource.quantity(o.LimitBytes).String()
	}
	if !o.LiveAt.IsZero() {
		facts[factLiveKillAt] = o.LiveAt.UTC().Format(time.RFC3339)
	}
	return sustainv1alpha1.TraceStage{Stage: stageOOMFloor, Value: memoryResource.quantity(value), Facts: facts}
}

// TracedOOM is what the OOM floor's stage of a container's trace records.
type TracedOOM struct {
	// Determined says the floor set the final memory request.
	Determined bool
	// LiveKillAt is when the kill the OOM Pod watcher saw happened, to the
	// second; zero when only the recording rules saw a kill.
	LiveKillAt time.Time
}

// TracedOOMFloor reads the OOM floor stage of a container's trace; false when
// no floor ran.
func TracedOOMFloor(t sustainv1alpha1.ContainerTrace) (TracedOOM, bool) {
	if t.Memory == nil {
		return TracedOOM{}, false
	}
	st, ok := stageNamed(t.Memory.Request, stageOOMFloor)
	if !ok {
		return TracedOOM{}, false
	}
	out := TracedOOM{Determined: st.Facts[factDetermined] == strconv.FormatBool(true)}
	if at, err := time.Parse(time.RFC3339, st.Facts[factLiveKillAt]); err == nil {
		out.LiveKillAt = at
	}
	return out, true
}

// liveOOMInputs is an InputsFetcher that merges the kills the OOM Pod watcher
// saw into the inputs another fetcher read, so the floor does not wait for the
// recording rules to surface a kill.
type liveOOMInputs struct {
	inputs InputsFetcher
	source oomwatch.Source
}

// WithLiveOOMs decorates inputs with the kills source holds. A kill counts as
// of an instant when it happened at or before it and the watcher saw it within
// LiveOOMWindow of it.
func WithLiveOOMs(inputs InputsFetcher, source oomwatch.Source) InputsFetcher {
	return &liveOOMInputs{inputs: inputs, source: source}
}

// FetchInputs merges every fetched identity's kills as of at into its inputs.
// An identity whose inputs could not be read stays failed.
func (l *liveOOMInputs) FetchInputs(
	ctx context.Context,
	at time.Time,
	cfg sustainv1alpha1.ResourcesConfigs,
	reqs []InputsRequest,
) map[promclient.WorkloadIdentity]InputsResult {
	out := l.inputs.FetchInputs(ctx, at, cfg, reqs)
	for id, r := range out {
		if r.Inputs == nil {
			continue
		}
		r.Inputs = withLiveOOMs(r.Inputs, l.source.ByWorkload(id), at)
		out[id] = r
	}
	return out
}

// withLiveOOMs returns in with the watcher's kills as of at merged into its
// OOM kills: a kill after at, or one the watcher saw longer than
// LiveOOMWindow before it, is ignored. in itself is left untouched: the
// decorated fetcher may still hold it.
func withLiveOOMs(in *WorkloadInputs, live map[string]*oomwatch.OOMRecord, at time.Time) *WorkloadInputs {
	if len(live) == 0 {
		return in
	}
	merged := maps.Clone(valuesOf[oomValues](in, oomFloor{}))
	if merged == nil {
		merged = make(oomValues, len(live))
	}
	for container, rec := range live {
		if rec == nil || at.Sub(rec.ObservedAt) > LiveOOMWindow {
			continue
		}
		// FinishedAt dates the kill but the kubelet may leave it unset; the
		// watcher seeing an OOMKilled termination is still a kill.
		killedAt := rec.TerminatedAt
		if killedAt.IsZero() {
			killedAt = rec.ObservedAt
		}
		if killedAt.After(at) {
			continue
		}
		o := merged[container]
		o.LiveAt = killedAt
		o.LimitBytes = max(o.LimitBytes, float64(rec.OOMLimitBytes))
		merged[container] = o
	}
	return in.with(oomFloor{}, merged)
}
