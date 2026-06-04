package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/noony/k8s-sustain/internal/workload"
)

func TestQuantityEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b *resource.Quantity
		want bool
	}{
		{"both nil", nil, nil, true},
		{"both zero", qty("0"), qty("0"), true},
		{"nil and zero", nil, qty("0"), true},
		{"equal nonzero", qty("100m"), qty("100m"), true},
		{"different", qty("100m"), qty("200m"), false},
		{"nil vs nonzero", nil, qty("100m"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := quantityEqual(c.a, c.b); got != c.want {
				t.Errorf("quantityEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestChangedContainers_DetectsRequestAndLimitDrift verifies that
// changedContainers flags every container whose request or limit drifts from
// the recommendation, ignoring containers without a recommendation entry.
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

	got := changedContainers(containers, recs)
	if len(got) != 1 || got[0] != "drift-cpu" {
		t.Errorf("expected ['drift-cpu'], got %v", got)
	}
}
