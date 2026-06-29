package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/noony/k8s-sustain/internal/workload"
)

// changedContainers returns the names of containers whose recommended CPU/memory
// requests or limits differ from the current spec.
//
// It defers to workload.ContainerMatches so the controller's "stale, recycle"
// decision uses exactly the same predicate the patcher uses when deciding
// whether an apply is a no-op. Keeping a single source of truth prevents the
// two sides from disagreeing (one recycling while the other skips, causing
// churn). See workload.ContainerMatches for the zero/unset semantics.
func changedContainers(current []corev1.Container, recs map[string]workload.ContainerRecommendation, tol workload.Tolerance) []string {
	recs = workload.ClampRecsToTolerance(current, recs, tol)
	byName := make(map[string]corev1.Container, len(current))
	for _, c := range current {
		byName[c.Name] = c
	}
	var changed []string
	for name, rec := range recs {
		c, ok := byName[name]
		if !ok {
			changed = append(changed, name)
			continue
		}
		if !workload.ContainerMatches(c.Resources, rec) {
			changed = append(changed, name)
		}
	}
	return changed
}

// quantityEqual reports whether two stored recommendation quantities are
// equal, treating a nil pointer and an explicit zero as the same "unset" value.
// Used to compare two WorkloadRecommendation statuses (see statusEquivalent);
// for comparing a live container against a recommendation use
// workload.ContainerMatches instead.
func quantityEqual(a *resource.Quantity, b *resource.Quantity) bool {
	aZero := a == nil || a.IsZero()
	bZero := b == nil || b.IsZero()
	if aZero || bZero {
		return aZero == bZero
	}
	return a.Cmp(*b) == 0
}
