package workload

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ContainerMatches reports whether a container's current resources already
// match the recommendation — i.e. whether applying rec to the container would
// leave its CPU/memory requests and limits unchanged.
//
// This is the single source of truth for the "is this container already at the
// recommended size?" question. Both the controller (deciding whether a
// workload's pods are stale and need recycling) and the patcher (deciding
// whether a live pod needs patching) MUST gate their recycle/resize decision on
// this predicate, so they never disagree (which would cause oscillation: one
// side recycles while the other skips).
//
// The semantics are defined by what applyRecToContainer actually DOES, so that
// ContainerMatches is true exactly when applying rec is a no-op on values:
//
//   - A nil rec field (CPURequest/MemoryRequest/CPULimit/MemoryLimit) means
//     "leave this dimension alone" — the patcher never touches it — so it can
//     never cause a mismatch.
//   - A non-nil request/limit rec matches iff the container's current value
//     equals it numerically. An UNSET request/limit reads as a zero Quantity
//     (ResourceList.Cpu()/Memory() return a zero-valued, non-nil Quantity when
//     the key is absent), so an unset value and an explicit zero value are
//     treated as equal — consistent with how Kubernetes itself compares them.
//   - RemoveCPULimit/RemoveMemoryLimit means "the limit should not be present".
//     It matches iff the current limit is unset/zero; any non-zero current
//     limit is a mismatch (applying would delete it).
//
// The comparison is numeric (resource.Quantity.Cmp), so an off-by-one-milli
// difference (e.g. 100m vs 101m) is a mismatch, while equal quantities written
// in different forms (e.g. 1000m vs 1) match.
func ContainerMatches(current corev1.ResourceRequirements, rec ContainerRecommendation) bool {
	if !requestMatches(current.Requests.Cpu(), rec.CPURequest) {
		return false
	}
	if !requestMatches(current.Requests.Memory(), rec.MemoryRequest) {
		return false
	}
	if !limitMatches(current.Limits.Cpu(), rec.CPULimit, rec.RemoveCPULimit) {
		return false
	}
	if !limitMatches(current.Limits.Memory(), rec.MemoryLimit, rec.RemoveMemoryLimit) {
		return false
	}
	return true
}

// requestMatches reports whether applying rec to current would leave the
// request unchanged. A nil rec means "leave alone" → always matches. current is
// never nil: ResourceList.Cpu()/Memory() return a zero Quantity when unset.
func requestMatches(current *resource.Quantity, rec *resource.Quantity) bool {
	if rec == nil {
		return true
	}
	return current.Cmp(*rec) == 0
}

// limitMatches reports whether applying the limit recommendation to current
// would leave the limit unchanged. remove=true matches iff the current limit is
// absent/zero; otherwise a nil rec means "leave alone" and a non-nil rec
// matches iff it equals current numerically. current is never nil.
func limitMatches(current *resource.Quantity, rec *resource.Quantity, remove bool) bool {
	if remove {
		return current.IsZero()
	}
	if rec == nil {
		return true
	}
	return current.Cmp(*rec) == 0
}

// DefaultDownsizePercent is the default percent used to compute the relative
// component of the downsize threshold band when a Policy does not configure one.
const DefaultDownsizePercent int32 = 5

// Default absolute floors of the downsize threshold band, per resource.
var (
	DefaultCPUDownsizeFloor    = resource.MustParse("10m")
	DefaultMemoryDownsizeFloor = resource.MustParse("15Mi")
)

// Tolerance carries the per-resource asymmetric downsize bands. The zero value
// disables suppression (band 0 => every decrease is acted on).
type Tolerance struct {
	CPUPercent int32
	CPUFloor   resource.Quantity
	MemPercent int32
	MemFloor   resource.Quantity
}

