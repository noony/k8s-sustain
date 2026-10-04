package recommender

import (
	"math"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
)

const (
	defaultPercentile = 95
	defaultWindow     = "168h" // 7 days
	mebibyte          = 1 << 20
	// Hard floors so we never emit zero/sub-unit recommendations even when
	// the percentile query returns ~0 (e.g. idle container, missing samples).
	minCPUMillicores = 1
	minMemoryMiB     = 1
)

// DefaultOOMBumpFactor is the multiplier applied to the OOM-time memory limit
// when computing the OOM-aware floor. 1.20 matches VPA's MemoryBumpUpRatio —
// large enough to escape the limit the kernel killed at, small enough to
// converge quickly when the workload's real need sits just above the prior
// limit.
const DefaultOOMBumpFactor = 1.20

// NewOOMSignal builds an OOMSignal with DefaultOOMBumpFactor pre-set.
// Callers that need LiveEventAt set it on the returned value directly.
func NewOOMSignal(recent bool, peakBytes, oomLimitBytes float64) OOMSignal {
	return OOMSignal{
		Recent:            recent,
		PeakBytes:         peakBytes,
		OOMTimeLimitBytes: oomLimitBytes,
		BumpFactor:        DefaultOOMBumpFactor,
	}
}

// MinCPURequest returns the hard floor applied to CPU recommendations.
func MinCPURequest() *resource.Quantity {
	return resource.NewMilliQuantity(minCPUMillicores, resource.DecimalSI)
}

// MinMemoryRequest returns the hard floor applied to memory recommendations.
func MinMemoryRequest() *resource.Quantity {
	return resource.NewQuantity(minMemoryMiB*mebibyte, resource.BinarySI)
}

// PercentileQuantile converts a percentile pointer (e.g. 95) to a
// Prometheus quantile float (0.95). Returns 0.95 when p is nil.
func PercentileQuantile(p *int32) float64 {
	if p == nil {
		return float64(defaultPercentile) / 100.0
	}
	return float64(*p) / 100.0
}

// ResourceWindow returns the window string or the default (168h) when empty.
func ResourceWindow(w string) string {
	if w == "" {
		return defaultWindow
	}
	return w
}

// LimitResult holds the outcome of a limit computation.
type LimitResult struct {
	// Quantity is the computed limit value. Nil means "keep the existing limit".
	Quantity *resource.Quantity
	// Remove, when true, signals that the limit should be deleted entirely.
	Remove bool
}

// cpuQuantity renders a CPU percentile (cores) at nanocore precision, so the
// trace shows the value headroom was applied to rather than a rounded one.
func cpuQuantity(cores float64) *resource.Quantity {
	return resource.NewScaledQuantity(int64(math.Round(cores*1e9)), resource.Nano)
}

// memoryQuantity renders a memory value (bytes) in whole bytes, as
// memoryWithHeadroom reads it.
func memoryQuantity(b float64) *resource.Quantity {
	return resource.NewQuantity(int64(b), resource.BinarySI)
}

// cpuWithHeadroom applies headroom to a CPU value (cores), rounding up to a
// whole millicore and raising it to the hard minimum.
func cpuWithHeadroom(rawCores float64, headroom *int32) resource.Quantity {
	milliCores := rawCores * 1000
	if headroom != nil && *headroom > 0 {
		milliCores *= 1.0 + float64(*headroom)/100.0
	}
	m := max(int64(math.Ceil(milliCores)), int64(minCPUMillicores))
	return *resource.NewMilliQuantity(m, resource.DecimalSI)
}

// memoryWithHeadroom applies headroom to a memory value (bytes). Arithmetic is
// done in integer bytes to avoid float64 drift, then rounded up to the nearest
// MiB for clean Kubernetes quantity values.
func memoryWithHeadroom(rawBytes float64, headroom *int32) resource.Quantity {
	// Truncate to integer bytes first; headroom provides the safety margin.
	b := int64(rawBytes)
	if headroom != nil && *headroom > 0 {
		b = b * int64(100+*headroom) / 100
	}
	mib := max((b+mebibyte-1)/mebibyte, int64(minMemoryMiB))
	return *resource.NewQuantity(mib*mebibyte, resource.BinarySI)
}

// cpuTrace runs a container's CPU percentile through headroom, the min/max
// clamp and autoscaler coordination. Nil when the Policy keeps the request.
func cpuTrace(in ContainerInputs) *sustainv1alpha1.ResourceTrace {
	cfg := in.RsCfg.CPU.Requests
	if cfg.KeepRequest {
		return nil
	}
	t := &sustainv1alpha1.ResourceTrace{
		Percentile:   cpuQuantity(in.CPUPerPod),
		WithHeadroom: cpuWithHeadroom(in.CPUPerPod, cfg.Headroom),
	}
	t.Clamped = clamp(t.WithHeadroom, cfg.MinAllowed, cfg.MaxAllowed)
	t.Coordination = coordinate(t.Clamped, autoscaler.ResourceCPU, in.CoordCfg, in.AutoInfo, cfg)
	return t
}

