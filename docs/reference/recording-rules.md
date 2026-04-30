<!-- Source of truth: charts/k8s-sustain/values.yaml and charts/k8s-sustain/templates/prometheusrules.yaml -->

# Recording Rules

k8s-sustain ships a set of Prometheus recording rules used to compute right-sizing recommendations and to power the dashboard. The rules are duplicated in `charts/k8s-sustain/values.yaml` (default values) and `charts/k8s-sustain/templates/prometheusrules.yaml` (PrometheusRule template). Both must stay in sync.

## Why recording rules?

Computing percentiles over multi-day windows from raw `container_cpu_usage_seconds_total` / `container_memory_working_set_bytes` is expensive at query time. Pre-aggregating into recording rules at write-time keeps the recommender's queries cheap and consistent.

## Rules at a glance

| Rule | Purpose |
|---|---|
| `pod_workload` | Pod → workload mapping (foundational) |
| `container_cpu_usage:rate5m`, `container_memory_working_set:bytes` | Per-container usage (foundational) |
| `container_cpu_usage_by_workload:rate5m`, `container_memory_by_workload:bytes` | Per-container usage with workload labels (recommender) |
| `pod_cpu_usage:rate5m`, `pod_memory_working_set:bytes` | Per-pod usage (dashboard headroom) |
| `container_*_requests_by_workload:*`, `pod_container_*_request:*` | Configured requests (recommender + dashboard) |
| `cluster_*_savings_*`, `policy_*_savings_*` | Savings aggregates (dashboard) |
| `cluster_*_headroom_breakdown` | Used/idle/free split (dashboard) |
| `workload_oom_24h`, `workload_drifted` | Risk signals (dashboard) |
| `workload_*_usage:*`, `workload_replicas:count` | Per-workload aggregates (recommender) |

## Rules

### `k8s_sustain:pod_workload`

```promql
max by (namespace, pod, owner_kind, owner_name) (
  kube_pod_owner{
    owner_kind=~"StatefulSet|DaemonSet|Job",
    owner_is_controller="true"
  }
)
```

Three rules share this name (direct owners; Deployment via ReplicaSet; Argo Rollouts via ReplicaSet). Maps every pod to its top-level workload.

### `k8s_sustain:container_cpu_usage:rate5m`

```promql
rate(container_cpu_usage_seconds_total{
  container!="",
  container!="POD",
  image!=""
}[5m])
```

Per-container CPU usage rate, no workload labels.

### `k8s_sustain:container_cpu_usage_by_workload:rate5m`

```promql
k8s_sustain:container_cpu_usage:rate5m
* on(namespace, pod) group_left(owner_kind, owner_name)
k8s_sustain:pod_workload
```

Per-container CPU rate enriched with workload labels. Queried by `internal/prometheus/client.go` for percentile-based CPU requests.

### `k8s_sustain:pod_cpu_usage:rate5m`

