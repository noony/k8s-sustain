// Package wlrcache owns the WorkloadRecommendation lifecycle. Every write goes
// through it, and so does every verdict on a stored object; callers never patch
// a WorkloadRecommendation themselves, so which writer owns which field is
// decided in one place:
//
//   - Request is the webhook's ask for an identity it admits a pod of: it
//     creates the object when absent, with the pod's container snapshot.
//   - Ensure is the governing Policy's claim on a live identity, each cycle: it
//     creates or adopts the object, refreshes the snapshot and clears departed.
//   - Record stores one pass's decision for the identity, departed included.
//     It alone writes the Recommendation, its trace, observedAt, the outcome
//     and computedBy, the Policy the Recommendation is served to (ADR 0003).
//   - Read is what a stored object means to a pod being admitted, and Expired
//     whether a sweep deletes it.
//
// # Never re-read after a write
//
// Every writer runs against a CACHE-BACKED client, so a Get issued right after
// a Create races the informer's watch event and reliably returns NotFound. The
// result is not a retried write but an object stranded half-written — for a
// once-a-day bare pod, for a day. So nothing re-reads an object it just wrote:
// Create and Patch return the stored object, each write patches off the object
// the previous one returned, and Ensure hands its object to Record. A Get is
// reserved for the initial lookup; a Create that loses the race to another
// writer claims that writer's object with a patch, whose response carries it.
// The tests use a lagging-reader interceptor because fake.NewClientBuilder is
// read-your-writes and cannot express cache lag.
package wlrcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// maxNameLength is the Kubernetes object-name limit (DNS subdomain).
const maxNameLength = 253

// Name builds the WorkloadRecommendation object name for a workload identity:
// "<lowercase-kind>-<name>", truncated with a short stable hash when it exceeds
// the 253-char limit.
func Name(kind, name string) string {
	n := fmt.Sprintf("%s-%s", strings.ToLower(kind), name)
	if len(n) <= maxNameLength {
		return n
	}
	sum := sha256.Sum256([]byte(n))
	hash := hex.EncodeToString(sum[:])[:10]
	return n[:maxNameLength-len(hash)-1] + "-" + hash
}

func keyOf(ref sustainv1alpha1.WorkloadReference) types.NamespacedName {
	return types.NamespacedName{Namespace: ref.Namespace, Name: Name(ref.Kind, ref.Name)}
}

// RefreshInterval bounds how long an unchanged WorkloadRecommendation status
// may keep its old ObservedAt before a writer rewrites it just to bump the
// timestamp. Must stay well under DefaultStaleness.
const RefreshInterval = 10 * time.Minute

// DefaultStaleness is the default bound on the age of a Recommendation the
// controller keeps refreshing: one full reconcile interval (5m) plus headroom
// for a backed-up controller and small clock skew.
const DefaultStaleness = 30 * time.Minute

// DefaultRetention is the default --recommendation-retention, shared by the
// controller, which keeps a Departed or Conflicted identity's object that
// long, and the webhook, which stops serving it after that long: two literals
// could drift into a window where the webhook serves what the controller
// considers expired.
const DefaultRetention = 168 * time.Hour

// BuildObservedResources snapshots per-container requests/limits so the
// recommendation record keeps showing what the workload actually ran with
// after its object is deleted.
func BuildObservedResources(containers, initContainers []corev1.Container) map[string]sustainv1alpha1.ObservedContainerResources {
	out := make(map[string]sustainv1alpha1.ObservedContainerResources, len(containers)+len(initContainers))
	add := func(cs []corev1.Container, init bool) {
		for _, c := range cs {
			out[c.Name] = sustainv1alpha1.ObservedContainerResources{
				Init:          init,
				CPURequest:    quantityFrom(c.Resources.Requests, corev1.ResourceCPU),
				MemoryRequest: quantityFrom(c.Resources.Requests, corev1.ResourceMemory),
				CPULimit:      quantityFrom(c.Resources.Limits, corev1.ResourceCPU),
				MemoryLimit:   quantityFrom(c.Resources.Limits, corev1.ResourceMemory),
			}
		}
	}
	add(containers, false)
	add(initContainers, true)
	return out
}

