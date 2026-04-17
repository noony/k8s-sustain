package webhook

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

func qtyp(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func TestModeForKind(t *testing.T) {
	ongoing := sustainv1alpha1.UpdateModeOngoing
	onCreate := sustainv1alpha1.UpdateModeOnCreate

	ut := sustainv1alpha1.UpdateTypes{
		Deployment:  &ongoing,
		StatefulSet: &onCreate,
		CronJob:     &ongoing,
	}

	tests := []struct {
		kind string
		want *sustainv1alpha1.UpdateMode
	}{
		{"Deployment", &ongoing},
		{"StatefulSet", &onCreate},
		{"CronJob", &ongoing},
		{"DaemonSet", nil},
		{"Unknown", nil},
	}

	for _, tt := range tests {
		got := modeForKind(ut, tt.kind)
		if tt.want == nil {
			if got != nil {
				t.Errorf("modeForKind(%q) = %v, want nil", tt.kind, *got)
			}
			continue
		}
		if got == nil || *got != *tt.want {
			t.Errorf("modeForKind(%q) = %v, want %v", tt.kind, got, *tt.want)
		}
	}
}

func TestBuildPatches_EmptyRecs(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}},
		},
	}
	recs := map[string]workload.ContainerRecommendation{}
	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil patches for empty recs")
	}
}

func TestBuildPatches_SetsResources(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {
			CPURequest:    qtyp("100m"),
			MemoryRequest: qtyp("64Mi"),
		},
	}

	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected patches, got nil")
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
	if patches[0].Op != "add" {
		t.Errorf("expected op 'add', got %q", patches[0].Op)
	}
	if patches[0].Path != "/spec/containers/0/resources" {
		t.Errorf("expected path '/spec/containers/0/resources', got %q", patches[0].Path)
	}
}

func TestBuildPatches_MultipleContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app":     {CPURequest: qtyp("100m")},
		"sidecar": {CPURequest: qtyp("50m")},
	}

	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches, got %d", len(patches))
	}
}

func TestBuildPatches_SkipsUnmatchedContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app"},
				{Name: "sidecar"},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: qtyp("100m")},
	}

	result, err := buildPatches(pod, recs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var patches []jsonPatch
	if err := json.Unmarshal(result, &patches); err != nil {
		t.Fatalf("unmarshal patches: %v", err)
	}
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(patches))
	}
}
