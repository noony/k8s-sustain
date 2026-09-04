package prometheus

// k8s_sustain_* names are emitted directly by the Go code; k8s_sustain:*
// recording rules are defined in charts/k8s-sustain/values.yaml under
// `prometheusRule.groups` — adding one means updating both places.
const (
	MetricClusterCPUSavingsCores         = "k8s_sustain:cluster_cpu_savings_cores"
	MetricClusterCPUSavingsRatio         = "k8s_sustain:cluster_cpu_savings_ratio"
	MetricClusterMemorySavingsBytes      = "k8s_sustain:cluster_memory_savings_bytes"
	MetricClusterMemorySavingsRatio      = "k8s_sustain:cluster_memory_savings_ratio"
	MetricClusterCPUHeadroomBreakdown    = "k8s_sustain:cluster_cpu_headroom_breakdown"
	MetricClusterMemoryHeadroomBreakdown = "k8s_sustain:cluster_memory_headroom_breakdown"

	MetricPolicyCPUSavingsCores    = "k8s_sustain:policy_cpu_savings_cores"
	MetricPolicyMemorySavingsBytes = "k8s_sustain:policy_memory_savings_bytes"
	MetricPolicyWorkloadCount      = "k8s_sustain_policy_workload_count"
	MetricPolicyAtRiskCount        = "k8s_sustain_policy_at_risk_count"

	MetricWorkloadOOM24h              = "k8s_sustain:workload_oom_24h"
	MetricWorkloadDrifted             = "k8s_sustain:workload_drifted"
	MetricWorkloadDriftRatio          = "k8s_sustain_workload_drift_ratio"
	MetricWorkloadRetryState          = "k8s_sustain_workload_retry_state"
	MetricWorkloadRetryAttempts       = "k8s_sustain_workload_retry_attempts"
	MetricWorkloadTemplateCPUCores    = "k8s_sustain_workload_template_cpu_cores"
	MetricWorkloadTemplateMemoryBytes = "k8s_sustain_workload_template_memory_bytes"
	MetricWorkloadCPUUsageCores       = "k8s_sustain:workload_cpu_usage:cores"
	MetricWorkloadMemoryUsageBytes    = "k8s_sustain:workload_memory_usage:bytes"

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
