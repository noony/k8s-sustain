// Package wlrcache centralizes writes of WorkloadRecommendation cache objects.
// The controller (every reconcile) and the webhook (pod creation, for ephemeral
// identities that live and die between two reconciles) share it so object
// naming, no-op suppression and the observed-resources snapshot cannot diverge
// — the webhook fallback contract breaks silently if naming does.
//
// # Never re-read after a successful Create
//
// Both writers run against a CACHE-BACKED client, so a Get issued right after a
// successful Create races the informer's watch event and reliably returns
// NotFound. The result is not a retried write but an object stranded with an
// empty status.observedResources, which the computation phase then skips — for
// a once-a-day bare pod, for a day. So Create populates the passed object in
// place and every function below patches status off that object; a Get is
// reserved for the paths where somebody else wrote the object (the initial
// lookup, the AlreadyExists branch). The tests use a lagging-reader interceptor
// because fake.NewClientBuilder is read-your-writes and cannot express cache lag.
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

// RefreshInterval bounds how long an unchanged WorkloadRecommendation status
// may keep its old ObservedAt before a writer rewrites it just to bump the
// timestamp. Must stay well under the webhook's DefaultCacheStaleness (30m).
const RefreshInterval = 10 * time.Minute

// Upsert writes (or updates) the WorkloadRecommendation for ref. Idempotent: an
// unchanged status makes no API call, subject to RefreshInterval.
//
// Every failure is both logged at V(1) and returned. The reconcile path may
// ignore the result — a failed cache write does not invalidate the recycle it
// accompanies — but the departed-refresh path must not: there the write IS the
// deliverable, and swallowing the error would have
// k8s_sustain_wlr_refresh_total count an unwritten recommendation as computed.
func Upsert(
	ctx context.Context,
	c client.Client,
	ref sustainv1alpha1.WorkloadReference,
	policyName string,
	recs map[string]workload.ContainerRecommendation,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
	now metav1.Time,
) error {
	logger := log.FromContext(ctx).WithValues("kind", ref.Kind, "name", ref.Name, "namespace", ref.Namespace)

	desired := buildStatus(recs, observed, now)
	if len(desired.Containers) == 0 {
		return nil
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
			return fmt.Errorf("creating WorkloadRecommendation %s: %w", key, err)
		}
		// No re-read: see the read-after-write note on this package.
		existing = *obj
	} else if err != nil {
		logger.V(1).Info("failed to read WorkloadRecommendation", "err", err)
		return fmt.Errorf("reading WorkloadRecommendation %s: %w", key, err)
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
			return fmt.Errorf("patching WorkloadRecommendation %s spec: %w", key, err)
		}
		existing = *patched
	}

	if statusEquivalent(existing.Status, desired) &&
		now.Sub(existing.Status.ObservedAt.Time) < RefreshInterval {
		return nil
	}

	patched := existing.DeepCopy()
	patched.Status = desired
	if err := c.Status().Patch(ctx, patched, client.MergeFrom(&existing)); err != nil {
		logger.V(1).Info("failed to patch WorkloadRecommendation status", "err", err)
		return fmt.Errorf("patching WorkloadRecommendation %s status: %w", key, err)
	}
	return nil
}

// EnsureExists creates the WorkloadRecommendation for ref if missing and keeps
// its spec, policy label and observed-resources snapshot current. It never
// writes Containers, Source or ObservedAt.
//
// It is the discovery half of the write path: Upsert deliberately refuses to
// create an object with no recommendation in it, but under WLR-driven
// computation the object must exist BEFORE anything can compute it. Clearing
// Departed here makes discovery the authority on that flag — an identity in a
// target listing is by definition not departed.
func EnsureExists(
	ctx context.Context,
	c client.Client,
	ref sustainv1alpha1.WorkloadReference,
	policyName string,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
) error {
	logger := log.FromContext(ctx).WithValues("kind", ref.Kind, "name", ref.Name, "namespace", ref.Namespace)
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
		if cErr := c.Create(ctx, obj); cErr != nil {
			if !apierrors.IsAlreadyExists(cErr) {
				logger.V(1).Info("failed to create WorkloadRecommendation", "err", cErr)
				return fmt.Errorf("creating WorkloadRecommendation %s: %w", key, cErr)
			}
			// Raced another writer (the webhook's stub creation). Its object is
			// equivalent, but only a read can say what status it already
			// carries, so this is the one branch that re-reads.
			if gErr := c.Get(ctx, key, &existing); gErr != nil {
				logger.V(1).Info("failed to re-read WorkloadRecommendation after create race", "err", gErr)
				return fmt.Errorf("re-reading WorkloadRecommendation %s after create race: %w", key, gErr)
			}
		} else {
			// No re-read: see the read-after-write note on this package.
			existing = *obj
		}
	} else if err != nil {
		logger.V(1).Info("failed to read WorkloadRecommendation", "err", err)
		return fmt.Errorf("reading WorkloadRecommendation %s: %w", key, err)
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
		if pErr := c.Patch(ctx, patched, client.MergeFrom(&existing)); pErr != nil {
			logger.V(1).Info("failed to patch WorkloadRecommendation spec", "err", pErr)
			return fmt.Errorf("patching WorkloadRecommendation %s spec: %w", key, pErr)
		}
		existing = *patched
	}

	if !existing.Status.Departed && observedEquivalent(existing.Status.ObservedResources, observed) {
		return nil
	}
	patched := existing.DeepCopy()
	patched.Status.Departed = false
	if observed != nil {
		patched.Status.ObservedResources = observed
	}
	if pErr := c.Status().Patch(ctx, patched, client.MergeFrom(&existing)); pErr != nil {
		logger.V(1).Info("failed to patch WorkloadRecommendation status", "err", pErr)
		return fmt.Errorf("patching WorkloadRecommendation %s status: %w", key, pErr)
	}
	return nil
}

