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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := autoscalingv2.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return s
}

func newScaledObject(ns, name, targetKind, targetName string, minR, maxR int32, triggers ...any) *unstructured.Unstructured {
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
	if len(triggers) > 0 {
		u.Object["spec"].(map[string]any)["triggers"] = triggers
	}
	return u
}

func trigger(typ, key, value string) map[string]any {
	return map[string]any{"type": typ, "metadata": map[string]any{key: value}}
}

func resourceMetric(name corev1.ResourceName, pct int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name:   name,
			Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To(pct)},
		},
	}
}

func newHPA(ns, name, targetKind, targetName string, minR, maxR int32, metrics ...autoscalingv2.MetricSpec) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: targetKind, Name: targetName},
			MinReplicas:    &minR,
			MaxReplicas:    maxR,
			Metrics:        metrics,
		},
	}
}

// What ForIdentity reads from each autoscaler, which one shapes an identity's
// Recommendation, and which members disagree on it.
func TestForIdentity(t *testing.T) {
	withReplicas := func(h *autoscalingv2.HorizontalPodAutoscaler, n int32) *autoscalingv2.HorizontalPodAutoscaler {
		h.Status.CurrentReplicas = n
		return h
	}
	withStatus := func(so *unstructured.Unstructured, n int64) *unstructured.Unstructured {
		so.Object["status"] = map[string]any{"currentReplicas": n}
		return so
	}
	hpaWeb := Info{
		Kind: KindHPA, Namespace: "default", Name: "web-hpa", MinReplicas: 2, MaxReplicas: 10,
		ConfiguredTargets: map[string]int32{ResourceCPU: 70},
	}

	cases := []struct {
		name    string
		objs    []client.Object
		members []string
		want    Group
	}{
		{
			name:    "no autoscaler",
			members: []string{"web"},
			want:    Group{Info: Info{Kind: KindNone}, Members: map[string]Info{"web": {Kind: KindNone}}},
		},
		{
			name:    "HPA with its replica bounds and CPU target",
			objs:    []client.Object{newHPA("default", "web-hpa", "Deployment", "web", 2, 10, resourceMetric(corev1.ResourceCPU, 70))},
			members: []string{"web"},
			want:    Group{Info: hpaWeb, Members: map[string]Info{"web": hpaWeb}},
		},
		{
			name: "HPA CPU and memory targets and current replicas",
			objs: []client.Object{withReplicas(newHPA("default", "h", "Deployment", "web", 1, 5,
				resourceMetric(corev1.ResourceCPU, 70), resourceMetric(corev1.ResourceMemory, 80)), 7)},
			members: []string{"web"},
			want: group("web", Info{
				Kind: KindHPA, Namespace: "default", Name: "h", MinReplicas: 1, MaxReplicas: 5, CurrentReplicas: 7,
				ConfiguredTargets: map[string]int32{ResourceCPU: 70, ResourceMemory: 80},
			}),
		},
		{
			name: "HPA on an external metric has an unknown trigger and no target",
			objs: []client.Object{newHPA("default", "h", "Deployment", "web", 1, 5, autoscalingv2.MetricSpec{
				Type:     autoscalingv2.ExternalMetricSourceType,
				External: &autoscalingv2.ExternalMetricSource{Metric: autoscalingv2.MetricIdentifier{Name: "queue_depth"}},
			})},
			members: []string{"web"},
			want:    group("web", Info{Kind: KindHPA, Namespace: "default", Name: "h", MinReplicas: 1, MaxReplicas: 5, HasUnknownTrigger: true}),
		},
		{
			name: "ScaledObject CPU and memory triggers and current replicas",
			objs: []client.Object{withStatus(newScaledObject("default", "so", "Deployment", "web", 1, 8,
				trigger("cpu", "value", "60"), trigger("memory", "value", "75")), 7)},
			members: []string{"web"},
			want: group("web", Info{
				Kind: KindKEDA, Namespace: "default", Name: "so", MinReplicas: 1, MaxReplicas: 8, CurrentReplicas: 7,
				ConfiguredTargets: map[string]int32{ResourceCPU: 60, ResourceMemory: 75},
			}),
		},
		{
			name:    "ScaledObject on Kafka lag has an unknown trigger and no target",
			objs:    []client.Object{newScaledObject("default", "so", "Deployment", "web", 0, 5, trigger("kafka", "lagThreshold", "10"))},
			members: []string{"web"},
			want:    group("web", Info{Kind: KindKEDA, Namespace: "default", Name: "so", MaxReplicas: 5, HasUnknownTrigger: true}),
		},
		{
			name: "a ScaledObject wins over the HPA KEDA generates for it",
			objs: []client.Object{
				newHPA("default", "keda-hpa-web", "Deployment", "web", 2, 10),
				newScaledObject("default", "web-so", "Deployment", "web", 1, 8),
			},
			members: []string{"web"},
			want:    group("web", Info{Kind: KindKEDA, Namespace: "default", Name: "web-so", MinReplicas: 1, MaxReplicas: 8}),
		},
		{
			name:    "an HPA targeting another name or kind targets no member",
			objs:    []client.Object{newHPA("default", "h", "StatefulSet", "web", 1, 5), newHPA("default", "g", "Deployment", "other", 1, 5)},
			members: []string{"web"},
			want:    group("web", Info{Kind: KindNone}),
		},
		{
			name:    "the first member by name with an autoscaler shapes the group, whatever the listing order",
			objs:    []client.Object{newHPA("default", "web-hpa", "Deployment", "api-green", 2, 10, resourceMetric(corev1.ResourceCPU, 70))},
			members: []string{"api-green", "api-blue"},
			want: Group{
				Info:        Info{Kind: KindHPA, Namespace: "default", Name: "web-hpa", MinReplicas: 2, MaxReplicas: 10, ConfiguredTargets: map[string]int32{ResourceCPU: 70}},
				Members:     map[string]Info{"api-blue": {Kind: KindNone}, "api-green": {Kind: KindHPA, Namespace: "default", Name: "web-hpa", MinReplicas: 2, MaxReplicas: 10, ConfiguredTargets: map[string]int32{ResourceCPU: 70}}},
				Disagreeing: []string{"api-green=HPA"},
			},
		},
		{
			name: "members with different autoscalers disagree",
			objs: []client.Object{
				newHPA("default", "a-hpa", "Deployment", "api-a", 1, 4),
				newScaledObject("default", "b-so", "Deployment", "api-b", 1, 6),
			},
			members: []string{"api-b", "api-a"},
			want: Group{
				Info: Info{Kind: KindHPA, Namespace: "default", Name: "a-hpa", MinReplicas: 1, MaxReplicas: 4},
				Members: map[string]Info{
					"api-a": {Kind: KindHPA, Namespace: "default", Name: "a-hpa", MinReplicas: 1, MaxReplicas: 4},
					"api-b": {Kind: KindKEDA, Namespace: "default", Name: "b-so", MinReplicas: 1, MaxReplicas: 6},
				},
				Disagreeing: []string{"api-b=KEDA"},
			},
		},
		{
			name: "an identity without members has none",
			objs: []client.Object{newHPA("default", "web-hpa", "Deployment", "web", 2, 10)},
			want: Group{Info: Info{Kind: KindNone}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(tc.objs...).Build()

			got, err := NewNamespacedSnapshot(c).ForIdentity(context.Background(), "default", "Deployment", tc.members)
			if err != nil {
				t.Fatalf("ForIdentity: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ForIdentity =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

func group(member string, info Info) Group {
	return Group{Info: info, Members: map[string]Info{member: info}}
}

// A cluster without the KEDA CRD has no ScaledObjects: its HPAs still count.
func TestForIdentity_ScaledObjectCRDMissing(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(newHPA("default", "web-hpa", "Deployment", "web", 1, 5)).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind() == scaledObjectListGVK {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "keda.sh", Kind: "ScaledObject"}}
			}
			return cl.List(ctx, list, opts...)
		},
	})

	got, err := NewNamespacedSnapshot(c).ForIdentity(context.Background(), "default", "Deployment", []string{"web"})
	if err != nil {
		t.Fatalf("a missing KEDA CRD must not fail the read: %v", err)
	}
	if got.Info.Kind != KindHPA {
		t.Errorf("Info = %+v, want the HPA", got.Info)
	}
}

// failingFirstList fails the first List of the whole test, and counts every
// List.
func failingFirstList(t *testing.T, lists *atomic.Int32, objs ...client.Object) client.Client {
	t.Helper()
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	var failed atomic.Bool
	return interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			if failed.CompareAndSwap(false, true) {
				return errors.New("transient list blip")
			}
			return cl.List(ctx, list, opts...)
		},
	})
}

