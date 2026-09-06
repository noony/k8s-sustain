package controller

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/noony/k8s-sustain/internal/workload"
)

// changedContainers returns the names of containers whose recommended CPU/memory
// requests or limits differ from the current spec. It defers to
// workload.ContainerMatches so the recycle decision and the patcher's own no-op
// check can never disagree and cause churn.
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