func quantityFrom(rl corev1.ResourceList, name corev1.ResourceName) *resource.Quantity {
	q, ok := rl[name]
	if !ok {
		return nil
	}
	return &q
}

// ContainersFromObserved rebuilds the container lists from an observed-
// resources snapshot, the only source left once the workload object is gone.
// Both lists are sorted by name; map order is random.
func ContainersFromObserved(obs map[string]sustainv1alpha1.ObservedContainerResources) (containers, initContainers []corev1.Container) {
	for name, o := range obs {
		c := corev1.Container{Name: name}
		setQuantity(&c.Resources.Requests, corev1.ResourceCPU, o.CPURequest)
		setQuantity(&c.Resources.Requests, corev1.ResourceMemory, o.MemoryRequest)
		setQuantity(&c.Resources.Limits, corev1.ResourceCPU, o.CPULimit)
		setQuantity(&c.Resources.Limits, corev1.ResourceMemory, o.MemoryLimit)
		if o.Init {
			initContainers = append(initContainers, c)
		} else {
			containers = append(containers, c)
		}
	}
	byName := func(a, b corev1.Container) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(containers, byName)
	slices.SortFunc(initContainers, byName)
	return containers, initContainers
}

func setQuantity(rl *corev1.ResourceList, name corev1.ResourceName, q *resource.Quantity) {
	if q == nil {
		return
	}
	if *rl == nil {
		*rl = corev1.ResourceList{}
	}
	(*rl)[name] = *q
}

// observedEqual compares two snapshots container by container.
func observedEqual(a, b map[string]sustainv1alpha1.ObservedContainerResources) bool {
	if len(a) != len(b) {
		return false
	}
	for name, av := range a {
		bv, ok := b[name]
		if !ok || av.Init != bv.Init ||
			!quantityEqual(av.CPURequest, bv.CPURequest) ||
			!quantityEqual(av.MemoryRequest, bv.MemoryRequest) ||
			!quantityEqual(av.CPULimit, bv.CPULimit) ||
			!quantityEqual(av.MemoryLimit, bv.MemoryLimit) {
			return false
		}
	}
	return true
}

// statusEquivalent compares two statuses ignoring ObservedAt and Trace, so
// write amplification scales with change rather than workload count. The
// trace moves with every sample (a percentile that rounds to the same request)
// and is only worth a write together with the values it explains.
func statusEquivalent(a, b sustainv1alpha1.WorkloadRecommendationStatus) bool {
	if a.Outcome != b.Outcome || a.ComputedBy != b.ComputedBy || a.Departed != b.Departed ||
		len(a.Containers) != len(b.Containers) {
		return false
	}
	for name, av := range a.Containers {
		bv, ok := b.Containers[name]
		if !ok ||
			!quantityEqual(av.CPURequest, bv.CPURequest) ||
			!quantityEqual(av.MemoryRequest, bv.MemoryRequest) ||
			!quantityEqual(av.CPULimit, bv.CPULimit) ||
			!quantityEqual(av.MemoryLimit, bv.MemoryLimit) ||
			av.RemoveCPULimit != bv.RemoveCPULimit ||
			av.RemoveMemoryLimit != bv.RemoveMemoryLimit {
			return false
		}
	}
	return observedEqual(a.ObservedResources, b.ObservedResources)
}

// quantityEqual treats a nil pointer and an explicit zero as the same "unset"
// value. Deliberately not shared with internal/controller/diff.go: comparing two
// stored recommendations is a distinct concern from comparing a live container
// against one.
func quantityEqual(a, b *resource.Quantity) bool {
	aZero := a == nil || a.IsZero()
	bZero := b == nil || b.IsZero()
	if aZero || bZero {
		return aZero == bZero
	}
	return a.Cmp(*b) == 0
}
