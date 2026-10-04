package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

	promclient "github.com/noony/k8s-sustain/internal/prometheus"
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

// identityLabels selects every series of one identity, whatever its other
// labels.
func identityLabels(id promclient.WorkloadIdentity) prometheus.Labels {
	return prometheus.Labels{"namespace": id.Namespace, "owner_kind": id.OwnerKind, "owner_name": id.OwnerName}
}

// EmitRetryState sets the identity's single retry-state series: 1 under
// reason when blocked, absent otherwise. A changed reason replaces the old
// series rather than adding a second one.
func EmitRetryState(id promclient.WorkloadIdentity, reason string, blocked bool) {
	workloadRetryState.DeletePartialMatch(identityLabels(id))
	if !blocked {
		return
	}
	workloadRetryState.WithLabelValues(id.Namespace, id.OwnerKind, id.OwnerName, reason).Set(1)
}

// IncrementRetryAttempt counts one failed step of one of the identity's
// members.
func IncrementRetryAttempt(id promclient.WorkloadIdentity) {
	workloadRetryAttempts.WithLabelValues(id.Namespace, id.OwnerKind, id.OwnerName).Inc()
}

// EmitAutoscalerPresent records the kind of the autoscaler shaping the
// identity's recommendation: "None", "HPA" or "KEDA". When the kind changes
// between reconciles, prior label values are cleared so only one series
// remains.
func EmitAutoscalerPresent(id promclient.WorkloadIdentity, autoscalerKind string) {
	autoscalerPresent.DeletePartialMatch(identityLabels(id))
	if autoscalerKind == "" || autoscalerKind == "None" {
		return
	}
	autoscalerPresent.WithLabelValues(id.Namespace, id.OwnerKind, id.OwnerName, autoscalerKind).Set(1)
}

// EmitPolicyRollup sets a policy's live identity count and how many of those
// identities are Blocked.
func EmitPolicyRollup(policy string, identities, blocked int) {
	policyWorkloadCount.WithLabelValues(policy).Set(float64(identities))
	policyBlockedCount.WithLabelValues(policy).Set(float64(blocked))
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
		policyBlockedCount,
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

// EmitAutoscalerTargetsConfigured records the configured averageUtilization
// targets of the autoscaler shaping the identity's recommendation. The
// identity's series are cleared first so that resource removal (e.g. a memory
// trigger dropped) or kind changes never leave stale series behind. Pass
// autoscalerKind="" or "None" to clear-only.
func EmitAutoscalerTargetsConfigured(id promclient.WorkloadIdentity, autoscalerKind string, configured map[string]int32) {
	autoscalerTargetConfigured.DeletePartialMatch(identityLabels(id))
	if autoscalerKind == "" || autoscalerKind == "None" {
		return
	}
	for res, v := range configured {
		autoscalerTargetConfigured.WithLabelValues(id.Namespace, id.OwnerKind, id.OwnerName, autoscalerKind, res).Set(float64(v))
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

func EmitWorkloadPods(id promclient.WorkloadIdentity, c workload.PodCounts) {
	workloadPods.WithLabelValues(id.Namespace, id.OwnerKind, id.OwnerName).Set(float64(c.Total))
	workloadStalePods.WithLabelValues(id.Namespace, id.OwnerKind, id.OwnerName).Set(float64(c.Stale))
}

func DeleteWorkloadPods(id promclient.WorkloadIdentity) {
	workloadPods.DeleteLabelValues(id.Namespace, id.OwnerKind, id.OwnerName)
	workloadStalePods.DeleteLabelValues(id.Namespace, id.OwnerKind, id.OwnerName)
}

// DeleteIdentityHealth removes every health series of an identity no policy
// targets any more, so a departed or re-scoped identity does not keep
// reporting its last state.
func DeleteIdentityHealth(id promclient.WorkloadIdentity) {
	l := identityLabels(id)
	for _, c := range []interface{ DeletePartialMatch(prometheus.Labels) int }{
		workloadPods,
		workloadStalePods,
		workloadRetryState,
		workloadRetryAttempts,
		autoscalerPresent,
		autoscalerTargetConfigured,
		coordinationFactor,
		recycleSuppressedTotal,
	} {
		c.DeletePartialMatch(l)
	}
}
