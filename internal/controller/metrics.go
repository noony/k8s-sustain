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
		Help: "Recommendations skipped without emitting, by reason (e.g. workload_too_young).",
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

	recycleSuppressedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_recycle_suppressed_total",
		Help: "Resource decreases not applied because they fell below the policy's downsizeThreshold, by resource. Counted once per resource per pod processed per reconcile.",
	}, []string{"namespace", "owner_kind", "owner_name", "resource"})

	// policyBatchRequested and policyBatchResolved are a coverage/capacity
	// pair, deliberately separate from policyBatchFailuresTotal below: a
	// workload identity can resolve with zero samples because it is
	// legitimately new, which is not the same thing as a genuine Prometheus
	// fetch failure. Collapsing the two would recreate exactly the ambiguity
	// recommender.BatchStats was introduced to eliminate -- see its doc
	// comment.
	policyBatchRequested = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_policy_batch_requested_count",
		Help: "Number of workload identities requested in a policy's sharded Prometheus batch prefetch this reconcile cycle.",
	}, []string{"policy"})

	policyBatchResolved = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_policy_batch_resolved_count",
		Help: "Number of requested workload identities that resolved with at least one CPU or memory sample this reconcile cycle. A workload can legitimately resolve to zero samples (too young, quiet container) without that being a failure -- see k8s_sustain_policy_batch_failures_total for the Prometheus-health signal, which is a distinct metric on purpose.",
	}, []string{"policy"})

	// policyBatchFailuresTotal is the "Prometheus is unwell" signal: it only
	// grows when a batch shard query AND its per-workload fallback both
	// genuinely failed for an identity (recommender.BatchStats.Failures).
	// It must never be derived from policyBatchRequested/policyBatchResolved
	// -- a low resolved count on its own says nothing about failures, only
	// this counter does.
	policyBatchFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_policy_batch_failures_total",
		Help: "Cumulative count of workload identities whose batch Prometheus fetch (shard query and its per-workload fallback) genuinely failed, per policy. Distinct from batch_requested/batch_resolved: a workload resolving with zero samples because it has no history yet is never counted here.",
	}, []string{"policy"})

	wlrRefreshTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8s_sustain_wlr_refresh_total",
			Help: "WorkloadRecommendation refresh outcomes for DEPARTED identities only -- those with a " +
				"WorkloadRecommendation but no live workload object in the current listing -- by namespace, " +
				"owner kind and outcome (computed, nodata, retained-empty, no-snapshot, error). A live " +
				"workload never increments this; see k8s_sustain_policy_batch_* for that population.",
		},
		[]string{"namespace", "owner_kind", "outcome"},
	)

	groupAutoscalerMismatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_sustain_group_autoscaler_mismatch_total",
		Help: "Owner-name groups whose members disagree on autoscaler presence or kind, counted once per " +
			"reconcile per identity. The group's shared recommendation is still shaped by the first sorted " +
			"member's autoscaler (see groupAutoscalerInfo) -- this counter exists to surface the resulting " +
			"misconfiguration, e.g. an un-autoscaled member silently inheriting an HPA-coordinated value it " +
			"was never meant to get.",
	}, []string{"namespace", "owner_kind", "owner_name"})
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
		recycleSuppressedTotal,
		policyBatchRequested,
		policyBatchResolved,
		policyBatchFailuresTotal,
		wlrRefreshTotal,
		groupAutoscalerMismatchTotal,
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

// EmitRecycleSuppressed increments the counter for a resource decrease that the
// downsize threshold held back on a workload.
func EmitRecycleSuppressed(namespace, ownerKind, ownerName, resource string) {
	recycleSuppressedTotal.WithLabelValues(namespace, ownerKind, ownerName, resource).Inc()
}

// WorkloadRecommendation refresh outcomes for EmitWLRRefresh.
const (
	// WLRRefreshComputed: fresh values were written.
	WLRRefreshComputed = "computed"
	// WLRRefreshNoData: the identity has never produced a recommendation and
	// still has no usable samples. Not terminal — retried next cycle.
	WLRRefreshNoData = "nodata"
	// WLRRefreshRetainedEmpty: the identity HAS a recommendation but produced
	// nothing this cycle, so the previous values were deliberately left in
	// place. This is the value worth alerting on: it is the moment a served
	// recommendation stops having live data behind it, and without a counter
	// it is completely silent — the webhook keeps injecting, the WLR keeps
	// looking populated, and nothing in the object records that its samples
	// aged out of the query window.
	WLRRefreshRetainedEmpty = "retained-empty"
	// WLRRefreshError: the computation failed (Prometheus unreachable, write
	// rejected). Distinct from nodata, which is a healthy Prometheus with
	// nothing to say.
	WLRRefreshError = "error"
	// WLRRefreshNoSnapshot: the identity's WLR carries no
	// status.observedResources, so there is no container list to compute
	// against and the refresh is skipped before it starts. Distinct from
	// nodata, which is about Prometheus having nothing to say — this one is
	// about k8s-sustain not knowing what to ask about.
	//
	// It exists because this branch used to return silently, which is how a
	// read-after-write bug that stranded every genuinely new identity in
	// exactly this state shipped unnoticed. Transient on a healthy cluster (a
	// discovery or webhook write is one cycle behind); sustained for the same
	// identity means the snapshot write is failing.
	WLRRefreshNoSnapshot = "no-snapshot"
)

// EmitWLRRefresh records the outcome of one WorkloadRecommendation refresh.
func EmitWLRRefresh(namespace, ownerKind, outcome string) {
	wlrRefreshTotal.WithLabelValues(namespace, ownerKind, outcome).Inc()
}

// EmitGroupAutoscalerMismatch increments the counter for an owner-name group
// whose members disagree on autoscaler presence or kind this reconcile.
func EmitGroupAutoscalerMismatch(namespace, ownerKind, ownerName string) {
	groupAutoscalerMismatchTotal.WithLabelValues(namespace, ownerKind, ownerName).Inc()
}
