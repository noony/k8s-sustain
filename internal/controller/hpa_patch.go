package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// patchHpaToAverageValue converts utilization-based CPU/memory metrics in the HPA
// to absolute AverageValue metrics. The absolute value is computed as:
// averageValue = currentRequest * (targetUtilization / 100).
// Metrics that are already AverageValue or non-resource metrics are left untouched.
func patchHpaToAverageValue(ctx context.Context, c client.Client, hpa *autoscalingv2.HorizontalPodAutoscaler, currentRequests map[corev1.ResourceName]resource.Quantity) error {
	logger := log.FromContext(ctx).WithValues("hpa", hpa.Name, "namespace", hpa.Namespace)

	base := hpa.DeepCopy()
	changed := false

	for i, m := range hpa.Spec.Metrics {
		if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
			continue
		}
		if m.Resource.Target.Type != autoscalingv2.UtilizationMetricType || m.Resource.Target.AverageUtilization == nil {
			continue
		}

		currentReq, ok := currentRequests[m.Resource.Name]
		if !ok {
			logger.V(1).Info("skipping metric: no matching container request",
				"resource", m.Resource.Name)
			continue
		}

		// averageValue = currentRequest * targetUtilization / 100
		millis := currentReq.MilliValue() * int64(*m.Resource.Target.AverageUtilization) / 100
		avgVal := resource.NewMilliQuantity(millis, currentReq.Format)

		logger.Info("converting HPA metric Utilization → AverageValue",
			"resource", m.Resource.Name,
			"targetUtilization", *m.Resource.Target.AverageUtilization,
			"currentRequest", currentReq.String(),
			"computedAverageValue", avgVal.String())

		hpa.Spec.Metrics[i].Resource.Target.Type = autoscalingv2.AverageValueMetricType
		hpa.Spec.Metrics[i].Resource.Target.AverageValue = avgVal
		hpa.Spec.Metrics[i].Resource.Target.AverageUtilization = nil
		changed = true
	}

	if !changed {
		logger.V(1).Info("HPA has no utilization metrics to convert; no patch applied")
		return nil
	}

	logger.Info("patching HPA spec.metrics to AverageValue")
	return c.Patch(ctx, hpa, client.MergeFrom(base))
}
