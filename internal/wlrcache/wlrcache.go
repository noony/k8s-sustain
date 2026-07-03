// Package wlrcache centralizes writes of WorkloadRecommendation cache
// objects. Two writers share it: the controller (every reconcile) and the
// admission webhook (pod creation, for ephemeral identities that can live
// and die entirely between two reconciles). A single implementation keeps
// object naming, no-op write suppression, and the observed-resources
// snapshot identical across both — the webhook fallback contract breaks
// silently if naming ever diverges.
package wlrcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// maxNameLength is the Kubernetes object-name limit (DNS subdomain).
const maxNameLength = 253

// Name builds the WorkloadRecommendation object name for a workload
// identity. Format: "<lowercase-kind>-<name>"; names exceeding the 253-char
// limit are truncated with a short stable hash appended.
func Name(kind, name string) string {
	n := fmt.Sprintf("%s-%s", strings.ToLower(kind), name)
	if len(n) <= maxNameLength {
		return n
	}
	sum := sha256.Sum256([]byte(n))
	hash := hex.EncodeToString(sum[:])[:10]
	return n[:maxNameLength-len(hash)-1] + "-" + hash
}

// RefreshInterval bounds how long an unchanged WorkloadRecommendation status
// may keep its old ObservedAt before a writer rewrites it just to bump the
// timestamp. Must stay well under the webhook's DefaultCacheStaleness (30m).
const RefreshInterval = 10 * time.Minute

// Upsert writes (or updates) the WorkloadRecommendation for ref. Idempotent:
// if the existing status matches, no API call is made (subject to
// RefreshInterval for the ObservedAt bump). Best-effort: errors are logged
// at V(1) and never returned — the cache is a fallback/visibility path, not
// load-bearing.
func Upsert(
	ctx context.Context,
	c client.Client,
	ref sustainv1alpha1.WorkloadReference,
	policyName string,
	recs map[string]workload.ContainerRecommendation,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
	now metav1.Time,
) {
	logger := log.FromContext(ctx).WithValues("kind", ref.Kind, "name", ref.Name, "namespace", ref.Namespace)

	desired := buildStatus(recs, observed, now)
	if len(desired.Containers) == 0 {
		return
	}

	key := types.NamespacedName{Namespace: ref.Namespace, Name: Name(ref.Kind, ref.Name)}
	var existing sustainv1alpha1.WorkloadRecommendation
	err := c.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		obj := &sustainv1alpha1.WorkloadRecommendation{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace,
				Name:      key.Name,
				Labels:    map[string]string{sustainv1alpha1.WLRPolicyLabel: policyName},
			},
			Spec: sustainv1alpha1.WorkloadRecommendationSpec{WorkloadRef: ref, Policy: policyName},
		}
		if err := c.Create(ctx, obj); err != nil {
			logger.V(1).Info("failed to create WorkloadRecommendation; skipping cache write", "err", err)
			return
		}
		if err := c.Get(ctx, key, &existing); err != nil {
			logger.V(1).Info("failed to re-read WorkloadRecommendation after create", "err", err)
			return
		}
	} else if err != nil {
		logger.V(1).Info("failed to read WorkloadRecommendation", "err", err)
		return
	}

	if existing.Spec.WorkloadRef != ref || existing.Spec.Policy != policyName ||
		existing.Labels[sustainv1alpha1.WLRPolicyLabel] != policyName {
		patched := existing.DeepCopy()
		patched.Spec.WorkloadRef = ref
		patched.Spec.Policy = policyName
		if patched.Labels == nil {
			patched.Labels = map[string]string{}
		}
		patched.Labels[sustainv1alpha1.WLRPolicyLabel] = policyName
		if err := c.Patch(ctx, patched, client.MergeFrom(&existing)); err != nil {
			logger.V(1).Info("failed to patch WorkloadRecommendation spec", "err", err)
			return
		}
		existing = *patched
	}

	if statusEquivalent(existing.Status, desired) &&
		now.Sub(existing.Status.ObservedAt.Time) < RefreshInterval {
		return
	}

	patched := existing.DeepCopy()
	patched.Status = desired
	if err := c.Status().Patch(ctx, patched, client.MergeFrom(&existing)); err != nil {
		logger.V(1).Info("failed to patch WorkloadRecommendation status", "err", err)
	}
}

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

// quantityFrom returns a copy of rl[name] as a pointer, nil when absent.
func quantityFrom(rl corev1.ResourceList, name corev1.ResourceName) *resource.Quantity {
	q, ok := rl[name]
	if !ok {
		return nil
	}
	return &q
}

// buildStatus converts the in-memory recommendation map into the CRD shape.
func buildStatus(
	recs map[string]workload.ContainerRecommendation,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
	now metav1.Time,
) sustainv1alpha1.WorkloadRecommendationStatus {
	out := sustainv1alpha1.WorkloadRecommendationStatus{
		ObservedAt:        now,
		Source:            "prometheus",
		Containers:        map[string]sustainv1alpha1.ContainerRecommendation{},
		ObservedResources: observed,
	}
	for name, rec := range recs {
		out.Containers[name] = sustainv1alpha1.ContainerRecommendation{
			CPURequest:        rec.CPURequest,
			MemoryRequest:     rec.MemoryRequest,
			CPULimit:          rec.CPULimit,
			MemoryLimit:       rec.MemoryLimit,
			RemoveCPULimit:    rec.RemoveCPULimit,
			RemoveMemoryLimit: rec.RemoveMemoryLimit,
		}
	}
	return out
}

// statusEquivalent reports whether two WLR statuses convey the same
// recommendation values, ignoring ObservedAt. Used to suppress no-op writes
// so write amplification scales with *change*, not workload count.
func statusEquivalent(a, b sustainv1alpha1.WorkloadRecommendationStatus) bool {
	if a.Source != b.Source {
		return false
	}
	if len(a.Containers) != len(b.Containers) {
		return false
	}
	for name, av := range a.Containers {
		bv, ok := b.Containers[name]
		if !ok {
			return false
		}
		if !quantityEqual(av.CPURequest, bv.CPURequest) ||
			!quantityEqual(av.MemoryRequest, bv.MemoryRequest) ||
			!quantityEqual(av.CPULimit, bv.CPULimit) ||
			!quantityEqual(av.MemoryLimit, bv.MemoryLimit) ||
			av.RemoveCPULimit != bv.RemoveCPULimit ||
			av.RemoveMemoryLimit != bv.RemoveMemoryLimit {
			return false
		}
	}

	if len(a.ObservedResources) != len(b.ObservedResources) {
		return false
	}
	for name, av := range a.ObservedResources {
		bv, ok := b.ObservedResources[name]
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

// quantityEqual reports whether two stored recommendation quantities are
// equal, treating a nil pointer and an explicit zero as the same "unset"
// value. Copied from internal/controller (rather than shared) because that
// package's own quantityEqual (internal/controller/diff.go) has other
// in-package callers/tests and comparing a stored recommendation against
// another stored recommendation is a distinct concern from
// workload.ContainerMatches (live container vs. recommendation).
func quantityEqual(a, b *resource.Quantity) bool {
	aZero := a == nil || a.IsZero()
	bZero := b == nil || b.IsZero()
	if aZero || bZero {
		return aZero == bZero
	}
	return a.Cmp(*b) == 0
}