// withinDecrease reports whether rec is a DECREASE from current that is smaller
// than the band max(percent% of current, floor) and should therefore be left
// alone. A nil rec, an increase, or an exact match return false — those are
// handled unchanged downstream (nil = leave alone; increase/equal flow through
// ContainerMatches as today).
//
// Arithmetic is in milli-units (MilliValue) so CPU (millicores) and memory
// (milli-bytes) share one code path. percent is divided into current first to
// keep the multiplication well clear of int64 overflow for realistic sizes.
func withinDecrease(current, rec *resource.Quantity, percent int32, floor resource.Quantity) bool {
	if rec == nil {
		return false
	}
	delta := current.MilliValue() - rec.MilliValue()
	if delta <= 0 {
		return false // increase or exact match
	}
	// /100 before *percent keeps the product clear of int64 overflow; it rounds
	// the band down by sub-percent amounts, which errs toward acting (the floor
	// rescues small workloads where the rounding would otherwise bite).
	band := current.MilliValue() / 100 * int64(percent)
	if fm := floor.MilliValue(); fm > band {
		band = fm
	}
	return delta < band
}

// clampDecreaseToTolerance returns a copy of rec in which each dimension that is
// a sub-threshold decrease vs current is cleared to nil ("leave alone").
// Increases and above-threshold decreases pass through unchanged; limit
// removals are always significant and never cleared. Both ContainerMatches and
// applyRecToContainer treat the cleared (nil) dimensions as no-ops, so this is
// the single enforcement point for the asymmetric downsize threshold.
func clampDecreaseToTolerance(current corev1.ResourceRequirements, rec ContainerRecommendation, tol Tolerance) ContainerRecommendation {
	out := rec
	if withinDecrease(current.Requests.Cpu(), out.CPURequest, tol.CPUPercent, tol.CPUFloor) {
		out.CPURequest = nil
	}
	if !out.RemoveCPULimit && withinDecrease(current.Limits.Cpu(), out.CPULimit, tol.CPUPercent, tol.CPUFloor) {
		out.CPULimit = nil
	}
	if withinDecrease(current.Requests.Memory(), out.MemoryRequest, tol.MemPercent, tol.MemFloor) {
		out.MemoryRequest = nil
	}
	if !out.RemoveMemoryLimit && withinDecrease(current.Limits.Memory(), out.MemoryLimit, tol.MemPercent, tol.MemFloor) {
		out.MemoryLimit = nil
	}
	return out
}

// ClampRecsToTolerance applies clampDecreaseToTolerance to every recommendation
// whose container is present in current, keyed by container name. Recs whose
// container is not found pass through unchanged (tolerance can't be evaluated
// without a current value). The input map is never mutated.
func ClampRecsToTolerance(current []corev1.Container, recs map[string]ContainerRecommendation, tol Tolerance) map[string]ContainerRecommendation {
	if len(recs) == 0 {
		return recs
	}
	byName := make(map[string]corev1.Container, len(current))
	for _, c := range current {
		byName[c.Name] = c
	}
	out := make(map[string]ContainerRecommendation, len(recs))
	for name, rec := range recs {
		if c, ok := byName[name]; ok {
			out[name] = clampDecreaseToTolerance(c.Resources, rec, tol)
		} else {
			out[name] = rec
		}
	}
	return out
}

// reportSuppressed calls observe("cpu") and/or observe("memory") once each for
// any dimension present in orig but cleared in clamped. observe may be nil.
func reportSuppressed(orig, clamped ContainerRecommendation, observe func(resource string)) {
	if observe == nil {
		return
	}
	if (orig.CPURequest != nil && clamped.CPURequest == nil) || (orig.CPULimit != nil && clamped.CPULimit == nil) {
		observe("cpu")
	}
	if (orig.MemoryRequest != nil && clamped.MemoryRequest == nil) || (orig.MemoryLimit != nil && clamped.MemoryLimit == nil) {
		observe("memory")
	}
}

// observeSuppressed reports every dimension cleared between orig and clamped
// (same keys) through observe. observe may be nil.
func observeSuppressed(orig, clamped map[string]ContainerRecommendation, observe func(resource string)) {
	if observe == nil {
		return
	}
	for name, o := range orig {
		reportSuppressed(o, clamped[name], observe)
	}
}
