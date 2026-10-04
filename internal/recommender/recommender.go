package recommender

import (
	"math"

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

// resourceKind is one of the two resources a Recommendation sizes. Its value
// is the key autoscaler.Info uses for the resource.
type resourceKind string

const (
	cpuResource    resourceKind = autoscaler.ResourceCPU
	memoryResource resourceKind = autoscaler.ResourceMemory
)

// config is the Policy's configuration of the resource.
func (r resourceKind) config(cfg sustainv1alpha1.ResourcesConfigs) sustainv1alpha1.ResourceConfig {
	if r == cpuResource {
		return cfg.CPU
	}
	return cfg.Memory
}

// quantity renders a value (cores or bytes) for the trace: CPU at nanocore
// precision, so the trace shows the value headroom was applied to rather than
// a rounded one, and memory in whole bytes, as withHeadroom reads it.
func (r resourceKind) quantity(v float64) *resource.Quantity {
	if r == cpuResource {
		return resource.NewScaledQuantity(int64(math.Round(v*1e9)), resource.Nano)
	}
	return resource.NewQuantity(int64(v), resource.BinarySI)
}

// withHeadroom applies headroom to a value (cores or bytes), rounds it up to a
// whole millicore or MiB, and raises it to the hard minimum. Memory arithmetic
// is done in integer bytes to avoid float64 drift.
func (r resourceKind) withHeadroom(v float64, headroom *int32) resource.Quantity {
	if r == cpuResource {
		milliCores := v * 1000
		if headroom != nil && *headroom > 0 {
			milliCores *= 1.0 + float64(*headroom)/100.0
		}
		m := max(int64(math.Ceil(milliCores)), int64(minCPUMillicores))
		return *resource.NewMilliQuantity(m, resource.DecimalSI)
	}
	// Truncate to integer bytes first; headroom provides the safety margin.
	b := int64(v)
	if headroom != nil && *headroom > 0 {
		b = b * int64(100+*headroom) / 100
	}
	mib := max((b+mebibyte-1)/mebibyte, int64(minMemoryMiB))
	return *resource.NewQuantity(mib*mebibyte, resource.BinarySI)
}

// LimitResult holds the outcome of a limit computation.
type LimitResult struct {
	// Quantity is the computed limit value. Nil means "keep the existing limit".
	Quantity *resource.Quantity
	// Remove, when true, signals that the limit should be deleted entirely.
	Remove bool
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
