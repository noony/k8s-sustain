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
// single namespace. It is the index key shared by Detect and Snapshot.Lookup.
type targetKey struct {
	kind string
	name string
}

// Snapshot is a per-namespace, pre-built index of autoscaler targets. It lets a
// caller that processes many workloads in one namespace pay the two List calls
// (HPAs + ScaledObjects) ONCE and then resolve each workload via an O(1) map
// lookup, instead of re-listing per workload (the controller N+1).
//
// Snapshot is safe for concurrent Lookup calls. BuildSnapshot performs the I/O;
// Lookup is pure map access.
type Snapshot struct {
	// hpa / keda map scaleTargetRef (kind,name) -> the detected Info. Built once
	// by BuildSnapshot. A nil map (e.g. when the KEDA CRD is absent) is treated
	// as empty.
	hpa  map[targetKey]Info
	keda map[targetKey]Info
}

// NamespacedSnapshot builds and caches one Snapshot per namespace on first
// successful access. A single reconcile pass may match workloads across several
// namespaces; this lets each namespace pay its two List calls once on success
// regardless of how many workloads it contains, while never listing namespaces
// the pass doesn't touch. Failed builds are NOT cached: a transient List blip
// fails only the Lookup that hit it, and the next Lookup for that namespace
// retries BuildSnapshot (parity with the old per-workload Detect, which retried
// independently). Under persistent API failure each Lookup therefore pays the
// two List calls, matching Detect's cost. It is safe for concurrent Lookup calls
// (the controller processes workloads in parallel); same-namespace builds are
// serialized behind a per-entry mutex while different namespaces never block
// each other.
type NamespacedSnapshot struct {
	c client.Client

	mu   sync.Mutex
	byNS map[string]*nsEntry
}

type nsEntry struct {
	// mu serializes BuildSnapshot for a single namespace so concurrent first
	// Lookups don't double-build; snap is nil until the first successful build.
	mu   sync.Mutex
	snap *Snapshot
}

// NewNamespacedSnapshot returns a lazy, per-namespace snapshot bound to the
// client. No I/O happens until the first Lookup for a namespace.
func NewNamespacedSnapshot(c client.Client) *NamespacedSnapshot {
	return &NamespacedSnapshot{c: c, byNS: map[string]*nsEntry{}}
}

// Lookup returns the autoscaler Info for (kind, name) in namespace, building the
// namespace's Snapshot on first use (and caching it on success) using ctx for the
// I/O. On a build error it logs nothing, caches nothing, and returns Info{Kind:
// KindNone} plus the error, leaving the caller to decide how to surface it
// (matching Detect's contract); the next Lookup for the namespace retries the
// build.
func (m *NamespacedSnapshot) Lookup(ctx context.Context, namespace, workloadKind, workloadName string) (Info, error) {
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
		snap, err := BuildSnapshot(ctx, m.c, namespace)
		if err != nil {
			// Don't cache failures: the next Lookup retries BuildSnapshot.
			return Info{Kind: KindNone}, err
		}
		e.snap = snap
	}
	return e.snap.Lookup(workloadKind, workloadName), nil
}

