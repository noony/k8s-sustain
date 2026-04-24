package controller

import (
	"context"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPatchHpaToAverageValue_CPU(t *testing.T) {
	hpa := makeHpa("prod", "web-hpa", "Deployment", "web", []autoscalingv2.MetricSpec{
		cpuUtilizationMetric(80),
	})

	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		WithObjects(hpa).
		Build()

	currentRequests := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceCPU: resource.MustParse("1000m"),
	}

	err := patchHpaToAverageValue(context.Background(), c, hpa, currentRequests)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the HPA was updated.
	updated := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), updated); err != nil {
		t.Fatalf("failed to get updated HPA: %v", err)
	}

	for _, m := range updated.Spec.Metrics {
		if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
			continue
		}
		if m.Resource.Name == corev1.ResourceCPU {
			if m.Resource.Target.Type != autoscalingv2.AverageValueMetricType {
				t.Errorf("expected AverageValue type, got %s", m.Resource.Target.Type)
			}
			want := resource.MustParse("800m") // 1000m * 80/100
			if m.Resource.Target.AverageValue.Cmp(want) != 0 {
				t.Errorf("expected 800m, got %s", m.Resource.Target.AverageValue)
			}
			if m.Resource.Target.AverageUtilization != nil {
				t.Error("expected nil AverageUtilization")
			}
		}
	}
}

func TestPatchHpaToAverageValue_NoUtilizationMetrics(t *testing.T) {
	hpa := makeHpa("prod", "web-hpa", "Deployment", "web", []autoscalingv2.MetricSpec{
		{
			Type: autoscalingv2.PodsMetricSourceType,
			Pods: &autoscalingv2.PodsMetricSource{
				Metric: autoscalingv2.MetricIdentifier{Name: "rps"},
				Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType},
			},
		},
	})

	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		WithObjects(hpa).
		Build()

	err := patchHpaToAverageValue(context.Background(), c, hpa, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be a no-op — no utilization metrics to convert.
}
