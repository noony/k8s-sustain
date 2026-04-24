package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// hpaDetection holds the detected HPA target utilization percentages for CPU and memory.
// Nil fields mean the HPA does not use that resource's utilization metric.
type hpaDetection struct {
	cpuUtilization    *int32
	memoryUtilization *int32
	hpa               *autoscalingv2.HorizontalPodAutoscaler
}

// detectHpa finds an HPA targeting the given workload and extracts its CPU/memory
// utilization targets. Returns nil when no matching HPA is found, or the HPA uses
// only custom/external metrics (no utilization-based resource metrics).
//
// When hpaConfig contains overrides, those values replace the auto-detected ones.
func detectHpa(ctx context.Context, c client.Client, namespace, kind, name string, hpaConfig *sustainv1alpha1.HpaConfig) (*hpaDetection, error) {
	logger := log.FromContext(ctx).WithValues("kind", kind, "name", name, "namespace", namespace)

	var hpaList autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &hpaList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	logger.V(1).Info("listed HPAs in namespace", "count", len(hpaList.Items))

	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		if hpa.Spec.ScaleTargetRef.Kind != kind || hpa.Spec.ScaleTargetRef.Name != name {
			continue
		}
		logger.V(1).Info("HPA matches workload", "hpa", hpa.Name, "metrics", len(hpa.Spec.Metrics))

		det := &hpaDetection{hpa: hpa}
		for _, m := range hpa.Spec.Metrics {
			if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
				logger.V(1).Info("skipping non-resource metric", "hpa", hpa.Name, "metricType", m.Type)
				continue
			}
			if m.Resource.Target.Type != autoscalingv2.UtilizationMetricType || m.Resource.Target.AverageUtilization == nil {
				logger.V(1).Info("skipping non-utilization metric", "hpa", hpa.Name, "resource", m.Resource.Name, "targetType", m.Resource.Target.Type)
				continue
			}
			switch m.Resource.Name {
			case corev1.ResourceCPU:
				v := *m.Resource.Target.AverageUtilization
				det.cpuUtilization = &v
				logger.V(1).Info("detected CPU utilization target", "hpa", hpa.Name, "targetUtilization", v)
			case corev1.ResourceMemory:
				v := *m.Resource.Target.AverageUtilization
				det.memoryUtilization = &v
				logger.V(1).Info("detected memory utilization target", "hpa", hpa.Name, "targetUtilization", v)
			}
		}

		// Apply overrides from the Policy.
		if hpaConfig != nil {
			if hpaConfig.CPU != nil && hpaConfig.CPU.TargetUtilizationOverride != nil {
				v := *hpaConfig.CPU.TargetUtilizationOverride
				logger.Info("applying CPU target-utilization override from policy", "hpa", hpa.Name, "override", v)
				det.cpuUtilization = &v
			}
			if hpaConfig.Memory != nil && hpaConfig.Memory.TargetUtilizationOverride != nil {
				v := *hpaConfig.Memory.TargetUtilizationOverride
				logger.Info("applying memory target-utilization override from policy", "hpa", hpa.Name, "override", v)
				det.memoryUtilization = &v
			}
		}

		// If no utilization-based metrics were found (and no overrides set), return nil.
		if det.cpuUtilization == nil && det.memoryUtilization == nil {
			logger.V(1).Info("HPA has no utilization-based metrics, no adjustment will be made", "hpa", hpa.Name)
			return nil, nil
		}

		logger.Info("HPA detected for workload",
			"hpa", hpa.Name,
			"cpuTargetUtilization", det.cpuUtilization,
			"memoryTargetUtilization", det.memoryUtilization)
		return det, nil
	}

	logger.V(1).Info("no HPA targets workload")
	return nil, nil
}

// resolveHpaMode returns the effective HPA mode from the policy config.
// When hpa config is nil or mode is unset (empty string), defaults to HpaAware.
func resolveHpaMode(policy *sustainv1alpha1.Policy) sustainv1alpha1.HpaMode {
	if policy.Spec.RightSizing.UpdatePolicy.Hpa != nil && policy.Spec.RightSizing.UpdatePolicy.Hpa.Mode != "" {
		return policy.Spec.RightSizing.UpdatePolicy.Hpa.Mode
	}
	return sustainv1alpha1.HpaModeHpaAware
}

// extractCurrentRequests returns the aggregate resource requests across all containers.
// Used to compute the absolute AverageValue for UpdateTargetValue mode.
func extractCurrentRequests(containers []corev1.Container) map[corev1.ResourceName]resource.Quantity {
	reqs := make(map[corev1.ResourceName]resource.Quantity)
	for _, c := range containers {
		if c.Resources.Requests != nil {
			for name, qty := range c.Resources.Requests {
				if _, exists := reqs[name]; !exists {
					reqs[name] = qty
				}
			}
		}
	}
	return reqs
}