// MarkNoData records that a computation produced nothing for an identity that
// has never produced anything.
//
// The no-op when Containers is already populated is the whole contract: every
// identity is recomputed every cycle, including departed ones whose samples
// eventually age out of the query window, and overwriting a retained
// last-known-good would strip exactly the recommendation retention exists to
// preserve. The state is NOT terminal — the next cycle recomputes.
func MarkNoData(
	ctx context.Context,
	c client.Client,
	ref sustainv1alpha1.WorkloadReference,
	now metav1.Time,
) error {
	logger := log.FromContext(ctx).WithValues("kind", ref.Kind, "name", ref.Name, "namespace", ref.Namespace)
	key := types.NamespacedName{Namespace: ref.Namespace, Name: Name(ref.Kind, ref.Name)}
	var existing sustainv1alpha1.WorkloadRecommendation
	if err := c.Get(ctx, key, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		logger.V(1).Info("failed to read WorkloadRecommendation", "err", err)
		return fmt.Errorf("reading WorkloadRecommendation %s: %w", key, err)
	}
	if len(existing.Status.Containers) > 0 {
		return nil
	}
	if existing.Status.Source == sustainv1alpha1.RecommendationSourceNoData {
		return nil // already recorded; avoid a write every cycle
	}
	patched := existing.DeepCopy()
	patched.Status.Source = sustainv1alpha1.RecommendationSourceNoData
	patched.Status.ObservedAt = now
	if err := c.Status().Patch(ctx, patched, client.MergeFrom(&existing)); err != nil {
		logger.V(1).Info("failed to patch WorkloadRecommendation status", "err", err)
		return fmt.Errorf("patching WorkloadRecommendation %s status: %w", key, err)
	}
	return nil
}

// observedEquivalent suppresses a write every cycle for an unchanged workload.
func observedEquivalent(a, b map[string]sustainv1alpha1.ObservedContainerResources) bool {
	if b == nil {
		return true // caller had nothing to snapshot; leave what is there
	}
	if len(a) != len(b) {
		return false
	}
	for name, av := range a {
		bv, ok := b[name]
		if !ok || av.Init != bv.Init {
			return false
		}
		if !quantityEqual(av.CPURequest, bv.CPURequest) ||
			!quantityEqual(av.MemoryRequest, bv.MemoryRequest) ||
			!quantityEqual(av.CPULimit, bv.CPULimit) ||
			!quantityEqual(av.MemoryLimit, bv.MemoryLimit) {
			return false
		}
	}
	return true
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

func quantityFrom(rl corev1.ResourceList, name corev1.ResourceName) *resource.Quantity {
	q, ok := rl[name]
	if !ok {
		return nil
	}
	return &q
}

func buildStatus(
	recs map[string]workload.ContainerRecommendation,
	observed map[string]sustainv1alpha1.ObservedContainerResources,
	now metav1.Time,
) sustainv1alpha1.WorkloadRecommendationStatus {
	out := sustainv1alpha1.WorkloadRecommendationStatus{
		ObservedAt:        now,
		Source:            sustainv1alpha1.RecommendationSourcePrometheus,
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

// statusEquivalent compares two statuses ignoring ObservedAt, so write
// amplification scales with change rather than workload count.
func statusEquivalent(a, b sustainv1alpha1.WorkloadRecommendationStatus) bool {
	if a.Source != b.Source {
		return false
	}
	// A departed identity coming back must write even with unchanged values: the
	// write is what clears Departed, and leaving it set keeps the webhook
	// waiving the freshness gate for a workload that is running again. The
	// caller's RefreshInterval condition happens to force this today, but that
	// is a coincidence of two independently-tuned constants.
	if a.Departed != b.Departed {
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
