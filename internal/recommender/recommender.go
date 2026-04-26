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
