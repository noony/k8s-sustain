package autoscaler

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	u.Object["spec"] = map[string]any{
		"scaleTargetRef": map[string]any{
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
						AverageUtilization: ptr.To[int32](70),
					},
				},
			}},
		},
	}
}

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
	hpa := newHPAWithTargets("default", "web-hpa", "Deployment", "web", 1, 5, ptr.To[int32](70), ptr.To[int32](80))
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
			MinReplicas:    ptr.To[int32](1),
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

func newScaledObjectWithTriggers(ns, name, targetKind, targetName string, minR, maxR int32, triggers []any) *unstructured.Unstructured {
	u := newScaledObject(ns, name, targetKind, targetName, minR, maxR)
	spec := u.Object["spec"].(map[string]any)
	spec["triggers"] = triggers
	return u
}

func TestDetect_ScaledObject_ExtractsCPUAndMemoryTriggers(t *testing.T) {
	so := newScaledObjectWithTriggers("default", "web-so", "Deployment", "web", 1, 8, []any{
		map[string]any{"type": "cpu", "metadata": map[string]any{"value": "60"}},
		map[string]any{"type": "memory", "metadata": map[string]any{"value": "75"}},
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
	so := newScaledObjectWithTriggers("default", "q-so", "Deployment", "q", 0, 5, []any{
		map[string]any{"type": "kafka", "metadata": map[string]any{"lagThreshold": "10"}},
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
			MinReplicas:    ptr.To[int32](2),
			MaxReplicas:    10,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptr.To[int32](70),
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
	so.Object["status"] = map[string]any{
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

// Snapshot.Lookup must return byte-for-byte the same Info as Detect across
// every match case: HPA-only, ScaledObject-only, both (KEDA precedence),
// neither, and a scaleTargetRef mismatch.
func TestSnapshotLookup_MatchesDetect(t *testing.T) {
	hpaSet := []client.Object{newHPAWithTargets("default", "web-hpa", "Deployment", "web", 2, 10, ptr.To[int32](70), ptr.To[int32](80))}
	soSet := []client.Object{newScaledObjectWithTriggers("default", "api-so", "Deployment", "api", 1, 8, []any{
		map[string]any{"type": "cpu", "metadata": map[string]any{"value": "60"}},
	})}
	bothSet := []client.Object{
		newHPA("default", "keda-hpa-web", "Deployment", "web", 2, 10),
		newScaledObject("default", "web-so", "Deployment", "web", 1, 8),
	}

	cases := []struct {
		name   string
		objs   []client.Object
		kind   string
		wlName string
	}{
		{"hpa match", hpaSet, "Deployment", "web"},
		{"scaledobject match", soSet, "Deployment", "api"},
		{"both present (keda precedence)", bothSet, "Deployment", "web"},
		{"neither present", nil, "Deployment", "ghost"},
		{"scaleTargetRef mismatch (wrong name)", hpaSet, "Deployment", "other"},
		{"scaleTargetRef mismatch (wrong kind)", hpaSet, "StatefulSet", "web"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.objs...).Build()

			want, err := Detect(context.Background(), c, "default", tc.kind, tc.wlName)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}

			snap, err := BuildSnapshot(context.Background(), c, "default")
			if err != nil {
				t.Fatalf("BuildSnapshot: %v", err)
			}
			got := snap.Lookup(tc.kind, tc.wlName)

			if !reflect.DeepEqual(got, want) {
				t.Errorf("Snapshot.Lookup = %+v, want (Detect) %+v", got, want)
			}
		})
	}
}

func TestNamespacedSnapshotLookup_MatchesDetect(t *testing.T) {
	hpaA := newHPA("ns-a", "a-hpa", "Deployment", "a", 1, 5)
	soB := newScaledObject("ns-b", "b-so", "Deployment", "b", 2, 9)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpaA, soB).Build()

	m := NewNamespacedSnapshot(c)

	for _, tc := range []struct{ ns, kind, name string }{
		{"ns-a", "Deployment", "a"},
		{"ns-b", "Deployment", "b"},
		{"ns-a", "Deployment", "b"}, // present in ns-b only → None in ns-a
		{"ns-b", "Deployment", "missing"},
	} {
		want, err := Detect(context.Background(), c, tc.ns, tc.kind, tc.name)
		if err != nil {
			t.Fatalf("Detect(%s/%s): %v", tc.ns, tc.name, err)
		}
		got, err := m.Lookup(context.Background(), tc.ns, tc.kind, tc.name)
		if err != nil {
			t.Fatalf("NamespacedSnapshot.Lookup(%s/%s): %v", tc.ns, tc.name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s/%s: Lookup = %+v, want %+v", tc.ns, tc.name, got, want)
		}
	}
}

// A transient List failure on the first Lookup must not be cached. The old
// sync.Once design poisoned every subsequent Lookup in the namespace with it.
func TestNamespacedSnapshotLookup_FailThenRecover(t *testing.T) {
	hpa := newHPA("default", "web-hpa", "Deployment", "web", 2, 10)
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()

	var firstFailed atomic.Bool
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			// Fail only the very first List call of the whole test.
			if firstFailed.CompareAndSwap(false, true) {
				return errors.New("transient list blip")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	m := NewNamespacedSnapshot(c)

	if _, err := m.Lookup(context.Background(), "default", "Deployment", "web"); err == nil {
		t.Fatalf("expected first Lookup to return the transient error")
	}

	got, err := m.Lookup(context.Background(), "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("expected second Lookup to recover, got error: %v", err)
	}
	if got.Kind != KindHPA || got.Name != "web-hpa" || got.MinReplicas != 2 || got.MaxReplicas != 10 {
		t.Errorf("expected recovered HPA info, got %+v", got)
	}
}

func TestNamespacedSnapshotLookup_CachesSuccess(t *testing.T) {
	hpa := newHPA("default", "web-hpa", "Deployment", "web", 2, 10)
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()

	var lists atomic.Int32
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return cl.List(ctx, list, opts...)
		},
	})

	m := NewNamespacedSnapshot(c)

	for i := range 3 {
		if _, err := m.Lookup(context.Background(), "default", "Deployment", "web"); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}

	// BuildSnapshot issues two Lists; caching means three Lookups still cost two.
	if n := lists.Load(); n != 2 {
		t.Errorf("expected exactly 2 List calls (HPA+ScaledObject, once), got %d", n)
	}
}

// Concurrent same-namespace Lookups under -race while the first List fails:
// asserts no data race, and that a fresh Lookup afterwards succeeds because the
// failed build was never cached.
func TestNamespacedSnapshotLookup_ConcurrentFailThenRecover(t *testing.T) {
	hpa := newHPA("default", "web-hpa", "Deployment", "web", 2, 10)
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hpa).Build()

	var firstFailed atomic.Bool
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if firstFailed.CompareAndSwap(false, true) {
				return errors.New("transient list blip")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	m := NewNamespacedSnapshot(c)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			// Ignored on purpose: under concurrency some hit the blip and some do
			// not; only race-freedom is asserted here.
			_, _ = m.Lookup(context.Background(), "default", "Deployment", "web")
		}()
	}
	wg.Wait()

	// No goroutine may have poisoned the namespace with the cached error.
	got, err := m.Lookup(context.Background(), "default", "Deployment", "web")
	if err != nil {
		t.Fatalf("expected post-storm Lookup to succeed, got error: %v", err)
	}
	if got.Kind != KindHPA || got.Name != "web-hpa" {
		t.Errorf("expected recovered HPA info, got %+v", got)
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
	var soList unstructured.UnstructuredList
	soList.SetGroupVersionKind(schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObjectList"})
	if err := c.List(context.Background(), &soList); err == nil {
		t.Skip("fake client does not error on unknown list; skipping CRD-missing assertion")
	} else if !apierrors.IsNotFound(err) && !runtime.IsNotRegisteredError(err) {
		t.Logf("ScaledObject list error (expected): %v", err)
	}
}
