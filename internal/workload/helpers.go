package workload

import corev1 "k8s.io/api/core/v1"

// MergeContainersForRecommendation returns the slice of containers that
// should feed into the recommendation pipeline plus the set of names that
// originate from initContainers. When excludeInit is true (or the workload
// has no init containers), the init list is dropped and the returned set is
// empty.
//
// Both the controller's workloadTarget and the admission webhook need this
// same merge to drive Prometheus queries against every container they may
// patch.
func MergeContainersForRecommendation(containers, initContainers []corev1.Container, excludeInit bool) ([]corev1.Container, map[string]struct{}) {
	if excludeInit || len(initContainers) == 0 {
		return containers, nil
	}
	merged := make([]corev1.Container, 0, len(containers)+len(initContainers))
	merged = append(merged, containers...)
	merged = append(merged, initContainers...)
	initNames := make(map[string]struct{}, len(initContainers))
	for _, c := range initContainers {
		initNames[c.Name] = struct{}{}
	}
	return merged, initNames
}

// ApplyRecommendation applies a single recommendation to one container in
// place, returning whether anything changed. A zero/empty rec is a no-op.
// Callers that need a fresh ResourceRequirements (e.g. the admission webhook
// JSON-patch builder) should DeepCopy the container first.
func ApplyRecommendation(c *corev1.Container, rec ContainerRecommendation) bool {
	return applyRecToContainer(c, rec)
}
