package recommender

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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

// ComputeCPURequest applies headroom and min/max clamping to a raw CPU percentile
// value (cores). Returns nil when KeepRequest is true.
func ComputeCPURequest(rawCores float64, cfg sustainv1alpha1.ResourceRequestsConfig) *resource.Quantity {
	if cfg.KeepRequest {
		return nil
	}

	milliCores := rawCores * 1000
	if cfg.Headroom != nil && *cfg.Headroom > 0 {
		milliCores *= 1.0 + float64(*cfg.Headroom)/100.0
	}

	m := max(int64(math.Ceil(milliCores)), int64(minCPUMillicores))
	qty := resource.NewMilliQuantity(m, resource.DecimalSI)
	clampQuantity(qty, cfg.MinAllowed, cfg.MaxAllowed)
	return qty
}

// ComputeMemoryRequest applies headroom and min/max clamping to a raw memory
// percentile value (bytes). Returns nil when KeepRequest is true.
// Arithmetic is done in integer bytes to avoid float64 drift, then rounded up
// to the nearest MiB for clean Kubernetes quantity values.
func ComputeMemoryRequest(rawBytes float64, cfg sustainv1alpha1.ResourceRequestsConfig) *resource.Quantity {
	if cfg.KeepRequest {
		return nil
	}

	// Truncate to integer bytes first; headroom provides the safety margin.
	b := int64(rawBytes)
	if cfg.Headroom != nil && *cfg.Headroom > 0 {
		b = b * int64(100+*cfg.Headroom) / 100
	}

	// Round up to the nearest MiB.
	mib := max((b+mebibyte-1)/mebibyte, int64(minMemoryMiB))
	qty := resource.NewQuantity(mib*mebibyte, resource.BinarySI)
	clampQuantity(qty, cfg.MinAllowed, cfg.MaxAllowed)
	return qty
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
// CurrentRequestBytes is intentionally NOT a floor: doing so would compound
// the previous reco's already-headroomed value on each reconcile, causing the
// limit to grow by `(1 + headroom)` per cycle even after the workload fits.
// The OOM-time-limit anchor avoids this because it only refreshes when a NEW
// OOM event fires; once the workload fits after a bump, no new OOM events
// occur and the recorded limit stays at the pre-bump value.
// To enforce a hard "never go below X", use cfg.MinAllowed.
type OOMSignal struct {
	Recent              bool
	PeakBytes           float64
	OOMTimeLimitBytes   float64
	BumpFactor          float64
	CurrentRequestBytes float64
}

// ComputeMemoryRequestWithOOM is ComputeMemoryRequest with an OOM-aware floor.
// When signal.Recent is true, the result is floored at PeakBytes (with
// headroom applied once). MinAllowed/MaxAllowed clamps still apply last so
// user overrides win.
func ComputeMemoryRequestWithOOM(rawBytes float64, signal OOMSignal, cfg sustainv1alpha1.ResourceRequestsConfig) *resource.Quantity {
	q, _ := ComputeMemoryRequestWithOOMFloorReport(rawBytes, signal, cfg)
	return q
}

// ComputeMemoryRequestWithOOMFloorReport is the same as ComputeMemoryRequestWithOOM
// but also reports whether the OOM floor produced the final value. Used by the
// controller to emit a metric when the floor is applied.
//
// floorApplied is true only when the floor actually drove the final returned
// quantity: floor must beat the raw percentile AND MaxAllowed must not have
// clamped the result below the floor's (headroomed, MiB-rounded) value. If
// MaxAllowed clamps below the floor, the user override produced the value and
// floorApplied is false.
func ComputeMemoryRequestWithOOMFloorReport(rawBytes float64, signal OOMSignal, cfg sustainv1alpha1.ResourceRequestsConfig) (*resource.Quantity, bool) {
	if cfg.KeepRequest {
		return nil, false
	}
	effective := rawBytes
	floorWins := false
	if signal.Recent {
		floor := signal.PeakBytes
		if signal.BumpFactor > 1 && signal.OOMTimeLimitBytes > 0 {
			if bumped := signal.OOMTimeLimitBytes * signal.BumpFactor; bumped > floor {
				floor = bumped
			}
		}
		if floor > effective {
			effective = floor
			floorWins = true
		}
	}
	finalQty := ComputeMemoryRequest(effective, cfg)
	if !floorWins {
		return finalQty, false
	}
	// Floor beat the raw percentile, but MaxAllowed may still have clamped
	// the result below the floor-derived value. Recompute the unclamped
	// floor-with-headroom quantity and only report floorApplied when the
	// final quantity is at least that value.
	floorQty := ComputeMemoryRequest(effective, sustainv1alpha1.ResourceRequestsConfig{Headroom: cfg.Headroom})
	floorApplied := finalQty != nil && floorQty != nil && finalQty.Cmp(*floorQty) >= 0
	return finalQty, floorApplied
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
		q := resource.NewMilliQuantity(
			int64(math.Ceil(float64(request.MilliValue())**cfg.RequestsLimitsRatio)),
			request.Format,
		)
		return LimitResult{Quantity: q}
	}
	if cfg.KeepLimitRequestRatio && currentRequest != nil && currentLimit != nil && !currentRequest.IsZero() {
		ratio := float64(currentLimit.MilliValue()) / float64(currentRequest.MilliValue())
		q := resource.NewMilliQuantity(
			int64(math.Ceil(float64(request.MilliValue())*ratio)),
			request.Format,
		)
		return LimitResult{Quantity: q}
	}
	return LimitResult{}
}

func clampQuantity(qty *resource.Quantity, min, max *resource.Quantity) {
	if min != nil && qty.Cmp(*min) < 0 {
		*qty = min.DeepCopy()
	}
	if max != nil && qty.Cmp(*max) > 0 {
		*qty = max.DeepCopy()
	}
}
