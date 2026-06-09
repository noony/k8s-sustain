<!-- Source of truth: charts/k8s-sustain/values.yaml under `prometheusRule.groups` -->

# Recording Rules

k8s-sustain ships a set of Prometheus recording rules used to compute right-sizing recommendations and to power the dashboard. The rules are defined once in `charts/k8s-sustain/values.yaml` under `prometheusRule.groups`, which carries a YAML anchor (`&recordingRulesGroups`). Both consumers read the same list: `templates/prometheusrule.yaml` renders it into a `PrometheusRule` resource via `.Values.prometheusRule.groups`, and the bundled Prometheus subchart receives it through `prometheus.serverFiles."recording_rules.yml".groups: *recordingRulesGroups`. No sync step or drift check is needed — edit the rules in one place and both consumers update together. The list is always consumed; the `prometheusRule.enabled` toggle only gates whether the standalone `PrometheusRule` resource is rendered.

## Why recording rules?

Computing percentiles over multi-day windows from raw `container_cpu_usage_seconds_total` / `container_memory_working_set_bytes` is expensive at query time. Pre-aggregating into recording rules at write-time keeps the recommender's queries cheap and consistent.

## Rules at a glance

| Rule | Purpose |
|---|---|
| `pod_workload` | Pod → workload mapping (foundational) |
| `container_cpu_usage:rate1m`, `container_memory_working_set:bytes` | Per-container usage (foundational) |
| `container_cpu_usage_by_workload:rate1m`, `container_memory_by_workload:bytes` | Per-container usage with workload labels (feeds the max-pod rules) |
| `workload_max_pod_cpu:cores`, `workload_max_pod_memory:bytes` | Busiest-replica per-pod usage — the recommender's percentile input |
| `container_*_requests_by_workload:*` | Configured requests (dashboard) |
| `cluster_*_savings_*`, `policy_*_savings_*` | Savings aggregates (dashboard) |
| `cluster_*_headroom_breakdown` | Used/idle/free split (dashboard) |
| `workload_oom_24h`, `workload_drifted` | Risk signals (dashboard) |
| `workload_*_usage:*` | Per-workload usage totals (dashboard trend) |

## Rules

### `k8s_sustain:pod_workload`

```promql
max by (namespace, pod, owner_kind, owner_name) (
  kube_pod_owner{
    owner_kind=~"StatefulSet|DaemonSet|Job",
    owner_is_controller="true"
  }
  unless on(namespace, pod) (
    label_replace(kube_pod_owner{owner_kind="Job", owner_is_controller="true"},
                  "job_name", "$1", "owner_name", "(.*)")
    * on(namespace, job_name) group_left
    max by (namespace, job_name) (
      kube_job_owner{owner_kind="CronJob", owner_is_controller="true"}
    )
  )
)
```

Four rules share this name (direct owners excluding CronJob-owned Jobs; Pod → Job → CronJob via `kube_job_owner`; Deployment via ReplicaSet; Argo Rollouts via ReplicaSet). Maps every pod to its top-level workload. The `unless` clause on the direct-owner rule prevents pods from carrying both `owner_kind=Job` and `owner_kind=CronJob`, which would break downstream `group_left` joins.

### `k8s_sustain:container_cpu_usage:rate1m`

```promql
max by (namespace, pod, container) (
  rate(container_cpu_usage_seconds_total{
    container!="",
    container!="POD",
    image!="",
    node!=""
  }[1m])
)
or
max by (namespace, pod, container) (
  rate(container_cpu_usage_seconds_total{
    container!="",
    container!="POD",
    image!="",
    node!=""
  }[5m])
)
```

Per-container CPU usage rate, no workload labels. Primary 1m window
preserves sub-5m bursts that a longer window would smooth away (matters
for percentile-based rightsizing). Falls back to a 5m window for
short-running pods (CronJobs, Jobs) whose lifetime is too brief to
accumulate ≥2 samples in 1m. `max by (namespace, pod, container)`
deduplicates cAdvisor's occasional double-emission (cgroup v1+v2
hierarchies, scrape transitions) so downstream `*` joins don't break
with many-to-many. `node!=""` drops cAdvisor series that briefly lose
the node label during scrape transitions.

### `k8s_sustain:container_cpu_usage_by_workload:rate1m`

```promql
k8s_sustain:container_cpu_usage:rate1m
* on(namespace, pod) group_left(owner_kind, owner_name)
k8s_sustain:pod_workload
```

Per-container CPU rate enriched with workload labels. Retains the `pod` label, so it is the input to `workload_max_pod_cpu:cores` (busiest-replica collapse) and to the dashboard's per-pod queries.

### `k8s_sustain:container_memory_working_set:bytes`

```promql
max by (namespace, pod, container) (
  container_memory_working_set_bytes{
    container!="",
    container!="POD",
    image!="",
    node!=""
  }
)
```

