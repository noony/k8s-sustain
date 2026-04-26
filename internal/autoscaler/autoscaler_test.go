package autoscaler

import (
	"context"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	_ "k8s.io/api/autoscaling/v2"
)

func newScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := autoscalingv2.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func newScaledObject(ns, name, targetKind, targetName string, minR, maxR int32) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"})
	u.SetNamespace(ns)
	u.SetName(name)
	u.Object["spec"] = map[string]interface{}{
		"scaleTargetRef": map[string]interface{}{
			"kind": targetKind,
			"name": targetName,
		},
		"minReplicaCount": int64(minR),
		"maxReplicaCount": int64(maxR),
	}
	return u
}

func newHPA(ns, name, targetKind, targetName string, minR, maxR int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: targetKind, Name: targetName},
			MinReplicas:    &minR,
			MaxReplicas:    maxR,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptrInt32(70),
					},
				},
			}},
		},
	}
}

func ptrInt32(v int32) *int32 { return &v }

func newHPAWithTargets(ns, name, targetKind, targetName string, minR, maxR int32, cpuPct, memPct *int32) *autoscalingv2.HorizontalPodAutoscaler {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: targetKind, Name: targetName},
			MinReplicas:    &minR,
			MaxReplicas:    maxR,
		},
	}
	if cpuPct != nil {
		hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: cpuPct,
				},
			},
		})
	}
	if memPct != nil {
		hpa.Spec.Metrics = append(hpa.Spec.Metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: corev1.ResourceMemory,
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: memPct,
				},
			},
		})
	}
	return hpa
}

