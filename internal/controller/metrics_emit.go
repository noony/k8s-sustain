package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

	"github.com/noony/k8s-sustain/internal/recommender"
	"github.com/noony/k8s-sustain/internal/workload"
)

// WorkloadMetrics is the per-reconcile snapshot of workload state that we emit
// as gauges.
type WorkloadMetrics struct {
	Namespace, Kind, Name, Policy string
	Containers                    []ContainerMetric
}

// ContainerMetric carries a single container's "current vs recommended" pair.
// Current values are configured resource requests, not live usage.
// HasCPU/HasMemory mark whether a recommendation was actually computed —
// when false (e.g. KeepRequest, no Prometheus data), we skip emitting so we
// don't publish 0-valued recommendations or drift=100%.
type ContainerMetric struct {
	Name                   string
	HasCPU                 bool
	CPUAtFloor             bool
	RecommendedCPUCores    float64
	CurrentCPUCores        float64
	HasMemory              bool
	MemoryAtFloor          bool
	RecommendedMemoryBytes float64
	CurrentMemoryBytes     float64
}

// EmitWorkloadMetrics writes recommendation gauges and drift ratios for one
// reconciled workload. Idempotent: each call overwrites the previous values.
func EmitWorkloadMetrics(w WorkloadMetrics) {
	for _, c := range w.Containers {
		if c.HasCPU {
			recommendedCPUCores.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy).Set(c.RecommendedCPUCores)
			if c.CurrentCPUCores > 0 && !c.CPUAtFloor {
				workloadDriftRatio.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "cpu").Set(c.RecommendedCPUCores / c.CurrentCPUCores)
			} else {
				workloadDriftRatio.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "cpu")
			}
		} else {
			recommendedCPUCores.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy)
			workloadDriftRatio.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "cpu")
		}
		if c.CurrentCPUCores > 0 {
			templateCPUCores.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy).Set(c.CurrentCPUCores)
		} else {
			templateCPUCores.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy)
		}
		if c.HasMemory {
			recommendedMemoryBytes.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy).Set(c.RecommendedMemoryBytes)
			if c.CurrentMemoryBytes > 0 && !c.MemoryAtFloor {
				workloadDriftRatio.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "memory").Set(c.RecommendedMemoryBytes / c.CurrentMemoryBytes)
			} else {
				workloadDriftRatio.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "memory")
			}
		} else {
			recommendedMemoryBytes.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy)
			workloadDriftRatio.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "memory")
		}
		if c.CurrentMemoryBytes > 0 {
			templateMemoryBytes.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy).Set(c.CurrentMemoryBytes)
		} else {
			templateMemoryBytes.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy)
		}
	}
}

// EmitRetryState marks a workload as blocked (state=1) for the given reason.
// When blocked is false, every reason variant for the workload is removed so
// that no stale series persists at 1 after recovery.
func EmitRetryState(namespace, kind, name, reason string, blocked bool) {
	if !blocked {
		workloadRetryState.DeletePartialMatch(prometheus.Labels{
			"namespace": namespace, "owner_kind": kind, "owner_name": name,
		})
		return
	}
	workloadRetryState.WithLabelValues(namespace, kind, name, reason).Set(1)
}

// IncrementRetryAttempt bumps the retry counter for a workload.
func IncrementRetryAttempt(namespace, kind, name string) {
	workloadRetryAttempts.WithLabelValues(namespace, kind, name).Inc()
}

// EmitAutoscalerPresent records the autoscaler kind targeting the workload.
// kind is one of "None", "HPA", "KEDA". When the kind changes between reconciles,
// prior label values for the workload are cleared so only one series remains.
func EmitAutoscalerPresent(namespace, ownerKind, ownerName, autoscalerKind string) {
	autoscalerPresent.DeletePartialMatch(prometheus.Labels{
		"namespace":  namespace,
		"owner_kind": ownerKind,
		"owner_name": ownerName,
	})
	if autoscalerKind == "" || autoscalerKind == "None" {
		return
	}
	autoscalerPresent.WithLabelValues(namespace, ownerKind, ownerName, autoscalerKind).Set(1)
}

