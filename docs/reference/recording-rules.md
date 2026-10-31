<!-- Source of truth: charts/k8s-sustain/values.yaml under `prometheusRule.groups` -->

# Recording Rules

k8s-sustain ships a set of Prometheus recording rules used to compute right-sizing recommendations and to power the dashboard. The rules are defined once in `charts/k8s-sustain/values.yaml` under `prometheusRule.groups`, which carries a YAML anchor (`&recordingRulesGroups`). Both consumers read the same list: `templates/prometheusrule.yaml` renders it into a `PrometheusRule` resource via `.Values.prometheusRule.groups`, and the bundled Prometheus subchart receives it through `prometheus.serverFiles."recording_rules.yml".groups: *recordingRulesGroups`. No sync step or drift check is needed — edit the rules in one place and both consumers update together. The list is always consumed; the `prometheusRule.enabled` toggle only gates whether the standalone `PrometheusRule` resource is rendered.

## Why recording rules?

Computing percentiles over multi-day windows from raw `container_cpu_usage_seconds_total` / `container_memory_working_set_bytes` is expensive at query time. Pre-aggregating into recording rules at write-time keeps the recommender's queries cheap and consistent.

## Rules at a glance

| Rule | Purpose |
|---|---|
| `pod_workload`, `pod_owner_kind_internal` | Pod → workload mapping (foundational); the second is a helper for the owner-name override |
| `container_cpu_usage:rate1m`, `container_memory_working_set:bytes` | Per-container usage (foundational) |
| `container_cpu_usage_by_workload:rate1m`, `container_memory_by_workload:bytes` | Per-container usage with workload labels (feeds the max-pod rules) |
| `workload_max_pod_cpu:cores`, `workload_max_pod_memory:bytes` | Busiest-replica per-pod usage — the recommender's percentile input |
| `container_*_requests_by_workload:*`, `container_*_limits_by_workload:*` | Configured requests and limits (dashboard) |
| `cluster_*_savings_*`, `policy_*_savings_*` | Savings aggregates (dashboard) |
| `cluster_*_headroom_breakdown` | Used/idle/free split (dashboard) |
| `container_oom_kills` | OOM kills, each recorded once when first seen: the one definition every OOM rule derives from, and the dashboard's chart markers |
| `container_peak_memory_24h:bytes`, `container_oom_limit_24h:bytes` | OOM floor anchors (recommender) |
| `workload_oom_24h`, `workload_drifted` | Risk signals: the OOM count feeds the recommender's OOM floor and the dashboard's At risk state; `workload_drifted` is for your own alerts and dashboards |
| `workload_*_usage:*` | Per-workload usage totals (dashboard trend) |

## Signal contracts