// A transient List failure is not cached: the next read of the namespace
// retries it.
func TestNamespacedSnapshot_FailThenRecover(t *testing.T) {
	var lists atomic.Int32
	m := NewNamespacedSnapshot(failingFirstList(t, &lists, newHPA("default", "web-hpa", "Deployment", "web", 2, 10)))

	got, err := m.ForIdentity(context.Background(), "default", "Deployment", []string{"web"})
	if err == nil {
		t.Fatal("expected the first read to return the transient error")
	}
	if got.Info.Kind != KindNone {
		t.Errorf("Info on a failed read = %+v, want KindNone", got.Info)
	}

	got, err = m.ForIdentity(context.Background(), "default", "Deployment", []string{"web"})
	if err != nil {
		t.Fatalf("expected the second read to recover, got %v", err)
	}
	if got.Info.Kind != KindHPA || got.Info.Name != "web-hpa" {
		t.Errorf("expected the recovered HPA, got %+v", got.Info)
	}
}

// A namespace is listed once, however many identities of it are read.
func TestNamespacedSnapshot_ListsANamespaceOnce(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(newHPA("default", "web-hpa", "Deployment", "web", 2, 10)).Build()
	var lists atomic.Int32
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return cl.List(ctx, list, opts...)
		},
	})
	m := NewNamespacedSnapshot(c)

	for _, name := range []string{"web", "api", "web"} {
		if _, err := m.ForIdentity(context.Background(), "default", "Deployment", []string{name}); err != nil {
			t.Fatalf("ForIdentity(%s): %v", name, err)
		}
	}

	if n := lists.Load(); n != 2 {
		t.Errorf("List calls = %d, want 2 (HPAs and ScaledObjects, once)", n)
	}
}

// Concurrent first reads of one namespace while the first List fails: no data
// race, and a read afterwards succeeds because the failed build was never
// cached.
func TestNamespacedSnapshot_ConcurrentFailThenRecover(t *testing.T) {
	var lists atomic.Int32
	m := NewNamespacedSnapshot(failingFirstList(t, &lists, newHPA("default", "web-hpa", "Deployment", "web", 2, 10)))

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			// Ignored on purpose: some reads hit the blip and some do not;
			// only race-freedom is asserted here.
			_, _ = m.ForIdentity(context.Background(), "default", "Deployment", []string{"web"})
		}()
	}
	wg.Wait()

	got, err := m.ForIdentity(context.Background(), "default", "Deployment", []string{"web"})
	if err != nil {
		t.Fatalf("expected the read after the storm to succeed, got %v", err)
	}
	if got.Info.Kind != KindHPA || got.Info.Name != "web-hpa" {
		t.Errorf("expected the recovered HPA, got %+v", got.Info)
	}
}