Per-container memory working set (excludes reclaimable page cache).
The outer `max by (namespace, pod, container)` deduplicates series that
cAdvisor can briefly emit twice for the same container (cgroup v1+v2
hierarchies, scrape transitions); without it, downstream `or` and `*`
joins would inflate values or fail with many-to-many. `node!=""` drops
cAdvisor series that briefly lose the node label.

### `k8s_sustain:container_memory_by_workload:bytes`

```promql
k8s_sustain:container_memory_working_set:bytes
* on(namespace, pod) group_left(owner_kind, owner_name)
k8s_sustain:pod_workload
```

Per-container memory with workload labels. Retains the `pod` label, so it is the input to `workload_max_pod_memory:bytes` (busiest-replica collapse) and to the dashboard's per-pod queries.

### `k8s_sustain:container_cpu_requests_by_workload:cores`

```promql
max by (namespace, container, owner_kind, owner_name) (
  (
    kube_pod_container_resource_requests{resource="cpu", container!="", container!="POD"}
    or
    kube_pod_init_container_resource_requests{resource="cpu", container!="", container!="POD"}
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-container CPU requests with workload labels. Used for current-vs-recommended comparison. Unions regular and init containers — kube-state-metrics reports init/sidecar requests under the separate `kube_pod_init_container_resource_requests` metric, so the `or` is required for the request line to appear for sidecars on the dashboard. Container names are unique across both lists, so one series is produced per container.

### `k8s_sustain:container_memory_requests_by_workload:bytes`

```promql
max by (namespace, container, owner_kind, owner_name) (
  (
    kube_pod_container_resource_requests{resource="memory", container!="", container!="POD"}
    or
    kube_pod_init_container_resource_requests{resource="memory", container!="", container!="POD"}
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-container memory requests with workload labels.

### `k8s_sustain:container_cpu_limits_by_workload:cores`

```promql
max by (namespace, container, owner_kind, owner_name) (
  (
    kube_pod_container_resource_limits{resource="cpu", container!="", container!="POD"}
    or
    kube_pod_init_container_resource_limits{resource="cpu", container!="", container!="POD"}
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-container CPU limits with workload labels. Reflects the per-pod cgroup limit (the value the webhook injects on admission), which can differ from the static `Deployment.spec.template` value the operator deliberately never touches. Unions regular and init containers (see the CPU requests rule above).

### `k8s_sustain:container_memory_limits_by_workload:bytes`

```promql
max by (namespace, container, owner_kind, owner_name) (
  (
    kube_pod_container_resource_limits{resource="memory", container!="", container!="POD"}
    or
    kube_pod_init_container_resource_limits{resource="memory", container!="", container!="POD"}
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

Per-container memory limits with workload labels. Same rationale as the CPU limits rule.

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
label_replace(sum(k8s_sustain:container_cpu_usage:rate1m), "segment", "used", "", "")
or
label_replace(
  sum(kube_pod_container_resource_requests{resource="cpu", container!="", container!="POD"}) - sum(k8s_sustain:container_cpu_usage:rate1m),
  "segment", "idle", "", ""
)
or
label_replace(
  sum(kube_node_status_allocatable{resource="cpu"}) - sum(kube_pod_container_resource_requests{resource="cpu", container!="", container!="POD"}),
  "segment", "free", "", ""
)
```

Splits cluster CPU into `segment` values: `used` (actual usage), `idle` (requested but unused), `free` (allocatable but not requested). Inputs are raw kube-state-metrics / cAdvisor (not the `*_by_workload` rules) so static pods, mirror pods, and bare pods without an owner mapping still count toward usage and idle — otherwise `free` would be overstated by their un-subtracted requests.

### `k8s_sustain:cluster_memory_headroom_breakdown`

```promql
label_replace(sum(k8s_sustain:container_memory_working_set:bytes), "segment", "used", "", "")
or
label_replace(
  sum(kube_pod_container_resource_requests{resource="memory", container!="", container!="POD"}) - sum(k8s_sustain:container_memory_working_set:bytes),
  "segment", "idle", "", ""
)
or
label_replace(
  sum(kube_node_status_allocatable{resource="memory"}) - sum(kube_pod_container_resource_requests{resource="memory", container!="", container!="POD"}),
  "segment", "free", "", ""
)
```

Same `used`/`idle`/`free` split, for memory. Same rationale as CPU: raw inputs so unmapped pods are counted.

### `k8s_sustain:container_oom_limit_24h:bytes`

```promql
max by (namespace, owner_kind, owner_name, container) (
  max by (namespace, pod, container) (
    max_over_time(
      (
        container_spec_memory_limit_bytes{container!="", container!="POD", image!="", node!=""}
        and on(namespace, pod, container)
        (changes(kube_pod_container_status_restarts_total{container!="", container!="POD"}[2m]) > 0)
        and on(namespace, pod, container)
        (kube_pod_container_status_last_terminated_reason{reason="OOMKilled", container!="", container!="POD"} == 1)
      )[24h:1m]
    )
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

The cgroup memory limit observed at the moment a recent OOM event fired, per (workload, container), max over 24h. The recommender uses this as a bump anchor — when a workload OOM'd recently, the memory recommendation floor is `max(peak_working_set_24h, oom_time_limit × 1.20)` — to push the request above the limit the kernel killed at.

The conjunction (`container_spec_memory_limit_bytes AND restart_changed AND last_term=OOMKilled`) ensures the limit is only recorded at moments a NEW OOM event fires, not while the workload sits at the bumped limit afterward. This is the property that prevents the feedback loop the `container_peak_memory_24h:bytes` comment warns about: once the workload fits after a bump, no new OOM events occur and the recorded limit stays at its pre-bump value. After 24h with no further OOMs, the signal drains and the recommender falls back to the percentile.

### `k8s_sustain:workload_oom_24h`

```promql
sum by (namespace, owner_kind, owner_name, container) (
  max by (namespace, pod, container) (
    label_replace(
      max by (namespace, pod, container) (
        increase(kube_pod_container_status_restarts_total{container!="", container!="POD"}[24h])
      )
      * on(namespace, pod, container) group_left()
      max by (namespace, pod, container) (
        kube_pod_container_status_last_terminated_reason{reason="OOMKilled", container!="", container!="POD"}
      ),
      "_src", "restarts", "", ""
    )
    or
    label_replace(
      max by (namespace, pod, container) (
        max_over_time(
          kube_pod_container_status_last_terminated_reason{reason="OOMKilled", container!="", container!="POD"}[24h]
        )
      ),
      "_src", "kill", "", ""
    )
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

OOMKilled events in the last 24h, per (workload, container). The `container` label is kept through the outer sum so the OOM recency signal is per-container: the recommender floors the memory of only the containers that actually OOMed — an innocent sidecar in the same pod keeps its pure percentile recommendation. Workload-level consumers (dashboard risk badge, attention queue, detail-page count, and the young-workload age-gate bypass) re-aggregate at query time with `sum by (namespace, owner_kind, owner_name)`. Two paths combined per (pod, container):

- **`restarts` path** — counts OOMs as restart events (`increase(restarts_total)` filtered by last-terminated-reason). Accurate event count for restartable workloads (Deployment, StatefulSet, DaemonSet, Rollout) where the kubelet restarts the same container in place.
- **`kill` path** — 0/1 indicator that the (pod, container) was OOMKilled at any point in the window. Catches one-shot Job/CronJob pods that fail once with `restartPolicy: Never` (or `backoffLimit: 0`) and never increment `restarts_total`.

Both paths are tagged with a distinct `_src` label so they survive the `or` union; `max by (namespace, pod, container)` then drops `_src` and keeps the larger value. For a Deployment pod that OOMed N times the restart path dominates (N ≥ 1); for a one-shot Job pod the kill path provides the 1 the restart path can't see.

**Caveats:**

- For Job pods, semantics shift from "number of OOM events" to "1 if the pod ever OOMed in the window" (each Job pod OOMs at most once anyway).
- A Job pod that creates → OOMs → gets garbage-collected by `failedJobsHistoryLimit` inside one kube-state-metrics scrape interval (~30s) is invisible to both paths. Realistic production cronjobs run for minutes and aren't affected; very short test pods can slip through.

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
  k8s_sustain:container_cpu_usage_by_workload:rate1m
)
```

Total CPU usage summed across all replicas, per container, per workload. Used by the dashboard trend view, **not** by the recommender (which uses `workload_max_pod_cpu:cores`).

### `k8s_sustain:workload_memory_usage:bytes`

```promql
sum by (namespace, owner_kind, owner_name, container) (
  k8s_sustain:container_memory_by_workload:bytes
)
```

Total memory working set summed across all replicas, per container, per workload. Dashboard trend view only.

### `k8s_sustain:workload_max_pod_cpu:cores`

```promql
max by (namespace, owner_kind, owner_name, container) (
  k8s_sustain:container_cpu_usage_by_workload:rate1m
)
```

Busiest-replica CPU rate, per container, per workload: at each instant this is the hottest live pod. The recommender runs `quantile_over_time(p, …)` over this to get a true per-pod percentile that covers the busiest replica — so it needs no replica division and no separate per-pod floor.

Collapsing across pods **here** (in the recording rule) rather than at query time is deliberate: it keeps the recommender's `[window:1m]` subquery cheap (one series per workload×container instead of one per historical pod) and immune to pod-name churn — a pod that briefly ran hot then died is the `max` for only those instants and drops out afterward, so its partial-lifetime samples can't distort the percentile.

### `k8s_sustain:workload_max_pod_memory:bytes`

```promql
max by (namespace, owner_kind, owner_name, container) (
  k8s_sustain:container_memory_by_workload:bytes
)
```

Busiest-replica memory working set, per container, per workload. Same rationale and percentile usage as `workload_max_pod_cpu:cores` above.

## Customising

The chart exposes the rules via `prometheusRule.groups` in `values.yaml`. Override individual rules to adapt to your environment, but keep names stable — the recommender queries by name.

## See also

- [Metrics](metrics.md) — controller self-metrics (distinct from these recording rules).
