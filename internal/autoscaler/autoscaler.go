// Package autoscaler provides read-only detection of Horizontal Pod Autoscalers
// and KEDA ScaledObjects targeting a workload. It never writes to either object.
//
// When both an HPA and a ScaledObject target the same workload, the ScaledObject
// is treated as canonical (KEDA owns and regenerates the HPA from it).
package autoscaler

import (
	"context"
	"fmt"
	"strconv"

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
	Kind        Kind
	Namespace   string
	Name        string
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

// Detect inspects the cluster for an HPA or ScaledObject targeting
// (kind=workloadKind, name=workloadName) in the given namespace.
//
// Order of precedence:
//  1. ScaledObject (KEDA) — wins over HPA because KEDA owns the HPA it generates.
//  2. HPA           — fallback when no ScaledObject targets the workload.
//  3. None          — neither found.
//
// Missing KEDA CRD is treated as "no ScaledObject" (the function falls back to HPA).
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
		var minR int32 = 1
		if hpa.Spec.MinReplicas != nil {
			minR = *hpa.Spec.MinReplicas
		}
		targets, unknown := extractHPATargets(hpa.Spec.Metrics)
		return &Info{
			Kind:              KindHPA,
			Namespace:         hpa.Namespace,
			Name:              hpa.Name,
			MinReplicas:       minR,
			MaxReplicas:       hpa.Spec.MaxReplicas,
			CurrentReplicas:   hpa.Status.CurrentReplicas,
			ConfiguredTargets: targets,
			HasUnknownTrigger: unknown,
		}, nil
	}
	return nil, nil
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
		spec, ok := so.Object["spec"].(map[string]interface{})
		if !ok {
			continue
		}
		ref, ok := spec["scaleTargetRef"].(map[string]interface{})
		if !ok {
			continue
		}
		if str(ref["kind"]) != workloadKind || str(ref["name"]) != workloadName {
			continue
		}
		minR := int32Or(spec["minReplicaCount"], 1)
		maxR := int32Or(spec["maxReplicaCount"], 0)
		status, _ := so.Object["status"].(map[string]interface{})
		curR := int32Or(status["currentReplicas"], 0)
		targets, unknown := extractScaledObjectTriggers(spec["triggers"])
		return &Info{
			Kind:              KindKEDA,
			Namespace:         so.GetNamespace(),
			Name:              so.GetName(),
			MinReplicas:       minR,
			MaxReplicas:       maxR,
			CurrentReplicas:   curR,
			ConfiguredTargets: targets,
			HasUnknownTrigger: unknown,
		}, nil
	}
	return nil, nil
}

// extractScaledObjectTriggers walks `spec.triggers` (an []interface{} from
// unstructured) and returns per-resource averageUtilization for cpu/memory
// triggers; bool reports presence of any other trigger type.
func extractScaledObjectTriggers(raw interface{}) (map[string]int32, bool) {
	triggers, ok := raw.([]interface{})
	if !ok {
		return nil, false
	}
	out := map[string]int32{}
	unknown := false
	for _, t := range triggers {
		m, ok := t.(map[string]interface{})
		if !ok {
			unknown = true
			continue
		}
		typ := str(m["type"])
		md, _ := m["metadata"].(map[string]interface{})
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

func str(v interface{}) string {
	s, _ := v.(string)
	return s
}

func int32Or(v interface{}, fallback int32) int32 {
	switch t := v.(type) {
	case int64:
		return int32(t)
	case float64:
		return int32(t)
	case int:
		return int32(t)
	}
	return fallback
}
