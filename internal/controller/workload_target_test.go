package controller

import (
	"slices"
	"testing"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// A bare-pod group's pods are its members as the inventory found them: a pod
// sharing the owner-name but owned by a controller, opted into another Policy,
// or grouped under another owner-name must never be applied to.
func TestTargetsOf_BarePodGroupHoldsOnlyItsGovernedPods(t *testing.T) {
	member := barePod("airflow", "etl-run-1", "etl-daily")
	sibling := barePod("airflow", "etl-run-2", "etl-daily")
	controlled := barePod("airflow", "etl-run-3", "etl-daily")
	controlled.OwnerReferences = controllerRef("ReplicaSet", "rs")
	theirs := barePod("airflow", "etl-run-4", "etl-daily")
	theirs.Annotations[sustainv1alpha1.PolicyAnnotation] = "other-policy"
	other := barePod("airflow", "other-run-1", "other-task")

	target := barePodTarget(t, "airflow", "etl-daily", member, sibling, controlled, theirs, other)

	var got []string
	for _, pod := range target.BarePodMembers {
		got = append(got, pod.Name)
	}
	if want := []string{"etl-run-1", "etl-run-2"}; !slices.Equal(got, want) {
		t.Errorf("bare-pod members = %v, want %v", got, want)
	}
}
