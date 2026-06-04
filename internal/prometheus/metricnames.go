package prometheus

// Recording-rule and raw-metric names produced by the controller / webhook
// and exposed through the Prometheus recording rules defined in
// charts/k8s-sustain/values.yaml under `prometheusRule.groups`.
// Centralising them lets the dashboard's query helpers stay typo-safe and
// gives IDEs a single grep target whenever a rule name changes.
//
// Two namespaces live here:
//   - lower_snake_case names declared as raw operator metrics
//     (k8s_sustain_*) — emitted directly by the Go code.
//   - colon-separated recording-rule names (k8s_sustain:*) — defined in
//     values.yaml's `prometheusRule.groups`; the Go code only queries them.
//
// Adding a new metric? Update both this file and the `prometheusRule.groups`
// list in values.yaml.
const (
	// --- Cluster aggregates (recording rules) -----------------------------

	MetricClusterCPUSavingsCores         = "k8s_sustain:cluster_cpu_savings_cores"
	MetricClusterCPUSavingsRatio         = "k8s_sustain:cluster_cpu_savings_ratio"
	MetricClusterMemorySavingsBytes      = "k8s_sustain:cluster_memory_savings_bytes"
	MetricClusterMemorySavingsRatio      = "k8s_sustain:cluster_memory_savings_ratio"
	MetricClusterCPUHeadroomBreakdown    = "k8s_sustain:cluster_cpu_headroom_breakdown"
	MetricClusterMemoryHeadroomBreakdown = "k8s_sustain:cluster_memory_headroom_breakdown"

	// --- Policy aggregates ------------------------------------------------

	MetricPolicyCPUSavingsCores    = "k8s_sustain:policy_cpu_savings_cores"
	MetricPolicyMemorySavingsBytes = "k8s_sustain:policy_memory_savings_bytes"
	MetricPolicyWorkloadCount      = "k8s_sustain_policy_workload_count"
	MetricPolicyAtRiskCount        = "k8s_sustain_policy_at_risk_count"

	// --- Per-workload signals ---------------------------------------------

	MetricWorkloadOOM24h              = "k8s_sustain:workload_oom_24h"
	MetricWorkloadDrifted             = "k8s_sustain:workload_drifted"
	MetricWorkloadDriftRatio          = "k8s_sustain_workload_drift_ratio"
	MetricWorkloadRetryState          = "k8s_sustain_workload_retry_state"
	MetricWorkloadRetryAttempts       = "k8s_sustain_workload_retry_attempts"
	MetricWorkloadTemplateCPUCores    = "k8s_sustain_workload_template_cpu_cores"
	MetricWorkloadTemplateMemoryBytes = "k8s_sustain_workload_template_memory_bytes"
	MetricWorkloadCPUUsageCores       = "k8s_sustain:workload_cpu_usage:cores"
	MetricWorkloadMemoryUsageBytes    = "k8s_sustain:workload_memory_usage:bytes"

	// --- Recommendation-basis recording rules -----------------------------

	MetricContainerCPUUsageByWorkloadRate1m = "k8s_sustain:container_cpu_usage_by_workload:rate1m"
	MetricContainerMemoryByWorkloadBytes    = "k8s_sustain:container_memory_by_workload:bytes"
	MetricWorkloadMaxPodCPUCores            = "k8s_sustain:workload_max_pod_cpu:cores"
	MetricWorkloadMaxPodMemoryBytes         = "k8s_sustain:workload_max_pod_memory:bytes"
	MetricContainerPeakMemory24hBytes       = "k8s_sustain:container_peak_memory_24h:bytes"
	MetricContainerOOMLimit24hBytes         = "k8s_sustain:container_oom_limit_24h:bytes"
	MetricPodWorkload                       = "k8s_sustain:pod_workload"

	MetricContainerCPURequestsByWorkloadCores    = "k8s_sustain:container_cpu_requests_by_workload:cores"
	MetricContainerMemoryRequestsByWorkloadBytes = "k8s_sustain:container_memory_requests_by_workload:bytes"
	MetricContainerCPULimitsByWorkloadCores      = "k8s_sustain:container_cpu_limits_by_workload:cores"
	MetricContainerMemoryLimitsByWorkloadBytes   = "k8s_sustain:container_memory_limits_by_workload:bytes"

	MetricAutoscalerPresent  = "k8s_sustain_autoscaler_present"
	MetricCoordinationFactor = "k8s_sustain_coordination_factor"
)