// BuildSnapshot lists HPAs and ScaledObjects in the namespace ONCE and indexes
// them by scaleTargetRef so subsequent Lookups are O(1). A missing KEDA CRD is
// treated as "no ScaledObjects" (the ScaledObject index is simply empty), the
// same fallback Detect applies.
func BuildSnapshot(ctx context.Context, c client.Client, namespace string) (*Snapshot, error) {
	hpa, err := indexHPAs(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	keda, err := indexScaledObjects(ctx, c, namespace)
	if err != nil {
		return nil, err
	}
	return &Snapshot{hpa: hpa, keda: keda}, nil
}

// Lookup returns the autoscaler Info for the workload (kind, name), applying the
// same precedence as Detect: ScaledObject (KEDA) wins over HPA, and Info{Kind:
// KindNone} is returned when neither targets the workload.
func (s *Snapshot) Lookup(workloadKind, workloadName string) Info {
	key := targetKey{kind: workloadKind, name: workloadName}
	if info, ok := s.keda[key]; ok {
		return info
	}
	if info, ok := s.hpa[key]; ok {
		return info
	}
	return Info{Kind: KindNone}
}

// Detect inspects the cluster for an HPA or ScaledObject targeting
// (kind=workloadKind, name=workloadName) in the given namespace.
//
// Order of precedence:
//  1. ScaledObject (KEDA) — wins over HPA because KEDA owns the HPA it generates.
//  2. HPA           — fallback when no ScaledObject targets the workload.
//  3. None          — neither found.
//
// Missing KEDA CRD is treated as "no ScaledObject" (the function falls back to HPA).
//
// Detect is the single-workload entry point (used by the webhook hot path). It
// short-circuits on the first matching target instead of indexing every HPA and
// ScaledObject in the namespace: it lists ScaledObjects first and returns on the
// first matching scaleTargetRef, only listing HPAs when no ScaledObject matches.
// Callers resolving many workloads in one namespace should BuildSnapshot once and
// Lookup instead, to amortise the two List calls.
//
// Its result is identical to Snapshot.Lookup (same KEDA-over-HPA precedence, same
// scaleTargetRef matching); the equivalence tests guard that contract.
func Detect(ctx context.Context, c client.Client, namespace, workloadKind, workloadName string) (Info, error) {
	if so, err := lookupScaledObject(ctx, c, namespace, workloadKind, workloadName); err != nil {
		return Info{Kind: KindNone}, err
	} else if so != nil {
		return *so, nil
	}

	if hpa, err := lookupHPA(ctx, c, namespace, workloadKind, workloadName); err != nil {
		return Info{Kind: KindNone}, err
	} else if hpa != nil {
		return *hpa, nil
	}

	return Info{Kind: KindNone}, nil
}

// lookupHPA lists HPAs in the namespace and returns the first one whose
// scaleTargetRef matches (kind, name), or nil if none does. It mirrors the
// per-item extraction in indexHPAs so Detect and Snapshot.Lookup agree.
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

func lookupHPA(ctx context.Context, c client.Client, namespace, workloadKind, workloadName string) (*Info, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list HPAs: %w", err)
	}
	for i := range list.Items {
		hpa := &list.Items[i]
		if hpa.Spec.ScaleTargetRef.Kind != workloadKind || hpa.Spec.ScaleTargetRef.Name != workloadName {
			continue
		}
		info := hpaInfo(hpa)
		return &info, nil
	}
	return nil, nil
}

// lookupScaledObject lists KEDA ScaledObjects in the namespace and returns the
// first one whose scaleTargetRef matches (kind, name), or nil if none does. A
// missing KEDA CRD is treated as "no ScaledObject" (nil, nil). It mirrors the
// per-item extraction in indexScaledObjects so Detect and Snapshot.Lookup agree.
func lookupScaledObject(ctx context.Context, c client.Client, namespace, workloadKind, workloadName string) (*Info, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(scaledObjectListGVK)
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		// CRD not installed → behave as if no ScaledObject exists.
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list ScaledObjects: %w", err)
	}
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
		if str(ref["kind"]) != workloadKind || str(ref["name"]) != workloadName {
			continue
		}
		info := scaledObjectInfo(so, spec)
		return &info, nil
	}
	return nil, nil
}

// indexHPAs lists all HPAs in the namespace and indexes them by scaleTargetRef.
func indexHPAs(ctx context.Context, c client.Client, namespace string) (map[targetKey]Info, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list HPAs: %w", err)
	}
	out := make(map[targetKey]Info, len(list.Items))
	for i := range list.Items {
		hpa := &list.Items[i]
		key := targetKey{kind: hpa.Spec.ScaleTargetRef.Kind, name: hpa.Spec.ScaleTargetRef.Name}
		// First HPA targeting a given workload wins, matching the old
		// linear-scan-and-return-first behaviour.
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

// indexScaledObjects lists all KEDA ScaledObjects in the namespace and indexes
// them by scaleTargetRef. A missing KEDA CRD yields an empty (non-nil) index.
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
		// First ScaledObject targeting a given workload wins, matching the old
		// linear-scan-and-return-first behaviour.
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
