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
// HasCPU/HasMemory mark whether a recommendation was actually computed —
// when false (e.g. KeepRequest, no Prometheus data), we skip emitting so we
// don't publish 0-valued recommendations.
type ContainerMetric struct {
	Name                   string
	Kind                   string // "regular" or "init"
	HasCPU                 bool
	RecommendedCPUCores    float64
	CurrentCPUCores        float64
	HasMemory              bool
	RecommendedMemoryBytes float64
	CurrentMemoryBytes     float64
}

// Container kind label values for the container_kind metric label.
const (
	ContainerKindRegular = "regular"
	ContainerKindInit    = "init"
)

// EmitWorkloadMetrics writes recommendation and template gauges for one
// reconciled workload. Idempotent: each call overwrites the previous values.
func EmitWorkloadMetrics(w WorkloadMetrics) {
	// Per-container recommendation + template gauges still carry container
	// labels: the savings recording rules join the two metrics on `container`,
	// and operators rely on container_kind="init" to verify init-container
	// recommendations during local testing.
	for _, c := range w.Containers {
		kind := c.Kind
		if kind == "" {
			kind = ContainerKindRegular
		}
		if c.HasCPU {
			recommendedCPUCores.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy).Set(c.RecommendedCPUCores)
		} else {
			recommendedCPUCores.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy)
		}
		if c.CurrentCPUCores > 0 {
			templateCPUCores.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy).Set(c.CurrentCPUCores)
		} else {
			templateCPUCores.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy)
		}
		if c.HasMemory {
			recommendedMemoryBytes.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy).Set(c.RecommendedMemoryBytes)
		} else {
			recommendedMemoryBytes.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy)
		}
		if c.CurrentMemoryBytes > 0 {
			templateMemoryBytes.WithLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy).Set(c.CurrentMemoryBytes)
		} else {
			templateMemoryBytes.DeleteLabelValues(w.Namespace, w.Kind, w.Name, c.Name, kind, w.Policy)
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

// EmitPolicyBatchCoverage records how many workload identities a policy's
// sharded Prometheus batch prefetch requested this cycle versus how many
// resolved with at least one usable sample (recommender.BatchInputs).
//
// A capacity signal, not a failure signal: a workload with no history yet looks
// the same here as one Prometheus never answered for -- see
// EmitPolicyBatchFailures. Kept as two gauges rather than one ratio so
// "0 requested" stays distinguishable from "N requested, 0 resolved".
func EmitPolicyBatchCoverage(policy string, requested, resolved int) {
	policyBatchRequested.WithLabelValues(policy).Set(float64(requested))
	policyBatchResolved.WithLabelValues(policy).Set(float64(resolved))
}

// EmitPolicyBatchFailures adds to the cumulative count of workload identities
// whose batch Prometheus fetch genuinely failed for a policy this cycle (see
// recommender.BatchStats.Failures). Never merged with EmitPolicyBatchCoverage's
// gauges, so "Prometheus is unwell" stays distinguishable from "no data yet".
// A no-op on failures<=0 so a healthy cycle never creates a zero-valued series.
func EmitPolicyBatchFailures(policy string, failures int) {
	if failures <= 0 {
		return
	}
	policyBatchFailuresTotal.WithLabelValues(policy).Add(float64(failures))
}

// DeletePolicyMetrics removes every series carrying this policy's label, for
// use when the Policy itself is deleted.
//
// Without it a Policy's series outlive the object forever: a gauge keeps
// exporting its last value, so a deleted policy is indistinguishable from a
// live one matching nothing, and cardinality grows with every policy ever seen.
//
// The per-WORKLOAD vectors are covered deliberately: their policy label names
// the policy that produced them, and the reconcile that would have refreshed or
// removed them is exactly the one that stops happening.
//
// DeletePartialMatch is used uniformly so the single-label and multi-label
// vectors (reconcile_total carries policy+result) are handled the same way.
func DeletePolicyMetrics(policy string) {
	l := prometheus.Labels{"policy": policy}
	for _, c := range []interface{ DeletePartialMatch(prometheus.Labels) int }{
		reconcileTotal,
		reconcileDuration,
		policyWorkloadCount,
		policyAtRiskCount,
		policyBatchRequested,
		policyBatchResolved,
		policyBatchFailuresTotal,
		recommendedCPUCores,
		templateCPUCores,
		recommendedMemoryBytes,
		templateMemoryBytes,
	} {
		c.DeletePartialMatch(l)
	}
}

// emitWorkloadFromRecs builds and emits WorkloadMetrics from the workload's
// container specs (current requests) and the per-container recommendations.
// initNames identifies which container names originated as InitContainers so
// their emitted samples carry container_kind="init".
func emitWorkloadFromRecs(t *workloadTarget, policyName string, recs map[string]workload.ContainerRecommendation, initNames map[string]struct{}) {
	m := WorkloadMetrics{
		Namespace: t.Namespace,
		Kind:      t.Kind,
		Name:      t.Name,
		Policy:    policyName,
	}
	all := make([]corev1.Container, 0, len(t.Containers)+len(t.InitContainers))
	all = append(all, t.Containers...)
	all = append(all, t.InitContainers...)
	for _, c := range all {
		rec, ok := recs[c.Name]
		if !ok {
			continue
		}
		kind := ContainerKindRegular
		if _, isInit := initNames[c.Name]; isInit {
			kind = ContainerKindInit
		}
		cm := ContainerMetric{Name: c.Name, Kind: kind}
		if rec.CPURequest != nil {
			cm.HasCPU = true
			cm.RecommendedCPUCores = float64(rec.CPURequest.MilliValue()) / 1000.0
		}
		if rec.MemoryRequest != nil {
			cm.HasMemory = true
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

func EmitWorkloadPods(namespace, kind, name string, c workload.PodCounts) {
	workloadPods.WithLabelValues(namespace, kind, name).Set(float64(c.Total))
	workloadStalePods.WithLabelValues(namespace, kind, name).Set(float64(c.Stale))
}

func DeleteWorkloadPods(namespace, kind, name string) {
	workloadPods.DeleteLabelValues(namespace, kind, name)
	workloadStalePods.DeleteLabelValues(namespace, kind, name)
}
