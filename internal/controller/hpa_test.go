package controller

import (
	"context"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// int32p returns a pointer to the given int32 value.
func int32p(v int32) *int32 { return &v }

func hpaScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := autoscalingv2.AddToScheme(s); err != nil {
		t.Fatalf("add autoscalingv2 scheme: %v", err)
	}
	return s
}

func makeHpa(namespace, name, targetKind, targetName string, metrics []autoscalingv2.MetricSpec) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: targetKind,
				Name: targetName,
			},
			Metrics: metrics,
		},
	}
}

func cpuUtilizationMetric(utilization int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceCPU,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32p(utilization),
			},
		},
	}
}

func memoryUtilizationMetric(utilization int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceMemory,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: int32p(utilization),
			},
		},
	}
}

// TestDetectHpa_MatchesByScaleTargetRef verifies that detectHpa finds an HPA whose
// scaleTargetRef matches the given kind/name and returns the CPU utilization.
func TestDetectHpa_MatchesByScaleTargetRef(t *testing.T) {
	hpa := makeHpa("default", "web-hpa", "Deployment", "web", []autoscalingv2.MetricSpec{
		cpuUtilizationMetric(80),
	})

	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		WithObjects(hpa).
		Build()

	det, err := detectHpa(context.Background(), c, "default", "Deployment", "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if det == nil {
		t.Fatal("expected detection result, got nil")
	}
	if det.cpuUtilization == nil {
		t.Fatal("expected cpuUtilization to be set")
	}
	if *det.cpuUtilization != 80 {
		t.Errorf("expected cpuUtilization=80, got %d", *det.cpuUtilization)
	}
	if det.memoryUtilization != nil {
		t.Errorf("expected memoryUtilization to be nil, got %d", *det.memoryUtilization)
	}
}

// TestDetectHpa_NoHpaFound verifies that detectHpa returns nil when no HPA exists.
func TestDetectHpa_NoHpaFound(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		Build()

	det, err := detectHpa(context.Background(), c, "default", "Deployment", "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if det != nil {
		t.Errorf("expected nil result, got %+v", det)
	}
}

// TestDetectHpa_OverrideTakesPrecedence verifies that a Policy-level override replaces
// the auto-detected HPA utilization value.
func TestDetectHpa_OverrideTakesPrecedence(t *testing.T) {
	hpa := makeHpa("default", "web-hpa", "Deployment", "web", []autoscalingv2.MetricSpec{
		cpuUtilizationMetric(80),
	})

	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		WithObjects(hpa).
		Build()

	override := int32p(75)
	hpaConfig := &sustainv1alpha1.HpaConfig{
		CPU: &sustainv1alpha1.HpaResourceConfig{
			TargetUtilizationOverride: override,
		},
	}

	det, err := detectHpa(context.Background(), c, "default", "Deployment", "web", hpaConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if det == nil {
		t.Fatal("expected detection result, got nil")
	}
	if det.cpuUtilization == nil {
		t.Fatal("expected cpuUtilization to be set")
	}
	if *det.cpuUtilization != 75 {
		t.Errorf("expected cpuUtilization=75 (override), got %d", *det.cpuUtilization)
	}
}

// TestDetectHpa_BothCpuAndMemory verifies that both CPU and memory utilization metrics
// are extracted when the HPA defines both.
func TestDetectHpa_BothCpuAndMemory(t *testing.T) {
	hpa := makeHpa("default", "web-hpa", "Deployment", "web", []autoscalingv2.MetricSpec{
		cpuUtilizationMetric(80),
		memoryUtilizationMetric(70),
	})

	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		WithObjects(hpa).
		Build()

	det, err := detectHpa(context.Background(), c, "default", "Deployment", "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if det == nil {
		t.Fatal("expected detection result, got nil")
	}
	if det.cpuUtilization == nil || *det.cpuUtilization != 80 {
		t.Errorf("expected cpuUtilization=80, got %v", det.cpuUtilization)
	}
	if det.memoryUtilization == nil || *det.memoryUtilization != 70 {
		t.Errorf("expected memoryUtilization=70, got %v", det.memoryUtilization)
	}
}

// TestDetectHpa_CustomMetricsOnly verifies that an HPA with only Pods-type (custom)
// metrics returns nil because no resource utilization metrics are present.
func TestDetectHpa_CustomMetricsOnly(t *testing.T) {
	hpa := makeHpa("default", "web-hpa", "Deployment", "web", []autoscalingv2.MetricSpec{
		{
			Type: autoscalingv2.PodsMetricSourceType,
			Pods: &autoscalingv2.PodsMetricSource{
				Metric: autoscalingv2.MetricIdentifier{Name: "requests_per_second"},
				Target: autoscalingv2.MetricTarget{
					Type: autoscalingv2.AverageValueMetricType,
				},
			},
		},
	})

	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		WithObjects(hpa).
		Build()

	det, err := detectHpa(context.Background(), c, "default", "Deployment", "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if det != nil {
		t.Errorf("expected nil result for custom-only metrics, got %+v", det)
	}
}

func TestResolveHpaMode_DefaultsToHpaAware(t *testing.T) {
	policy := &sustainv1alpha1.Policy{}
	if mode := resolveHpaMode(policy); mode != sustainv1alpha1.HpaModeHpaAware {
		t.Errorf("expected HpaAware, got %s", mode)
	}
}

func TestResolveHpaMode_ReadsFromConfig(t *testing.T) {
	policy := &sustainv1alpha1.Policy{}
	policy.Spec.RightSizing.UpdatePolicy.Hpa = &sustainv1alpha1.HpaConfig{
		Mode: sustainv1alpha1.HpaModeIgnore,
	}
	if mode := resolveHpaMode(policy); mode != sustainv1alpha1.HpaModeIgnore {
		t.Errorf("expected Ignore, got %s", mode)
	}
}

// TestDetectHpa_AverageValueNotUtilization verifies that an HPA using AverageValue
// (not Utilization) metric type returns nil because we cannot apply utilization math.
func TestDetectHpa_AverageValueNotUtilization(t *testing.T) {
	averageValue := autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceCPU,
			Target: autoscalingv2.MetricTarget{
				Type: autoscalingv2.AverageValueMetricType,
				// AverageUtilization intentionally nil
			},
		},
	}

	hpa := makeHpa("default", "web-hpa", "Deployment", "web", []autoscalingv2.MetricSpec{
		averageValue,
	})

	c := fake.NewClientBuilder().
		WithScheme(hpaScheme(t)).
		WithObjects(hpa).
		Build()

	det, err := detectHpa(context.Background(), c, "default", "Deployment", "web", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if det != nil {
		t.Errorf("expected nil result for AverageValue metric type, got %+v", det)
	}
}
