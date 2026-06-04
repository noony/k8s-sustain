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
