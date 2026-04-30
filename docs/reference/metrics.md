<!-- Source of truth: Prometheus metrics emitted in internal/ (grep for prometheus.New*) -->

# Metrics & Recording Rules

This page lists every metric exposed by k8s-sustain plus the recording rules
shipped in the Helm chart. Use these to build alerts or custom Grafana boards.

## Metrics emitted by the controller

### Reconcile

| Name | Type | Labels | Meaning |
|------|------|--------|---------|
| `k8s_sustain_reconcile_total` | counter | `policy`, `result` | Total reconciliations per policy and outcome. |
| `k8s_sustain_reconcile_duration_seconds` | histogram | `policy` | Reconcile duration. |
| `k8s_sustain_workload_patch_total` | counter | `kind`, `result` | Workload patches by kind and outcome. |

### Recommendations

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_recommended_cpu_cores`        | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `policy` |
| `k8s_sustain_recommended_memory_bytes`     | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `policy` |
| `k8s_sustain_workload_template_cpu_cores`  | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `policy` |
| `k8s_sustain_workload_template_memory_bytes` | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `policy` |

`k8s_sustain_workload_template_cpu_cores` and `k8s_sustain_workload_template_memory_bytes` record the CPU/memory request from the workload's pod-template spec (the pre-injection value). Stable across webhook injection so savings rules can compare against the template.

### Drift, retry, autoscaler

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_workload_drift_ratio`      | gauge   | `namespace`, `owner_kind`, `owner_name`, `container`, `resource` |
| `k8s_sustain_workload_retry_state`      | gauge   | `namespace`, `owner_kind`, `owner_name`, `reason` |
| `k8s_sustain_workload_retry_attempts`   | counter | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_policy_workload_count`     | gauge   | `policy` |
| `k8s_sustain_policy_at_risk_count`      | gauge   | `policy` |
| `k8s_sustain_autoscaler_present`        | gauge   | `namespace`, `owner_kind`, `owner_name`, `kind` |
| `k8s_sustain_autoscaler_target_configured` | gauge | `namespace`, `owner_kind`, `owner_name`, `kind`, `resource` |
| `k8s_sustain_coordination_factor`       | gauge   | `namespace`, `owner_kind`, `owner_name`, `resource`, `kind` |

#### `k8s_sustain_autoscaler_target_configured`

Configured autoscaler `averageUtilization` (%) for a workload's resource trigger.
`kind` is `HPA` or `KEDA`; `resource` is `cpu` or `memory`.

#### `k8s_sustain_coordination_factor`

Multiplier applied by autoscaler coordination to the per-pod request.
`resource` is `cpu` or `memory`; `kind` is `overhead` (the always-on
`(100 / hpa_target_pct) × 1.10` adjustment) or `replica` (the optional
CPU-only replica-budget correction). The value is `1.0` when no effect
was applied. See [Autoscaler Coordination](../concepts/autoscaler-coordination.md)
for the formulas.

### Dashboard server

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_dashboard_request_duration_seconds`         | histogram | `path`, `status` |
| `k8s_sustain_dashboard_prometheus_query_duration_seconds`| histogram | `rule` |

## Recording rules

All rules are evaluated every minute. They live in
`charts/k8s-sustain/templates/prometheusrules.yaml`.

### Workload mapping (existing)

`k8s_sustain:pod_workload` resolves Pod → owner workload via kube-state-metrics.

### Usage rates (existing)

`k8s_sustain:container_cpu_usage:rate5m`, `k8s_sustain:container_cpu_usage_by_workload:rate5m`,
`k8s_sustain:container_memory_working_set:bytes`, `k8s_sustain:container_memory_by_workload:bytes`,
`k8s_sustain:pod_cpu_usage:rate5m`, `k8s_sustain:pod_memory_working_set:bytes`.

### Resource requests (existing)

Per-workload (max across replicas — used for per-workload dashboard views):
`k8s_sustain:container_cpu_requests_by_workload:cores`,
`k8s_sustain:container_memory_requests_by_workload:bytes`.

Per-pod-container (one series per pod, used by cluster-total aggregations):
`k8s_sustain:pod_container_cpu_request:cores`,
`k8s_sustain:pod_container_memory_request:bytes`.

### Savings aggregates (new)

`k8s_sustain:cluster_cpu_savings_cores`, `k8s_sustain:cluster_memory_savings_bytes`,
`k8s_sustain:cluster_cpu_savings_ratio`, `k8s_sustain:cluster_memory_savings_ratio`,
`k8s_sustain:policy_cpu_savings_cores`, `k8s_sustain:policy_memory_savings_bytes`.

### Headroom (new)

`k8s_sustain:cluster_cpu_headroom_breakdown` and
`k8s_sustain:cluster_memory_headroom_breakdown` with label `segment={used,idle,free}`.

### Workload signals (new)

`k8s_sustain:workload_oom_24h`, `k8s_sustain:workload_drifted` (boolean: drift > 10%).
