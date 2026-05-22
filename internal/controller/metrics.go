package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	reconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_reconcile_total",
		Help: "Total number of policy reconciliations by result.",
	}, []string{"policy", "result"})

	reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "k8s_sustain_reconcile_duration_seconds",
		Help:    "Duration of a policy reconciliation in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"policy"})

	workloadPatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_workload_patch_total",
		Help: "Total number of workload patches by kind and result.",
	}, []string{"kind", "result"})

	recommendedCPUCores = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_recommended_cpu_cores",
		Help: "Current CPU recommendation in cores for a workload's container, by policy.",
	}, []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"})

	recommendedMemoryBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_recommended_memory_bytes",
		Help: "Current memory recommendation in bytes for a workload's container, by policy.",
	}, []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"})

	templateCPUCores = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_workload_template_cpu_cores",
		Help: "CPU request from the workload's pod-template spec (the 'original' value) in cores. Stable across webhook injection so savings rules can compare against it.",
	}, []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"})

	templateMemoryBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_workload_template_memory_bytes",
		Help: "Memory request from the workload's pod-template spec (the 'original' value) in bytes.",
	}, []string{"namespace", "owner_kind", "owner_name", "container", "container_kind", "policy"})

	workloadDriftRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_workload_drift_ratio",
		Help: "Largest absolute drift ratio (recommended / current) across the workload's containers, per resource. 1.0 means no container has drifted. Aggregated at emit time so this metric stays O(workload), not O(workload × container).",
	}, []string{"namespace", "owner_kind", "owner_name", "resource"})

	workloadRetryState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_workload_retry_state",
		Help: "1 when the workload is currently in retry-backoff, 0 otherwise.",
	}, []string{"namespace", "owner_kind", "owner_name", "reason"})

	workloadRetryAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_workload_retry_attempts",
		Help: "Total retry attempts per workload.",
	}, []string{"namespace", "owner_kind", "owner_name"})

	policyWorkloadCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_policy_workload_count",
		Help: "Number of workloads matched by a policy.",
	}, []string{"policy"})

	policyAtRiskCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_policy_at_risk_count",
		Help: "Number of policy-matched workloads in a risk state (OOM, drift, blocked).",
	}, []string{"policy"})

	autoscalerPresent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_sustain_autoscaler_present",
			Help: "Set to 1 when an autoscaler (HPA or KEDA ScaledObject) targets the workload, with the autoscaler kind as a label.",
		},
		[]string{"namespace", "owner_kind", "owner_name", "kind"},
	)

	autoscalerTargetConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_sustain_autoscaler_target_configured",
			Help: "Configured autoscaler averageUtilization (%) for a workload's resource trigger.",
		},
		[]string{"namespace", "owner_kind", "owner_name", "kind", "resource"},
	)

	recommendationSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_recommendation_skipped_total",
		Help: "Recommendations skipped without emitting, by reason (e.g. insufficient_history).",
	}, []string{"namespace", "owner_kind", "owner_name", "reason"})

	oomFloorApplied = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_oom_floor_applied_total",
		Help: "Memory recommendations where the recent-OOM floor raised the value above the percentile.",
	}, []string{"namespace", "owner_kind", "owner_name", "container"})

	coordinationFactor = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8s_sustain_coordination_factor",
			Help: "Multiplier applied by autoscaler coordination. 1.0 when off or no match.",
		},
		[]string{"namespace", "owner_kind", "owner_name", "resource", "kind"},
	)

	oomObservedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_oom_observed_total",
		Help: "Total OOM kills observed by the active Pod watcher.",
	}, []string{"namespace", "owner_kind", "owner_name", "container"})

	oomReactionLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "k8s_sustain_oom_reaction_latency_seconds",
		Help:    "Seconds between an OOM kill (live record TerminatedAt) and the floor-driven memory recommendation that responds to it.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200},
	}, []string{"namespace", "owner_kind", "owner_name"})

	oomCacheEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "k8s_sustain_oom_cache_entries",
		Help: "Current number of distinct workload+container entries in the OOM watch cache.",
	})
)

func init() {
	metrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDuration,
		workloadPatchTotal,
		recommendedCPUCores,
		recommendedMemoryBytes,
		templateCPUCores,
		templateMemoryBytes,
		workloadDriftRatio,
		workloadRetryState,
		workloadRetryAttempts,
		policyWorkloadCount,
		policyAtRiskCount,
		autoscalerPresent,
		autoscalerTargetConfigured,
		coordinationFactor,
		recommendationSkipped,
		oomFloorApplied,
		oomObservedTotal,
		oomReactionLatencySeconds,
		oomCacheEntries,
	)
}

// EmitOOMObserved increments the observed-OOM counter for a workload container.
func EmitOOMObserved(namespace, ownerKind, ownerName, container string) {
	oomObservedTotal.WithLabelValues(namespace, ownerKind, ownerName, container).Inc()
}

// EmitOOMReactionLatency records the elapsed time between the cache record's
// TerminatedAt and the floor-driven memory recommendation that responds to it.
func EmitOOMReactionLatency(namespace, ownerKind, ownerName string, seconds float64) {
	oomReactionLatencySeconds.WithLabelValues(namespace, ownerKind, ownerName).Observe(seconds)
}

// SetOOMCacheEntries sets the current cache size gauge.
func SetOOMCacheEntries(n int) {
	oomCacheEntries.Set(float64(n))
}