// EmitPolicyRollup sets per-policy workload and at-risk counts after a reconcile.
func EmitPolicyRollup(policy string, workloadCount, atRiskCount int) {
	policyWorkloadCount.WithLabelValues(policy).Set(float64(workloadCount))
	policyAtRiskCount.WithLabelValues(policy).Set(float64(atRiskCount))
}

// emitWorkloadFromRecs builds and emits WorkloadMetrics from the workload's
// container specs (current requests) and the per-container recommendations.
func emitWorkloadFromRecs(t *workloadTarget, policyName string, recs map[string]workload.ContainerRecommendation) {
	m := WorkloadMetrics{
		Namespace: t.Namespace,
		Kind:      t.Kind,
		Name:      t.Name,
		Policy:    policyName,
	}
	for _, c := range t.Containers {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}
		cm := ContainerMetric{Name: c.Name}
		// When the recommendation lands at the floor (1m / 1Mi) it is the
		// "no real usage data" sentinel — emit the value but skip drift so the
		// dashboard doesn't surface a misleading "huge over-provisioning" signal.
		floorCPU := recommender.MinCPURequest()
		floorMem := recommender.MinMemoryRequest()
		if rec.CPURequest != nil {
			cm.HasCPU = true
			cm.RecommendedCPUCores = float64(rec.CPURequest.MilliValue()) / 1000.0
			cm.CPUAtFloor = rec.CPURequest.Cmp(*floorCPU) <= 0
		}
		if rec.MemoryRequest != nil {
			cm.HasMemory = true
			cm.RecommendedMemoryBytes = float64(rec.MemoryRequest.Value())
			cm.MemoryAtFloor = rec.MemoryRequest.Cmp(*floorMem) <= 0
		}
		if cur := containerRequestCPUCores(c); cur > 0 {
			cm.CurrentCPUCores = cur
		}
		if cur := containerRequestMemoryBytes(c); cur > 0 {
			cm.CurrentMemoryBytes = cur
		}
		m.Containers = append(m.Containers, cm)
	}
	if len(m.Containers) == 0 {
		return
	}
	EmitWorkloadMetrics(m)
}

// EmitAutoscalerTargetsConfigured records configured autoscaler averageUtilization
// targets for a workload. Per-workload series are cleared first so that resource
// removal (e.g. a memory trigger dropped) or kind changes never leave stale
// series behind. Pass autoscalerKind="" or "None" to clear-only.
func EmitAutoscalerTargetsConfigured(namespace, ownerKind, ownerName, autoscalerKind string, configured map[string]int32) {
	wl := prometheus.Labels{
		"namespace":  namespace,
		"owner_kind": ownerKind,
		"owner_name": ownerName,
	}
	autoscalerTargetConfigured.DeletePartialMatch(wl)

	if autoscalerKind == "" || autoscalerKind == "None" {
		return
	}
	for res, v := range configured {
		autoscalerTargetConfigured.WithLabelValues(namespace, ownerKind, ownerName, autoscalerKind, res).Set(float64(v))
	}
}

// EmitCoordinationFactor records the multiplier applied for one resource and
// factor kind. Pass 1.0 to clear (matches "no effect").
func EmitCoordinationFactor(namespace, ownerKind, ownerName, resourceKey, factorKind string, factor float64) {
	coordinationFactor.With(prometheus.Labels{
		"namespace": namespace, "owner_kind": ownerKind, "owner_name": ownerName,
		"resource": resourceKey, "kind": factorKind,
	}).Set(factor)
}

// containerRequestCPUCores returns the CPU request in cores, or 0 if unset.
func containerRequestCPUCores(c corev1.Container) float64 {
	q := c.Resources.Requests.Cpu()
	if q == nil || q.IsZero() {
		return 0
	}
	return float64(q.MilliValue()) / 1000.0
}

// containerRequestMemoryBytes returns the memory request in bytes, or 0 if unset.
func containerRequestMemoryBytes(c corev1.Container) float64 {
	q := c.Resources.Requests.Memory()
	if q == nil || q.IsZero() {
		return 0
	}
	return float64(q.Value())
}