func TestDetect_HPA_ExtractsCPUAndMemoryTargets(t *testing.T) {
	hpa := newHPAWithTargets("default", "web-hpa", "Deployment", "web", 1, 5, ptrInt32(70), ptrInt32(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()

	got, err := Detect(context.Background(), c, "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.ConfiguredTargets[ResourceCPU] != 70 {
		t.Errorf("expected cpu target 70, got %d", got.ConfiguredTargets[ResourceCPU])
	}
	if got.ConfiguredTargets[ResourceMemory] != 80 {
		t.Errorf("expected memory target 80, got %d", got.ConfiguredTargets[ResourceMemory])
	}
	if got.HasUnknownTrigger {
		t.Errorf("expected HasUnknownTrigger=false for cpu+memory only")
	}
}

func TestDetect_HPA_FlagsUnknownTrigger(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ext-hpa"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "ext"},
			MinReplicas:    ptrInt32(1),
			MaxReplicas:    5,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ExternalMetricSourceType,
				External: &autoscalingv2.ExternalMetricSource{
					Metric: autoscalingv2.MetricIdentifier{Name: "queue_depth"},
				},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()
	got, err := Detect(context.Background(), c, "default", "Deployment", "ext")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !got.HasUnknownTrigger {
		t.Errorf("expected HasUnknownTrigger=true for external metric")
	}
	if len(got.ConfiguredTargets) != 0 {
		t.Errorf("expected no ConfiguredTargets, got %v", got.ConfiguredTargets)
	}
}

func newScaledObjectWithTriggers(ns, name, targetKind, targetName string, minR, maxR int32, triggers []interface{}) *unstructured.Unstructured {
	u := newScaledObject(ns, name, targetKind, targetName, minR, maxR)
	spec := u.Object["spec"].(map[string]interface{})
	spec["triggers"] = triggers
	return u
}

func TestDetect_ScaledObject_ExtractsCPUAndMemoryTriggers(t *testing.T) {
	so := newScaledObjectWithTriggers("default", "web-so", "Deployment", "web", 1, 8, []interface{}{
		map[string]interface{}{"type": "cpu", "metadata": map[string]interface{}{"value": "60"}},
		map[string]interface{}{"type": "memory", "metadata": map[string]interface{}{"value": "75"}},
	})
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(so).Build()
	got, err := Detect(context.Background(), c, "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.ConfiguredTargets[ResourceCPU] != 60 {
		t.Errorf("expected cpu 60, got %d", got.ConfiguredTargets[ResourceCPU])
	}
	if got.ConfiguredTargets[ResourceMemory] != 75 {
		t.Errorf("expected memory 75, got %d", got.ConfiguredTargets[ResourceMemory])
	}
	if got.HasUnknownTrigger {
		t.Errorf("expected HasUnknownTrigger=false")
	}
}

func TestDetect_ScaledObject_FlagsUnknownTriggerType(t *testing.T) {
	so := newScaledObjectWithTriggers("default", "q-so", "Deployment", "q", 0, 5, []interface{}{
		map[string]interface{}{"type": "kafka", "metadata": map[string]interface{}{"lagThreshold": "10"}},
	})
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(so).Build()
	got, err := Detect(context.Background(), c, "default", "Deployment", "q")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !got.HasUnknownTrigger {
		t.Errorf("expected HasUnknownTrigger=true for kafka trigger")
	}
	if len(got.ConfiguredTargets) != 0 {
		t.Errorf("expected no ConfiguredTargets, got %v", got.ConfiguredTargets)
	}
}

func TestDetect_None(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	got, err := Detect(context.Background(), c, "default", "Deployment", "missing")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindNone {
		t.Errorf("expected KindNone, got %v", got.Kind)
	}
}

func TestDetect_HPAOnly(t *testing.T) {
	hpa := newHPA("default", "web-hpa", "Deployment", "web", 2, 10)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()
	got, err := Detect(context.Background(), c, "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindHPA || got.Name != "web-hpa" || got.MinReplicas != 2 || got.MaxReplicas != 10 {
		t.Errorf("unexpected info: %+v", got)
	}
}

func TestDetect_ScaledObjectTakesPriority(t *testing.T) {
	hpa := newHPA("default", "keda-hpa-web", "Deployment", "web", 2, 10)
	so := newScaledObject("default", "web-so", "Deployment", "web", 1, 8)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa, so).Build()

	got, err := Detect(context.Background(), c, "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindKEDA || got.Name != "web-so" || got.MinReplicas != 1 || got.MaxReplicas != 8 {
		t.Errorf("expected KEDA preferred, got %+v", got)
	}
}

func TestDetect_HPACurrentReplicas(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "h"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "w"},
			MinReplicas:    ptrInt32(2),
			MaxReplicas:    10,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptrInt32(70),
					},
				},
			}},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 7},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()
	got, err := Detect(context.Background(), c, "ns", "Deployment", "w")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindHPA {
		t.Errorf("expected KindHPA, got %v", got.Kind)
	}
	if got.CurrentReplicas != 7 {
		t.Errorf("expected CurrentReplicas=7, got %d", got.CurrentReplicas)
	}
}

func TestDetect_KEDACurrentReplicas(t *testing.T) {
	so := newScaledObject("ns", "so", "Deployment", "w", 1, 10)
	so.Object["status"] = map[string]interface{}{
		"currentReplicas": int64(7),
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(so).Build()
	got, err := Detect(context.Background(), c, "ns", "Deployment", "w")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Kind != KindKEDA {
		t.Errorf("expected KindKEDA, got %v", got.Kind)
	}
	if got.CurrentReplicas != 7 {
		t.Errorf("expected CurrentReplicas=7, got %d", got.CurrentReplicas)
	}
}

func TestDetect_ScaledObjectCRDMissing(t *testing.T) {
	// Fake client without registering keda.sh — List returns NoKindMatchError-like.
	hpa := newHPA("default", "web-hpa", "Deployment", "web", 1, 5)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()

	got, err := Detect(context.Background(), c, "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("Detect should swallow missing-CRD error: %v", err)
	}
	if got.Kind != KindHPA {
		t.Errorf("expected fallback to HPA when ScaledObject CRD is missing, got %+v", got)
	}
	// Sanity: confirm the fake client really would have errored on ScaledObject list.
	var soList unstructured.UnstructuredList
	soList.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObjectList"})
	if err := c.List(context.Background(), &soList); err == nil {
		t.Skip("fake client does not error on unknown list; skipping CRD-missing assertion")
	} else if !apierrors.IsNotFound(err) && !runtime.IsNotRegisteredError(err) {
		t.Logf("ScaledObject list error (expected): %v", err)
	}
}
