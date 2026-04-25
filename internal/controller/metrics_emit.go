package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

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
type ContainerMetric struct {
	Name                   string
	RecommendedCPUCores    float64
	CurrentCPUCores        float64
	RecommendedMemoryBytes float64
	CurrentMemoryBytes     float64
}

// EmitWorkloadMetrics writes recommendation gauges and drift ratios for one
// reconciled workload. Idempotent: each call overwrites the previous values.
func EmitWorkloadMetrics(w WorkloadMetrics) {
	for _, c := range w.Containers {
		recommendedCPUCores.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy).Set(c.RecommendedCPUCores)
		recommendedMemoryBytes.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, w.Policy).Set(c.RecommendedMemoryBytes)
		if c.CurrentCPUCores > 0 {
			workloadDriftRatio.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "cpu").Set(c.RecommendedCPUCores / c.CurrentCPUCores)
		}
		if c.CurrentMemoryBytes > 0 {
			workloadDriftRatio.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, "memory").Set(c.RecommendedMemoryBytes / c.CurrentMemoryBytes)
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

// EmitHPAPresent marks whether an HPA targets the workload, with the active
// sustain mode. mode is one of "HpaAware", "UpdateTargetValue", "Ignore".
// When the mode changes between reconciles, prior mode variants for the
// workload are cleared so only one series remains.
func EmitHPAPresent(namespace, kind, name, mode string, present bool) {
	if !present {
		hpaPresent.DeletePartialMatch(prometheus.Labels{
			"namespace": namespace, "owner_kind": kind, "owner_name": name,
		})
		return
	}
	// Remove any prior mode entries for this workload, then set the current mode.
	hpaPresent.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace, "owner_kind": kind, "owner_name": name,
	})
	hpaPresent.WithLabelValues(namespace, kind, name, mode).Set(1)
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
		if rec.CPURequest != nil {
			cm.RecommendedCPUCores = float64(rec.CPURequest.MilliValue()) / 1000.0
		}
		if rec.MemoryRequest != nil {
			cm.RecommendedMemoryBytes = float64(rec.MemoryRequest.Value())
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
