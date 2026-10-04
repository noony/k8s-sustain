package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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