```promql
sum by (namespace, pod, owner_kind, owner_name) (
  k8s_sustain:container_cpu_usage:rate5m
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-pod CPU usage (containers summed within the pod), with workload labels.

### `k8s_sustain:container_memory_working_set:bytes`

```promql
container_memory_working_set_bytes{
  container!="",
  container!="POD",
  image!=""
}
```

Per-container memory working set (excludes reclaimable page cache).

### `k8s_sustain:container_memory_by_workload:bytes`

```promql
k8s_sustain:container_memory_working_set:bytes
* on(namespace, pod) group_left(owner_kind, owner_name)
k8s_sustain:pod_workload
```

Per-container memory with workload labels. Queried for percentile-based memory requests.

### `k8s_sustain:pod_memory_working_set:bytes`

```promql
sum by (namespace, pod, owner_kind, owner_name) (
  k8s_sustain:container_memory_working_set:bytes
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-pod memory working set (containers summed), with workload labels.

### `k8s_sustain:container_cpu_requests_by_workload:cores`

```promql
max by (namespace, container, owner_kind, owner_name) (
  kube_pod_container_resource_requests{resource="cpu", container!="", container!="POD"}
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-container CPU requests with workload labels. Used for current-vs-recommended comparison.

### `k8s_sustain:container_memory_requests_by_workload:bytes`

```promql
max by (namespace, container, owner_kind, owner_name) (
  kube_pod_container_resource_requests{resource="memory", container!="", container!="POD"}
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-container memory requests with workload labels.

### `k8s_sustain:pod_container_cpu_request:cores`

```promql
kube_pod_container_resource_requests{resource="cpu", container!="", container!="POD"}
* on(namespace, pod) group_left(owner_kind, owner_name)
k8s_sustain:pod_workload
```

Per-pod-container CPU request (one series per pod-container) with workload labels. Sums give cluster totals.

### `k8s_sustain:pod_container_memory_request:bytes`

```promql
kube_pod_container_resource_requests{resource="memory", container!="", container!="POD"}
* on(namespace, pod) group_left(owner_kind, owner_name)
k8s_sustain:pod_workload
```

Per-pod-container memory request, same shape as the CPU rule above.

### `k8s_sustain:cluster_cpu_savings_cores`

```promql
sum(
  k8s_sustain_workload_template_cpu_cores
  - on(namespace, owner_kind, owner_name, container, policy)
  k8s_sustain_recommended_cpu_cores
)
```

Cluster-total CPU savings: sum of `template_request - recommendation`. Uses `k8s_sustain_workload_template_cpu_cores` (the original pod-template request, stable across webhook injection) so savings reflect the gap from the user's original spec, not the injected pod's already-rightsized value.

### `k8s_sustain:cluster_memory_savings_bytes`

```promql
sum(
  k8s_sustain_workload_template_memory_bytes
  - on(namespace, owner_kind, owner_name, container, policy)
  k8s_sustain_recommended_memory_bytes
)
```

Cluster-total memory savings, same delta as the CPU rule.

### `k8s_sustain:cluster_cpu_savings_ratio`

```promql
k8s_sustain:cluster_cpu_savings_cores
/ on()
sum(k8s_sustain_workload_template_cpu_cores)
```

Ratio of saved CPU cores to total templated CPU cores.

### `k8s_sustain:cluster_memory_savings_ratio`

```promql
k8s_sustain:cluster_memory_savings_bytes
/ on()
sum(k8s_sustain_workload_template_memory_bytes)
```

Ratio of saved memory to total templated memory.

### `k8s_sustain:policy_cpu_savings_cores`

```promql
sum by (policy) (
  k8s_sustain_workload_template_cpu_cores
  - on(namespace, owner_kind, owner_name, container, policy)
  k8s_sustain_recommended_cpu_cores
)
```

Per-policy CPU savings.

### `k8s_sustain:policy_memory_savings_bytes`

```promql
sum by (policy) (
  k8s_sustain_workload_template_memory_bytes
  - on(namespace, owner_kind, owner_name, container, policy)
  k8s_sustain_recommended_memory_bytes
)
```

Per-policy memory savings.

### `k8s_sustain:cluster_cpu_headroom_breakdown`

```promql
label_replace(sum(k8s_sustain:pod_cpu_usage:rate5m), "segment", "used", "", "")
or
label_replace(
  sum(k8s_sustain:pod_container_cpu_request:cores) - sum(k8s_sustain:pod_cpu_usage:rate5m),
  "segment", "idle", "", ""
)
or
label_replace(
  sum(kube_node_status_allocatable{resource="cpu"}) - sum(k8s_sustain:pod_container_cpu_request:cores),
  "segment", "free", "", ""
)
```

Splits cluster CPU into `segment` values: `used` (actual usage), `idle` (requested but unused), `free` (allocatable but not requested).

### `k8s_sustain:cluster_memory_headroom_breakdown`

```promql
label_replace(sum(k8s_sustain:pod_memory_working_set:bytes), "segment", "used", "", "")
or
label_replace(
  sum(k8s_sustain:pod_container_memory_request:bytes) - sum(k8s_sustain:pod_memory_working_set:bytes),
  "segment", "idle", "", ""
)
or
label_replace(
  sum(kube_node_status_allocatable{resource="memory"}) - sum(k8s_sustain:pod_container_memory_request:bytes),
  "segment", "free", "", ""
)
```

Same `used`/`idle`/`free` split, for memory.

### `k8s_sustain:workload_oom_24h`

```promql
sum by (namespace, owner_kind, owner_name) (
  increase(
    kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}[24h]
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

OOMKilled events in the last 24h, aggregated to the workload.

### `k8s_sustain:workload_drifted`

```promql
(
  max by (namespace, owner_kind, owner_name) (
    abs(1 - k8s_sustain_workload_drift_ratio)
  ) > 0.10
) * 1
```

Boolean (0/1) per workload indicating drift > 10% between current spec and recommendation.

### `k8s_sustain:workload_cpu_usage:cores`

```promql
sum by (namespace, owner_kind, owner_name, container) (
  k8s_sustain:container_cpu_usage_by_workload:rate5m
)
```

Total CPU usage across all replicas, per container, per workload.

### `k8s_sustain:workload_memory_usage:bytes`

```promql
sum by (namespace, owner_kind, owner_name, container) (
  k8s_sustain:container_memory_by_workload:bytes
)
```

Total memory working set across all replicas, per container, per workload.

### `k8s_sustain:workload_replicas:count`

```promql
count by (namespace, owner_kind, owner_name) (
  count by (namespace, owner_kind, owner_name, pod) (
    k8s_sustain:container_cpu_usage_by_workload:rate5m
  )
)
```

Replica count, derived from distinct pods reporting metrics. Counted at workload level so multi-container pods don't inflate the count.

## Customising

The chart exposes the rules via `prometheusRule.rules` in `values.yaml`. Override individual rules to adapt to your environment, but keep names stable — the recommender queries by name.

## See also

- [Metrics](metrics.md) — controller self-metrics (distinct from these recording rules).
