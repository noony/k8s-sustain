package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestRecommendableContainers(t *testing.T) {
	t.Run("no init containers returns regular slice unchanged", func(t *testing.T) {
		w := &workloadTarget{
			Containers: []corev1.Container{{Name: "app"}},
		}
		got, initNames := w.recommendableContainers(false)
		if len(got) != 1 || got[0].Name != "app" {
			t.Fatalf("unexpected containers: %+v", got)
		}
		if initNames != nil {
			t.Fatalf("expected nil initNames, got %v", initNames)
		}
	})

	t.Run("merges regular + init when not excluded", func(t *testing.T) {
		w := &workloadTarget{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "migrate"}},
		}
		got, initNames := w.recommendableContainers(false)
		if len(got) != 2 {
			t.Fatalf("expected 2 containers, got %d", len(got))
		}
		if _, ok := initNames["migrate"]; !ok {
			t.Errorf("expected migrate in init names, got %v", initNames)
		}
		if _, ok := initNames["app"]; ok {
			t.Errorf("regular container should not be in init names, got %v", initNames)
		}
	})

	t.Run("excludes init when ExcludeInitContainers=true", func(t *testing.T) {
		w := &workloadTarget{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "migrate"}},
		}
		got, initNames := w.recommendableContainers(true)
		if len(got) != 1 || got[0].Name != "app" {
			t.Fatalf("expected only regular container, got %+v", got)
		}
		if initNames != nil {
			t.Errorf("expected nil initNames when excluded, got %v", initNames)
		}
	})
}

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
