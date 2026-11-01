package workload

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// podChange is what applying a Recommendation changes on a pod, ordered so
// the larger of two changes is the pod's.
type podChange int

const (
	noChange podChange = iota
	// limitChange: the requests already match and only limits differ. It
	// never evicts a pod (docs/adr/0009).
	limitChange
	// requestChange: a request differs. A limit the guard raises along with
	// a rising request is part of it, not a change to limits alone.
	requestChange
)

// containerChange is what applying rec to a container changes in its
// CPU/memory requests and limits, the limit Inject raises to a request
// included.
//
// It is the single source of truth for "is this container already at the
// recommended size?": the stale count and the resize decision both gate on
// it, or they oscillate against each other.
//
// A nil rec field means "leave alone" and never changes anything. An unset
// request/limit reads as a zero Quantity, so unset and explicit zero compare
// equal. Remove*Limit changes nothing when the current limit is unset or zero.
// Comparison is numeric, so 1000m and 1 match while 100m and 101m do not.
func containerChange(current corev1.ResourceRequirements, rec ContainerRecommendation) podChange {
	out, _, changed := applyTo(current, rec)
	switch {
	case !changed:
		return noChange
	case !requestsEqual(current, out):
		return requestChange
	}
	return limitChange
}

// podPlan is what applying a Recommendation means for one pod. Both what the
// pass applies to a pod and whether the pod counts stale read it, so the two
// cannot disagree.
type podPlan struct {
	// recs is the Recommendation with the decreases the tolerance withholds
	// on this pod cleared.
	recs   map[string]ContainerRecommendation
	change podChange
}

// planFor plans applying recs to pod under tol. Only regular and sidecar
// containers count: a classic init container has already exited.
func planFor(pod *corev1.Pod, recs map[string]ContainerRecommendation, tol Tolerance) podPlan {
	pl := podPlan{recs: clampRecsToTolerance(podContainers(pod), recs, tol)}
	for _, c := range pod.Spec.Containers {
		if rec, ok := pl.recs[c.Name]; ok {
			pl.change = max(pl.change, containerChange(c.Resources, rec))
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if rec, ok := pl.recs[c.Name]; ok && isRestartableInitContainer(c) {
			pl.change = max(pl.change, containerChange(c.Resources, rec))
		}
	}
	return pl
}

// stale reports whether the pod counts stale: a request differs from the
// Recommendation by more than the Policy tolerates. A change to limits alone
// is resized in place or waits for the pod's replacement, and like a
// suppressed decrease it is not drift.
func (pl podPlan) stale() bool { return pl.change == requestChange }

// Default downsize threshold of a Policy that configures none: 5% of the
// current value, at least 10m CPU or 15Mi memory.
const defaultDownsizePercent int32 = 5

var (
	defaultCPUDownsizeFloor    = resource.MustParse("10m")
	defaultMemoryDownsizeFloor = resource.MustParse("15Mi")
)

// Tolerance carries the per-resource asymmetric downsize bands. The zero value
// disables suppression (band 0 => every decrease is acted on).
type Tolerance struct {
	CPUPercent int32
	CPUFloor   resource.Quantity
	MemPercent int32
	MemFloor   resource.Quantity
}

// toleranceOf resolves a Policy's per-resource downsize thresholds, each unset
// value taking its default. Setting both percent and minDecrease to 0 acts on
// every decrease of that resource.
func toleranceOf(rs sustainv1alpha1.ResourcesConfigs) Tolerance {
	cp, cf := downsizeThreshold(rs.CPU.DownsizeThreshold, defaultCPUDownsizeFloor)
	mp, mf := downsizeThreshold(rs.Memory.DownsizeThreshold, defaultMemoryDownsizeFloor)
	return Tolerance{CPUPercent: cp, CPUFloor: cf, MemPercent: mp, MemFloor: mf}
}

func downsizeThreshold(t *sustainv1alpha1.DownsizeThreshold, defFloor resource.Quantity) (int32, resource.Quantity) {
	pct, floor := defaultDownsizePercent, defFloor
	if t != nil {
		if t.Percent != nil {
			pct = *t.Percent
		}
		if t.MinDecrease != nil {
			floor = *t.MinDecrease
		}
	}
	return pct, floor
}

// withinDecrease reports whether rec is a DECREASE from current that is smaller
// than the band max(percent% of current, floor) and should therefore be left
// alone. A nil rec, an increase, or an exact match return false — those are
// handled unchanged downstream (nil = leave alone; increase/equal flow through
// containerChange).
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
// removals are always significant and never cleared. containerChange and
// Inject treat the cleared (nil) dimensions as no-ops, so this is the single
// enforcement point for the asymmetric downsize threshold.
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

// clampRecsToTolerance applies clampDecreaseToTolerance to every recommendation
// whose container is present in current, keyed by container name. Recs whose
// container is not found pass through unchanged (tolerance can't be evaluated
// without a current value). The input map is never mutated.
func clampRecsToTolerance(current []corev1.Container, recs map[string]ContainerRecommendation, tol Tolerance) map[string]ContainerRecommendation {
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

// observeSuppressed reports every dimension cleared between orig and clamped
// (same keys) through observe — observe("cpu") and/or observe("memory") once
// each for any dimension present in orig but cleared in clamped. observe may be
// nil.
func observeSuppressed(orig, clamped map[string]ContainerRecommendation, observe func(resource string)) {
	if observe == nil {
		return
	}
	for name, o := range orig {
		c := clamped[name]
		if (o.CPURequest != nil && c.CPURequest == nil) || (o.CPULimit != nil && c.CPULimit == nil) {
			observe("cpu")
		}
		if (o.MemoryRequest != nil && c.MemoryRequest == nil) || (o.MemoryLimit != nil && c.MemoryLimit == nil) {
			observe("memory")
		}
	}
}