Each [recommendation signal](../concepts/recommendation-pipeline.md#signals) declares what it needs from Prometheus next to its query, as a `PromContract` (`internal/recommender`): the rules it reads, the labels they aggregate by, the evaluation interval its query cost assumes, and the raw series those rules compute from, through every rule they read in turn.

| Signal | Rules it reads | Aggregated by | Interval | cAdvisor series | kube-state-metrics series |
|---|---|---|---|---|---|
| CPU usage | `workload_max_pod_cpu:cores` | `namespace`, `owner_kind`, `owner_name`, `container` | 1m | `container_cpu_usage_seconds_total` | `kube_pod_owner`, `kube_job_owner`, `kube_replicaset_owner`, `kube_pod_annotations` |
| Memory usage | `workload_max_pod_memory:bytes` | the same | 1m | `container_memory_working_set_bytes` | the same four |
| OOM floor | `workload_oom_24h`, `container_peak_memory_24h:bytes`, `container_oom_limit_24h:bytes` | the same | none assumed | `container_memory_max_usage_bytes`, `container_memory_peak_working_set_bytes`, `container_memory_working_set_bytes`, `container_spec_memory_limit_bytes` | `kube_pod_container_status_restarts_total`, `kube_pod_container_status_last_terminated_reason`, `kube_pod_container_status_terminated_reason`, `kube_pod_restart_policy` and the same four |

- **Labels.** A query reads a whole shard of identities at once and attributes each series by these four labels. A series missing one is dropped; a rule keeping one more, such as `pod`, would hand a container several series, and the usage signals keep only one of them.
- **Interval.** The usage signals read their rule as a plain range vector, so its 1m interval is the percentile's sample resolution and what the shard sample budget counts. The OOM floor reads its rules as an instant vector, one sample per rule, whatever their interval.
- **Series.** The four kube-state-metrics series shared by every signal are what `pod_workload` maps a pod to its identity from; `kube_pod_annotations` carries the owner-name override. The OOM floor's other four are what [`container_oom_kills`](#k8s_sustaincontainer_oom_kills) detects a kill from.

`go test ./hack/signalcontract`, part of `make test`, checks the chart against every contract. Each rule a signal reads must be recorded in `prometheusRule.groups`, be one aggregation `by` exactly the declared labels over its whole expression, and evaluate at the declared interval. It and every rule it reads in turn, such as `container_oom_kills` or `pod_workload`, must be evaluated with expected samples by a promtool fixture in `charts/k8s-sustain/tests/promtool/` (run by `make helm-promtool`). Following the rules they read down to raw series, those series must be exactly the declared ones, and each must be kept by its exporter's keep-list in the bundled Prometheus: the `metric_relabel_configs` of `prometheus.scrapeConfigs.kubernetes-nodes-cadvisor`, or of the `kube-state-metrics` job in `prometheus.extraScrapeConfigs`. A signal that needs a series the chart does not keep yet, such as cAdvisor's CFS throttling counters, fails the test until the keep-list keeps it. Expressions are read textually, without the PromQL parser, so a rule a signal reads has to stay a single `<aggregation> by (…) (…)`.

If you bring your own Prometheus, it must scrape every series in the table.

## Rules

### `k8s_sustain:pod_workload`

```promql
(
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
)
unless on(namespace, pod) kube_pod_annotations{annotation_k8s_sustain_io_owner_name=~"[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?"}
```

Five rules share this name and map each pod to the identity the controller groups it under (`workload.ResolveControllerOwner`, then `workload.ApplyOwnerNameOverride`):

1. Direct owners (StatefulSet, DaemonSet, Job), excluding CronJob-owned Jobs — shown above.
2. Pod → Job → CronJob via `kube_job_owner`.
3. Pod → ReplicaSet → Deployment.
4. Pod → ReplicaSet → Argo Rollout.
5. Owner-name override: pods whose `kube_pod_annotations` carry an `annotation_k8s_sustain_io_owner_name` (the pod's own [`k8s.sustain.io/owner-name`](annotation.md#annotations-and-labels) annotation) that is a valid, non-empty Kubernetes label value. `owner_name` becomes the annotation value; `owner_kind` is the kind of the pod's controller as the controller resolves it (the Deployment or Rollout of its ReplicaSet, the CronJob of its Job, otherwise the controller's own kind, whatever it is), or `Pod` when the pod has no controller.

Rules 1–4 end with `unless on(namespace, pod) kube_pod_annotations{annotation_k8s_sustain_io_owner_name=~"…"}`, so a pod with an override is mapped only by rule 5 and every pod has at most one row. The CronJob exclusion in rule 1 likewise stops a pod carrying both `owner_kind=Job` and `owner_kind=CronJob`.

Rules 1–4 map only the kinds a Policy can manage. A pod whose controller is of another kind (an orphan ReplicaSet, a ReplicationController, a custom controller) has no row without an override: the controller resolves it to an identity of that kind, which no Policy manages and nothing queries. With an override, rule 5 maps it under that kind, as the controller groups it, never under `Pod`, so its usage stays out of a bare-pod identity of the same name.

All five rules select the annotation with the same regex, `[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?`. PromQL anchors a regex matcher at both ends, so it matches exactly the non-empty values Kubernetes accepts as a label value (apimachinery's `IsValidLabelValue`: at most 63 characters, alphanumerics, `-`, `_` and `.`, starting and ending with an alphanumeric), which are the values the controller applies the override for (`workload.ApplyOwnerNameOverride`). A pod whose annotation is empty or invalid keeps its real owner in rules 1–4, as it does in the controller, and a bare pod with one is not mapped at all. `go test ./hack/signalcontract` fails when their regexes differ or stop matching exactly the values the controller accepts, when rule 5 gives a pod without a controller another kind than the controller does, or when the bundled kube-state-metrics stops exporting the annotation. `pod_workload_owner_name_test.yaml` pins the kind rule 5 gives a pod of each controller kind.

Rule 5 needs the kube-state-metrics `metricAnnotationsAllowList: ["pods=[k8s.sustain.io/owner-name]"]` setting (the `--metric-annotations-allowlist` flag, on by default in the bundled subchart); without it, pods using the override are mapped to their real owner, and bare pods not at all.

### `k8s_sustain:pod_owner_kind_internal`

Helper for rule 5: the kind of each pod's controller as `workload.ResolveControllerOwner` resolves it (the CronJob of a CronJob's Job, the Deployment or Rollout of a ReplicaSet, otherwise the controller's own kind), with no owner-name exclusion, so it has a row for every pod with a controller, overridden or not. Its last alternative selects every controller kind: filtering it would make rule 5 map the pods of a kind it leaves out as `Pod`. Not meant to be queried directly.

### `k8s_sustain:container_cpu_usage:rate1m`

```promql
max by (namespace, pod, container) (
  rate(container_cpu_usage_seconds_total{
    container!="",
    container!="POD",
    image!="",
    node!=""
  }[1m:10s])
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
(a `[1m:10s]` subquery) preserves sub-5m bursts that a longer window would smooth away (matters
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

Both gauges exist only for the live members of the identities a Policy governs, under that Policy, so the savings rules count each member once: a member that is deleted, opts out or becomes part of a Conflicted identity drops out at its Policy's next reconcile, and one that moves to another Policy counts under the new one only, once both have reconciled (see [Series lifecycle](metrics.md#series-lifecycle)).

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

Per-policy CPU savings: the members of the identities the policy governs. Carries a static `resource="cpu"` label.

### `k8s_sustain:policy_memory_savings_bytes`

```promql
sum by (policy) (
  k8s_sustain_workload_template_memory_bytes
  - on(namespace, owner_kind, owner_name, container, policy)
  k8s_sustain_recommended_memory_bytes
)
```

Per-policy memory savings. Carries a static `resource="memory"` label.

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

### `k8s_sustain:container_oom_kills`

```promql
(
  (
    (
      max by (namespace, pod, container) (
        kube_pod_container_status_restarts_total{container!="", container!="POD"}
      )
      - on(namespace, pod, container)
      max by (namespace, pod, container) (
        last_over_time(kube_pod_container_status_restarts_total{container!="", container!="POD"}[10m] offset 1m)
      )
      > 0
    )
    and on(namespace, pod, container)
    max by (namespace, pod, container) (
      kube_pod_container_status_last_terminated_reason{reason="OOMKilled", container!="", container!="POD"}
    )
  )
  or
  (
    (
      max by (namespace, pod, container) (
        kube_pod_container_status_terminated_reason{reason="OOMKilled", container!="", container!="POD"}
      )
      unless on(namespace, pod, container)
      max by (namespace, pod, container) (
        max_over_time(kube_pod_container_status_terminated_reason{reason="OOMKilled", container!="", container!="POD"}[24h] offset 1m)
      )
    )
    and on(namespace, pod)
    max by (namespace, pod) (kube_pod_restart_policy{type="Never"})
  )
)
* on(namespace, pod) group_left(owner_kind, owner_name)
k8s_sustain:pod_workload
```

OOM kills per (namespace, pod, container), labelled with the identity's `owner_kind` and `owner_name`, each recorded once, at the evaluation that first sees it. A sample's value is the number of kills first seen at that evaluation; between kills the series has no sample at all, so a container never killed costs nothing. This is the one definition of an OOM kill: [`workload_oom_24h`](#k8s_sustainworkload_oom_24h) counts its samples, [`container_oom_limit_24h:bytes`](#k8s_sustaincontainer_oom_limit_24hbytes) samples the memory limit at them, the dashboard's chart markers read it, and the [Pod OOM watcher](../concepts/architecture.md#pod-oom-watcher) applies the same definition to the pods it sees. A kill is one of:

- **A restart whose last termination is `OOMKilled`.** The kubelet restarts the container in place: a Deployment's, StatefulSet's, DaemonSet's or Rollout's, or a Job's with `restartPolicy: OnFailure`. The restart count is compared with the previous evaluation, one group interval (1m) earlier, so consecutive evaluations count each restart once; `last_over_time` skips the staleness markers a failed scrape writes, so a kube-state-metrics gap of up to 10 minutes delays its restarts instead of losing them. `last_terminated_reason` only gates the restart: kube-state-metrics keeps it at 1 for as long as that kill is the container's last termination, the rest of the pod's life if it never fails again. Under `Always` or `OnFailure` only the restart counts a kill, even when a scrape catches the container's brief terminated state before it.
- **A container that will not restart turning `OOMKilled`.** In a pod whose `restartPolicy` is `Never`, such as a Job's, a CronJob's or a bare pod, the kill stays the container's current termination until the pod is deleted and never becomes a last termination. It counts the first time it is seen, and not while it was already seen in the previous 24h, so a scrape gap does not count it twice.

The rule is the last of the `k8s_sustain.workload_mapping` group: rules of one group run in order at the same instant, so a pod killed before kube-state-metrics first scraped it is already mapped by `pod_workload` when this rule first sees the kill. Each kill carries its identity, so it keeps counting after its pod is deleted. `charts/k8s-sustain/tests/promtool/container_oom_kills_test.yaml` pins each case.

**Caveats:**

- A restart before the container's first scrape, or inside a kube-state-metrics gap longer than 10 minutes, is not counted, and neither is a kill whose pod is created and deleted between two scrapes.
- Two restarts between consecutive evaluations both count when the later one's termination is `OOMKilled`, and neither counts otherwise.
- When `kube_pod_container_status_terminated_reason` first reaches Prometheus, a `Never` pod whose status still holds an old OOM kill counts it once, as though it had just happened. A `Never` pod recreated under the same name within 24h of a kill is not counted again.

### `k8s_sustain:container_peak_memory_24h:bytes`

```promql
max by (namespace, owner_kind, owner_name, container) (
  (
    label_replace(
      max by (namespace, pod, container) (max_over_time(container_memory_max_usage_bytes{container!="", container!="POD", image!="", node!=""}[24h])),
      "_src", "max_usage", "", ""
    )
    or
    label_replace(
      max by (namespace, pod, container) (max_over_time(container_memory_peak_working_set_bytes{container!="", container!="POD", image!="", node!=""}[24h])),
      "_src", "peak_ws", "", ""
    )
    or
    label_replace(
      max by (namespace, pod, container) (max_over_time(container_memory_working_set_bytes{container!="", container!="POD", image!="", node!=""}[24h:1m])),
      "_src", "working_set", "", ""
    )
  )
  * on(namespace, pod) group_left(owner_kind, owner_name)
  k8s_sustain:pod_workload
)
```

True 24h memory peak per (workload, container): the kernel high-water mark (cgroup v1 `max_usage_bytes`, cgroup v2 `peak_working_set_bytes`) or the working-set max, whichever is higher. It catches sub-scrape spikes that working set alone misses. The recommender uses it as the peak anchor of the OOM floor. The cgroup limit is deliberately excluded — see the next rule.

### `k8s_sustain:container_oom_limit_24h:bytes`

```promql
max by (namespace, owner_kind, owner_name, container) (
  max_over_time(
    (
      max by (namespace, pod, container) (
        min_over_time(container_spec_memory_limit_bytes{container!="", container!="POD", image!="", node!=""}[5m])
      )
      * on(namespace, pod, container) group_left(owner_kind, owner_name)
      (k8s_sustain:container_oom_kills > bool 0)
    )[24h:1m]
  )
)
```

The memory limit each OOM kill of a container happened at, per (workload, container), the highest over 24h. The recommender uses it as a bump anchor — when a container OOM'd recently, its memory recommendation floor is `max(peak_working_set_24h, killed_at_limit × 1.20)` — to push the request above the limit the kernel killed at.

The limit is sampled only at the kills [`container_oom_kills`](#k8s_sustaincontainer_oom_kills) records, never while the workload sits at the raised limit afterward. This is what prevents the feedback loop the `container_peak_memory_24h:bytes` rule avoids: once the workload fits after a bump, no new kill samples the raised limit, so the anchor stays at the limit the container was killed at and drains 24h after the last kill, when the recommender falls back to the percentile. A kill carries its identity, so a deleted pod's kill stays in the anchor.

`min_over_time(...[5m])` keeps k8s-sustain's own response out of the anchor. The [Pod OOM watcher](../concepts/architecture.md#pod-oom-watcher) triggers a reconcile within seconds of a kill, which raises the limit, while the kill reaches this rule up to a few minutes later: the restart, the kube-state-metrics scrape, the kill rule's evaluation and this rule's 1m subquery step each add a delay. Sampling the limit at that moment would record the raised limit as though the kernel had killed at it, and `max_over_time` would hold that inflated value for 24h — the recommendation deriving its next anchor from its own previous output. An OOM response only ever *raises* a limit, so the minimum over a window reaching back past the kill is the limit the container was actually killed at. A downsize landing in the window makes the anchor conservative rather than inflated, and the peak anchor covers that direction. `charts/k8s-sustain/tests/promtool/oom_limit_anchor_test.yaml` pins both, and that a genuine kill at the raised limit still raises the anchor.

### `k8s_sustain:workload_oom_24h`

```promql
sum by (namespace, owner_kind, owner_name, container) (
  sum_over_time(k8s_sustain:container_oom_kills[24h])
)
```

OOM kills in the last 24h, per (workload, container): the kills [`container_oom_kills`](#k8s_sustaincontainer_oom_kills) recorded in the window. A kill stops counting 24h after it was recorded, whatever its container does since: a container OOM-killed once, then running clean or restarted again for another reason, counts that kill for 24h and then not at all. The `container` label is kept through the outer sum so the OOM recency signal is per-container: the recommender floors the memory of only the containers that actually OOMed — an innocent sidecar in the same pod keeps its pure percentile recommendation. Workload-level consumers (dashboard risk badge, attention queue, detail-page count, and the young-workload age-gate bypass) re-aggregate at query time with `sum by (namespace, owner_kind, owner_name)`. `charts/k8s-sustain/tests/promtool/workload_oom_24h_test.yaml` pins the count.

### `k8s_sustain:workload_drifted`

```promql
max by (namespace, owner_kind, owner_name) (k8s_sustain_workload_stale_pods > bool 0)
```

Boolean (0/1) per workload identity: 1 when at least one live pod of the identity's members is not yet running the recommendation (`k8s_sustain_workload_stale_pods > 0`). The k8s-sustain dashboard reads `k8s_sustain_workload_stale_pods` directly; this rule is for alerts and external dashboards.

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

Collapsing across pods **here** (in the recording rule) rather than at query time is deliberate: it keeps the recommender's `quantile_over_time(p, …[window])` range-vector scan cheap (one series per workload×container instead of one per historical pod) and immune to pod-name churn — a pod that briefly ran hot then died is the `max` for only those instants and drops out afterward, so its partial-lifetime samples can't distort the percentile.

This rule (and `workload_max_pod_memory:bytes` below) lives in the `k8s_sustain.workload_max_pod` group, which evaluates at `interval: 1m`. The recommender reads it as a plain range vector rather than a `[window:1m]` subquery, so that `interval` **is** the percentile's effective sample resolution — there is no subquery step left to resample it. Raising the group's interval to cut Prometheus load would coarsen every recommendation percentile in the product; it is not a free knob, and the usage signals' [contract](#signal-contracts) fails the build when it changes.

### `k8s_sustain:workload_max_pod_memory:bytes`

```promql
max by (namespace, owner_kind, owner_name, container) (
  k8s_sustain:container_memory_by_workload:bytes
)
```

Busiest-replica memory working set, per container, per workload. Same rationale and percentile usage as `workload_max_pod_cpu:cores` above.

## Customising

The chart exposes the rules via `prometheusRule.groups` in `values.yaml`. Override individual rules to adapt to your environment, but keep names stable — the recommender queries by name — and keep each rule a signal reads to its [contract](#signal-contracts): the same labels, interval and source series.

## See also

- [Metrics](metrics.md) — controller self-metrics (distinct from these recording rules).
