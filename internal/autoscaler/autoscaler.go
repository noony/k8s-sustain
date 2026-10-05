// Package autoscaler provides read-only detection of Horizontal Pod Autoscalers
// and KEDA ScaledObjects targeting a workload. It never writes to either object.
//
// When both an HPA and a ScaledObject target the same workload, the ScaledObject
// is treated as canonical (KEDA owns and regenerates the HPA from it).
package autoscaler

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kind enumerates the autoscaler implementations we recognise.
type Kind string

const (
	KindNone Kind = "None"
	KindHPA  Kind = "HPA"
	KindKEDA Kind = "KEDA"
)

// Info is the detection result for a workload.
type Info struct {
	Kind            Kind
	Namespace       string
	Name            string
	MinReplicas     int32
	MaxReplicas     int32
	CurrentReplicas int32

	// ConfiguredTargets captures the autoscaler's averageUtilization (%) per
	// supported resource. Keys are "cpu" / "memory". Empty when the autoscaler
	// has no Resource/cpu|memory trigger configured.
	ConfiguredTargets map[string]int32

	// HasUnknownTrigger is true when the autoscaler has at least one trigger
	// we don't understand (External, Object, queue depth, custom prometheus,
	// etc.). The health classifier uses this to fall through to "Unknown"
	// when no CPU/memory target was extracted.
	HasUnknownTrigger bool
}

// Resource keys used inside ConfiguredTargets / advice metrics.
const (
	ResourceCPU    = "cpu"
	ResourceMemory = "memory"
)

var scaledObjectListGVK = schema.GroupVersionKind{
	Group:   "keda.sh",
	Version: "v1alpha1",
	Kind:    "ScaledObjectList",
}

// targetKey identifies a workload by its scaleTargetRef (kind, name) within a
// single namespace.
type targetKey struct {
	kind string
	name string
}

// snapshot is one namespace's autoscalers, indexed by scaleTargetRef.
type snapshot struct {
	// A nil map (e.g. when the KEDA CRD is absent) is treated as empty.
	hpa  map[targetKey]Info
	keda map[targetKey]Info
}

// Group is what targets each member of one identity. One Recommendation is
// injected into every member's pods, so one autoscaler shapes it.
type Group struct {
	// Info is the autoscaler the identity's Recommendation is shaped against:
	// that of the first member, in name order, that has one. KindNone when
	// none has.
	Info Info
	// Members holds each member's own autoscaler, by name.
	Members map[string]Info
	// Disagreeing names each member whose kind of autoscaler differs from the
	// first member's, as name=Kind.
	Disagreeing []string
}

// NamespacedSnapshot builds and caches one snapshot per namespace on first
// successful access, so a reconcile pass never lists a namespace it does not
// touch. Failed builds are deliberately NOT cached: a transient List blip fails
// only the read that hit it, and the next one retries. Safe for concurrent
// use; same-namespace builds serialize behind a per-entry mutex.
type NamespacedSnapshot struct {
	c client.Client

	mu   sync.Mutex
	byNS map[string]*nsEntry
}

type nsEntry struct {
	// mu serializes buildSnapshot so concurrent first reads don't double-build.
	mu   sync.Mutex
	snap *snapshot
}

// NewNamespacedSnapshot returns a lazy, per-namespace snapshot bound to the
// client. No I/O happens until the first read of a namespace.
func NewNamespacedSnapshot(c client.Client) *NamespacedSnapshot {
	return &NamespacedSnapshot{c: c, byNS: map[string]*nsEntry{}}
}

// ForIdentity reads the autoscaler of every member of one identity, each a
// workload of kind in namespace named in members. An identity without members
// has none and lists nothing. A failed read caches nothing and returns a Group
// with Info{Kind: KindNone} and the error; the next read retries.
func (m *NamespacedSnapshot) ForIdentity(ctx context.Context, namespace, kind string, members []string) (Group, error) {
	g := Group{Info: Info{Kind: KindNone}}
	if len(members) == 0 {
		return g, nil
	}
	snap, err := m.namespace(ctx, namespace)
	if err != nil {
		return g, err
	}
	names := slices.Sorted(slices.Values(members))
	g.Members = make(map[string]Info, len(names))
	for _, name := range names {
		info := snap.lookup(kind, name)
		g.Members[name] = info
		if g.Info.Kind == KindNone {
			g.Info = info
		}
		if first := g.Members[names[0]]; info.Kind != first.Kind {
			g.Disagreeing = append(g.Disagreeing, name+"="+string(info.Kind))
		}
	}
	return g, nil
}

// namespace returns the namespace's snapshot, building it on first use.
func (m *NamespacedSnapshot) namespace(ctx context.Context, namespace string) (*snapshot, error) {
	m.mu.Lock()
	e, ok := m.byNS[namespace]
	if !ok {
		e = &nsEntry{}
		m.byNS[namespace] = e
	}
	m.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snap == nil {
		snap, err := buildSnapshot(ctx, m.c, namespace)
		if err != nil {
			return nil, err
		}
		e.snap = snap
	}
	return e.snap, nil
}

