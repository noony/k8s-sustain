package workload

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestGroupBarePods_GroupsByNamespaceAndOwnerName(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	podA1 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-1", CreationTimestamp: metav1.NewTime(older),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "old-container"}}},
	}
	podA2 := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow", Name: "etl-run-2", CreationTimestamp: metav1.NewTime(newer),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "new-container"}}},
	}
	// Same owner-name, different namespace — must NOT collapse with podA1/podA2.
	podB := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "airflow-staging", Name: "etl-run-1", CreationTimestamp: metav1.NewTime(older),
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	groups := GroupBarePods([]corev1.Pod{podA1, podA2, podB})
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (one per namespace), got %d: %+v", len(groups), groups)
	}
	for _, g := range groups {
		if g.Name != "etl-daily" {
			t.Errorf("Name = %s, want etl-daily", g.Name)
		}
		if g.Namespace == "airflow" {
			if len(g.Containers) != 1 || g.Containers[0].Name != "new-container" {
				t.Errorf("expected the more recently created pod's container, got %+v", g.Containers)
			}
		}
	}
}

func TestGroupBarePods_NoOwnerNameAnnotation_NotDiscovered(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "standalone",
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	groups := GroupBarePods([]corev1.Pod{pod})
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for a pod with no owner-name annotation, got %d", len(groups))
	}
}

func TestGroupBarePods_OwnedPod_NotDiscovered(t *testing.T) {
	ctrlBool := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "owned",
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "etl-daily-job", Controller: &ctrlBool}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	groups := GroupBarePods([]corev1.Pod{pod})
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for an owned pod (handled by another kind's listing instead), got %d", len(groups))
	}
}

func TestGroupBarePods_NoPolicyAnnotation_NotDiscovered(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "unmanaged",
			Annotations: map[string]string{sustainv1alpha1.OwnerNameAnnotation: "etl-daily"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	groups := GroupBarePods([]corev1.Pod{pod})
	if len(groups) != 0 {
		t.Errorf("expected 0 groups for a pod with no policy annotation, got %d", len(groups))
	}
}
