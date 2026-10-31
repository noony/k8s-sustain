package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// A bare-pod group's pods are its members as the inventory found them: a pod
// sharing the owner-name but owned by a controller, opted into another Policy,
// or grouped under another owner-name must never be applied to. Apply narrows
// the Recommendation to the containers the group declares.
func TestTargetsOf_BarePodGroupHoldsOnlyItsGovernedPods(t *testing.T) {
	member := barePod("airflow", "etl-run-1", "etl-daily")
	sibling := barePod("airflow", "etl-run-2", "etl-daily")
	controlled := barePod("airflow", "etl-run-3", "etl-daily")
	controlled.OwnerReferences = controllerRef("ReplicaSet", "rs")
	theirs := barePod("airflow", "etl-run-4", "etl-daily")
	theirs.Annotations[sustainv1alpha1.PolicyAnnotation] = "other-policy"
	other := barePod("airflow", "other-run-1", "other-task")

	m := barePodTarget(t, "airflow", "etl-daily", member, sibling, controlled, theirs, other).member()

	if want := []string{"etl-run-1", "etl-run-2"}; !slices.Equal(names(m.Pods), want) {
		t.Errorf("bare-pod members = %v, want %v", names(m.Pods), want)
	}
	containers := make([]string, len(m.Containers))
	for i, c := range m.Containers {
		containers[i] = c.Name
	}
	if want := []string{"worker"}; !slices.Equal(containers, want) {
		t.Errorf("declared containers = %v, want %v", containers, want)
	}
}

func names(pods []*corev1.Pod) []string {
	out := make([]string, len(pods))
	for i, pod := range pods {
		out[i] = pod.Name
	}
	return out
}