// memoryTrace runs a container's memory percentile through the OOM floor,
// headroom, the min/max clamp and autoscaler coordination. Nil when the Policy
// keeps the request.
func memoryTrace(in ContainerInputs) *sustainv1alpha1.ResourceTrace {
	cfg := in.RsCfg.Memory.Requests
	if cfg.KeepRequest {
		return nil
	}
	t := &sustainv1alpha1.ResourceTrace{}
	var effective float64
	if in.HasMemUsage {
		effective = in.MemPerPod
		t.Percentile = memoryQuantity(in.MemPerPod)
	}
	floorWins := false
	if in.OOM.recent() {
		floor := in.OOM.floor()
		t.OOMFloor = &sustainv1alpha1.OOMFloorTrace{Value: *memoryQuantity(floor)}
		if floor > effective {
			effective, floorWins = floor, true
		}
	}
	t.WithHeadroom = memoryWithHeadroom(effective, cfg.Headroom)
	t.Clamped = clamp(t.WithHeadroom, cfg.MinAllowed, cfg.MaxAllowed)
	t.Coordination = coordinate(t.Clamped, autoscaler.ResourceMemory, in.CoordCfg, in.AutoInfo, cfg)
	// Judged on the final request: a clamp on either side of coordination
	// means an operator bound, not the floor, set it.
	if floorWins {
		t.OOMFloor.Determined = t.Clamped.Cmp(t.WithHeadroom) == 0 &&
			(t.Coordination == nil || t.Coordination.Value.Cmp(t.Coordination.Scaled) == 0)
	}
	return t
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

// OOMSignal carries an OOM-aware floor for memory recommendations. Recent=true
// means the workload OOM'd within the lookback window (24h), in which case the
// recommendation is floored at max(PeakBytes, OOMTimeLimitBytes*BumpFactor)
// (with headroom applied once on top).
//
// Two anchors so we degrade gracefully:
//   - PeakBytes is the kernel high-water mark when cAdvisor manages to
//     observe it. Unreliable on cgroup v2 (sub-scrape OOM kills) but precise
//     when it works.
//   - OOMTimeLimitBytes is the cgroup limit captured at the moment the OOM
//     fired. Always available (it's the limit the kernel killed at), and the
//     BumpFactor multiplier pushes the recommendation above it.
//
// The container's current request is deliberately NOT a floor: it would
// compound the previous reco's already-headroomed value, growing the limit by
// (1 + headroom) per cycle even after the workload fits. The OOM-time-limit
// anchor only refreshes when a new OOM fires, so it converges. For a hard
// "never go below X", use cfg.MinAllowed.
//
// LiveEventAt is set when the active Pod watcher observed a kill; the floor
// logic treats it as equivalent to Recent so a bump does not wait for the
// recording rule to surface it.
type OOMSignal struct {
	Recent            bool
	PeakBytes         float64
	OOMTimeLimitBytes float64
	BumpFactor        float64
	LiveEventAt       time.Time
}

// recent reports whether the floor applies: an OOM within the lookback window
// or a kill the live watcher saw.
func (s OOMSignal) recent() bool {
	return s.Recent || !s.LiveEventAt.IsZero()
}

// floor is max(PeakBytes, OOMTimeLimitBytes*BumpFactor); a BumpFactor of 1 or
// less disables the OOM-time-limit anchor.
func (s OOMSignal) floor() float64 {
	floor := s.PeakBytes
	if s.BumpFactor > 1 && s.OOMTimeLimitBytes > 0 {
		floor = max(floor, s.OOMTimeLimitBytes*s.BumpFactor)
	}
	return floor
}

// ComputeLimit derives a resource limit from the computed request and the limit
// config. Returns LimitResult{} (keep existing) when no change is required.
func ComputeLimit(request *resource.Quantity, currentRequest, currentLimit *resource.Quantity, cfg sustainv1alpha1.ResourceLimitsConfig) LimitResult {
	if request == nil || cfg.KeepLimit {
		return LimitResult{}
	}
	if cfg.NoLimit {
		return LimitResult{Remove: true}
	}
	if cfg.EqualsToRequest {
		q := request.DeepCopy()
		return LimitResult{Quantity: &q}
	}
	if cfg.RequestsLimitsRatio != nil {
		return LimitResult{Quantity: scaleByRatio(request, *cfg.RequestsLimitsRatio)}
	}
	if cfg.KeepLimitRequestRatio && currentRequest != nil && currentLimit != nil && !currentRequest.IsZero() {
		ratio := float64(currentLimit.MilliValue()) / float64(currentRequest.MilliValue())
		return LimitResult{Quantity: scaleByRatio(request, ratio)}
	}
	return LimitResult{}
}

// scaleByRatio multiplies request by ratio, rounding up. Memory quantities
// (BinarySI) are scaled in whole bytes — milli math would carry float64
// representation error into fractional-byte limits (e.g. 100Mi * 1.1 →
// 115343360001 milli-bytes), which Kubernetes warns about in pod specs.
// CPU keeps millivalue precision.
func scaleByRatio(request *resource.Quantity, ratio float64) *resource.Quantity {
	if request.Format == resource.BinarySI {
		return resource.NewQuantity(
			int64(math.Ceil(float64(request.Value())*ratio)),
			request.Format,
		)
	}
	return resource.NewMilliQuantity(
		int64(math.Ceil(float64(request.MilliValue())*ratio)),
		request.Format,
	)
}

// clamp raises q to minQ and caps it at maxQ, maxQ winning a conflict.
func clamp(q resource.Quantity, minQ, maxQ *resource.Quantity) resource.Quantity {
	if minQ != nil && q.Cmp(*minQ) < 0 {
		q = minQ.DeepCopy()
	}
	if maxQ != nil && q.Cmp(*maxQ) > 0 {
		q = maxQ.DeepCopy()
	}
	return q
}
