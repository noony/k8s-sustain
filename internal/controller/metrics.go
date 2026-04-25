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
	}, []string{"namespace", "owner_kind", "owner_name", "container", "policy"})

	recommendedMemoryBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_recommended_memory_bytes",
		Help: "Current memory recommendation in bytes for a workload's container, by policy.",
	}, []string{"namespace", "owner_kind", "owner_name", "container", "policy"})

	workloadDriftRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_workload_drift_ratio",
		Help: "Ratio of recommended request to current request (1.0 = no drift).",
	}, []string{"namespace", "owner_kind", "owner_name", "container", "resource"})

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

	hpaPresent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "k8s_sustain_hpa_present",
		Help: "1 when a HorizontalPodAutoscaler targets the workload, labelled by sustain HPA mode.",
	}, []string{"namespace", "owner_kind", "owner_name", "mode"})
)

func init() {
	metrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDuration,
		workloadPatchTotal,
		recommendedCPUCores,
		recommendedMemoryBytes,
		workloadDriftRatio,
		workloadRetryState,
		workloadRetryAttempts,
		policyWorkloadCount,
		policyAtRiskCount,
		hpaPresent,
	)
}