// buildSnapshot lists HPAs and ScaledObjects in the namespace once and indexes
// them by scaleTargetRef. A missing KEDA CRD is treated as "no ScaledObjects".
func buildSnapshot(ctx context.Context, c client.Client, namespace string) (*snapshot, error) {
	hpa, err := indexHPAs(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	keda, err := indexScaledObjects(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	return &snapshot{hpa: hpa, keda: keda}, nil
}

// lookup returns the autoscaler Info for the workload (kind, name): a
// ScaledObject (KEDA) wins over an HPA, and Info{Kind: KindNone} is returned
// when neither targets the workload.
func (s *snapshot) lookup(workloadKind, workloadName string) Info {
	key := targetKey{kind: workloadKind, name: workloadName}
	if info, ok := s.keda[key]; ok {
		return info
	}
	if info, ok := s.hpa[key]; ok {
		return info
	}
	return Info{Kind: KindNone}
}

// hpaInfo converts an HPA into an autoscaler Info, defaulting MinReplicas to 1
// when unset (matching the Kubernetes HPA default).
func hpaInfo(hpa *autoscalingv2.HorizontalPodAutoscaler) Info {
	var minR int32 = 1
	if hpa.Spec.MinReplicas != nil {
		minR = *hpa.Spec.MinReplicas
	}
	targets, unknown := extractHPATargets(hpa.Spec.Metrics)
	return Info{
		Kind:              KindHPA,
		Namespace:         hpa.Namespace,
		Name:              hpa.Name,
		MinReplicas:       minR,
		MaxReplicas:       hpa.Spec.MaxReplicas,
		CurrentReplicas:   hpa.Status.CurrentReplicas,
		ConfiguredTargets: targets,
		HasUnknownTrigger: unknown,
	}
}

// scaledObjectInfo converts a KEDA ScaledObject (with its already-extracted
// spec map) into an autoscaler Info.
func scaledObjectInfo(so *unstructured.Unstructured, spec map[string]any) Info {
	status, _ := so.Object["status"].(map[string]any)
	targets, unknown := extractScaledObjectTriggers(spec["triggers"])
	return Info{
		Kind:              KindKEDA,
		Namespace:         so.GetNamespace(),
		Name:              so.GetName(),
		MinReplicas:       int32Or(spec["minReplicaCount"], 1),
		MaxReplicas:       int32Or(spec["maxReplicaCount"], 0),
		CurrentReplicas:   int32Or(status["currentReplicas"], 0),
		ConfiguredTargets: targets,
		HasUnknownTrigger: unknown,
	}
}

func indexHPAs(ctx context.Context, c client.Client, namespace string) (map[targetKey]Info, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list HPAs: %w", err)
	}
	out := make(map[targetKey]Info, len(list.Items))
	for i := range list.Items {
		hpa := &list.Items[i]
		key := targetKey{kind: hpa.Spec.ScaleTargetRef.Kind, name: hpa.Spec.ScaleTargetRef.Name}
		// The first HPA listed for a workload wins.
		if _, exists := out[key]; exists {
			continue
		}
		out[key] = hpaInfo(hpa)
	}
	return out, nil
}

// extractHPATargets walks HPA metrics and returns per-resource averageUtilization
// (cpu/memory only). The bool reports whether at least one non-Resource or
// non-(cpu|memory) metric was present.
func extractHPATargets(metrics []autoscalingv2.MetricSpec) (map[string]int32, bool) {
	out := map[string]int32{}
	unknown := false
	for _, m := range metrics {
		if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
			unknown = true
			continue
		}
		if m.Resource.Target.AverageUtilization == nil {
			unknown = true
			continue
		}
		switch m.Resource.Name {
		case corev1.ResourceCPU:
			out[ResourceCPU] = *m.Resource.Target.AverageUtilization
		case corev1.ResourceMemory:
			out[ResourceMemory] = *m.Resource.Target.AverageUtilization
		default:
			unknown = true
		}
	}
	if len(out) == 0 {
		out = nil
	}
	return out, unknown
}

// indexScaledObjects indexes the namespace's KEDA ScaledObjects by
// scaleTargetRef. A missing KEDA CRD yields an empty (non-nil) index.
func indexScaledObjects(ctx context.Context, c client.Client, namespace string) (map[targetKey]Info, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(scaledObjectListGVK)
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		// CRD not installed → behave as if no ScaledObject exists.
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return map[targetKey]Info{}, nil
		}
		return nil, fmt.Errorf("list ScaledObjects: %w", err)
	}
	out := make(map[targetKey]Info, len(list.Items))
	for i := range list.Items {
		so := &list.Items[i]
		spec, ok := so.Object["spec"].(map[string]any)
		if !ok {
			continue
		}
		ref, ok := spec["scaleTargetRef"].(map[string]any)
		if !ok {
			continue
		}
		key := targetKey{kind: str(ref["kind"]), name: str(ref["name"])}
		// The first ScaledObject listed for a workload wins.
		if _, exists := out[key]; exists {
			continue
		}
		out[key] = scaledObjectInfo(so, spec)
	}
	return out, nil
}

// extractScaledObjectTriggers walks `spec.triggers` (an []any from
// unstructured) and returns per-resource averageUtilization for cpu/memory
// triggers; bool reports presence of any other trigger type.
func extractScaledObjectTriggers(raw any) (map[string]int32, bool) {
	triggers, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := map[string]int32{}
	unknown := false
	for _, t := range triggers {
		m, ok := t.(map[string]any)
		if !ok {
			unknown = true
			continue
		}
		typ := str(m["type"])
		md, _ := m["metadata"].(map[string]any)
		val := str(md["value"])
		switch typ {
		case "cpu":
			if v, err := strconv.ParseInt(val, 10, 32); err == nil {
				out[ResourceCPU] = int32(v)
			} else {
				unknown = true
			}
		case "memory":
			if v, err := strconv.ParseInt(val, 10, 32); err == nil {
				out[ResourceMemory] = int32(v)
			} else {
				unknown = true
			}
		default:
			unknown = true
		}
	}
	if len(out) == 0 {
		out = nil
	}
	return out, unknown
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func int32Or(v any, fallback int32) int32 {
	switch t := v.(type) {
	case int64:
		return clampInt32(t)
	case float64:
		return clampInt32(int64(t))
	case int:
		return clampInt32(int64(t))
	}
	return fallback
}

func clampInt32(v int64) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}
