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
| `k8s_sustain_recommended_cpu_cores`     | gauge | `namespace`, `kind`, `name`, `container`, `policy` |
| `k8s_sustain_recommended_memory_bytes`  | gauge | `namespace`, `kind`, `name`, `container`, `policy` |

### Drift, retry, HPA

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_workload_drift_ratio`      | gauge   | `namespace`, `kind`, `name`, `container`, `resource` |
| `k8s_sustain_workload_retry_state`      | gauge   | `namespace`, `kind`, `name`, `reason` |
| `k8s_sustain_workload_retry_attempts`   | counter | `namespace`, `kind`, `name` |
| `k8s_sustain_policy_workload_count`     | gauge   | `policy` |
| `k8s_sustain_policy_at_risk_count`      | gauge   | `policy` |
| `k8s_sustain_hpa_present`               | gauge   | `namespace`, `kind`, `name`, `mode` |

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

`k8s_sustain:container_cpu_requests_by_workload:cores`,
`k8s_sustain:container_memory_requests_by_workload:bytes`.

### Savings aggregates (new)

`k8s_sustain:cluster_cpu_savings_cores`, `k8s_sustain:cluster_memory_savings_bytes`,
`k8s_sustain:cluster_cpu_savings_ratio`, `k8s_sustain:cluster_memory_savings_ratio`,
`k8s_sustain:policy_cpu_savings_cores`, `k8s_sustain:policy_memory_savings_bytes`.

### Headroom (new)

`k8s_sustain:cluster_cpu_headroom_breakdown` and
`k8s_sustain:cluster_memory_headroom_breakdown` with label `segment={used,idle,free}`.

### Workload signals (new)

`k8s_sustain:workload_oom_24h`, `k8s_sustain:workload_drifted` (boolean: drift > 10%).
