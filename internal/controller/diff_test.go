package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/noony/k8s-sustain/internal/workload"
)

func TestChangedContainers_DetectsRequestAndLimitDrift(t *testing.T) {
	containers := []corev1.Container{
		{
			Name: "matches",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
			},
		},
		{
			Name: "drift-cpu",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
		},
		{
			Name: "no-rec",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
			},
		},
	}
	recs := map[string]workload.ContainerRecommendation{
		"matches":   {CPURequest: qty("100m"), MemoryRequest: qty("64Mi")},
		"drift-cpu": {CPURequest: qty("250m")},
		// no-rec intentionally absent
	}

	got := changedContainers(containers, recs, workload.Tolerance{})
	if len(got) != 1 || got[0] != "drift-cpu" {
		t.Errorf("expected ['drift-cpu'], got %v", got)
	}
}
