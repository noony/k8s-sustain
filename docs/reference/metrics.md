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
| `k8s_sustain_recommended_cpu_cores`        | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |
| `k8s_sustain_recommended_memory_bytes`     | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |
| `k8s_sustain_workload_template_cpu_cores`  | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |
| `k8s_sustain_workload_template_memory_bytes` | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |

`container_kind` is `regular` or `init`, identifying whether the container originated as a regular pod container or an init container (including restartable sidecars). Use it to slice dashboards by container kind.

`k8s_sustain_workload_template_cpu_cores` and `k8s_sustain_workload_template_memory_bytes` record the CPU/memory request from the workload's pod-template spec (the pre-injection value). Stable across webhook injection so savings rules can compare against the template.

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_recommendation_skipped_total` | counter | `namespace`, `owner_kind`, `owner_name`, `reason` |
| `k8s_sustain_oom_floor_applied_total`       | counter | `namespace`, `owner_kind`, `owner_name`, `container` |
| `k8s_sustain_oom_observed_total`            | counter | `namespace`, `owner_kind`, `owner_name`, `container` |
| `k8s_sustain_oom_reaction_latency_seconds`  | histogram | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_oom_cache_entries`             | gauge   | — |

`k8s_sustain_recommendation_skipped_total` increments when the recommender bypasses a workload without emitting a recommendation. `reason="workload_too_young"` means the identity has been known for less than 10 minutes — the [workload-age gate](../concepts/recommendation-pipeline.md#stages) — so percentile queries would otherwise floor to ~0 and trigger an immediate recycle. The gate is age-based, not sample-count based, and it reads the earlier of the workload object's `CreationTimestamp` and its `WorkloadRecommendation`'s.

`k8s_sustain_oom_floor_applied_total` increments when the OOM-aware floor raises a memory recommendation above the percentile value. This means the workload OOM'd in the last 24h and the recommendation was floored at `max(peak_working_set_24h, oom_time_limit × 1.20)` plus headroom, instead of the (lower) percentile value. The peak anchor relies on cAdvisor observing the high-water mark; the OOM-time-limit anchor is the safety net for cgroup v2 / sub-scrape OOM kills where peak underreports.

`k8s_sustain_oom_observed_total`, `k8s_sustain_oom_reaction_latency_seconds`, and `k8s_sustain_oom_cache_entries` are emitted by the [Pod OOM watcher](../concepts/architecture.md#pod-oom-watcher). `observed_total` counts deduped OOM kills seen on annotated pods. `reaction_latency_seconds` measures the wall-clock delay from the container's `TerminatedAt` to the floor-driven memory recommendation that responds to it — the value the watcher is designed to compress. `cache_entries` reports the current size of the in-memory OOM cache.

### Drift, retry, autoscaler

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_workload_drift_ratio`      | gauge   | `namespace`, `owner_kind`, `owner_name`, `resource` |
| `k8s_sustain_workload_retry_state`      | gauge   | `namespace`, `owner_kind`, `owner_name`, `reason` |
| `k8s_sustain_workload_retry_attempts`   | counter | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_policy_workload_count`     | gauge   | `policy` |
| `k8s_sustain_policy_at_risk_count`      | gauge   | `policy` |
| `k8s_sustain_policy_batch_requested_count` | gauge | `policy` |
| `k8s_sustain_policy_batch_resolved_count`  | gauge | `policy` |
| `k8s_sustain_policy_batch_failures_total`  | counter | `policy` |
| `k8s_sustain_autoscaler_present`        | gauge   | `namespace`, `owner_kind`, `owner_name`, `kind` |
| `k8s_sustain_autoscaler_target_configured` | gauge | `namespace`, `owner_kind`, `owner_name`, `kind`, `resource` |
| `k8s_sustain_coordination_factor`       | gauge   | `namespace`, `owner_kind`, `owner_name`, `resource`, `kind` |
| `k8s_sustain_recycle_suppressed_total`  | counter | `namespace`, `owner_kind`, `owner_name`, `resource` |
| `k8s_sustain_wlr_refresh_total`         | counter | `namespace`, `owner_kind`, `outcome` |
| `k8s_sustain_group_autoscaler_mismatch_total` | counter | `namespace`, `owner_kind`, `owner_name` |

`k8s_sustain_recycle_suppressed_total` counts resource decreases that the controller declined to apply because they fell below the policy's [`downsizeThreshold`](policy.md#cpudownsizethreshold-memorydownsizethreshold). It is incremented once per resource (`cpu`/`memory`) per pod processed in a reconcile, so a sustained non-zero rate means a workload keeps recommending small downsizes that are being held back to avoid churn. Increases are never suppressed and never counted here.

`k8s_sustain_group_autoscaler_mismatch_total` increments once per reconcile for an [owner-name group](../guides/standalone-pods-and-grouping.md) whose members disagree on autoscaler presence or kind — one member HPA-governed, a sibling with none, or governed by a different autoscaler kind entirely. The group still gets exactly one shared recommendation, shaped by the first sorted member's autoscaler (`groupAutoscalerInfo`), so a mismatch is a misconfiguration signal: some member is inheriting a recommendation coordinated against an autoscaler that has no view of it. Any non-zero rate for an identity is worth investigating; it is not expected to fire in steady state.

#### Batch prefetch coverage vs. failures

Each reconcile, the controller fetches Prometheus data for every matched workload in one sharded batch call per policy (see [Architecture](../concepts/architecture.md)) instead of one query set per workload. Three metrics observe that call, and they answer two genuinely different questions on purpose:

- `k8s_sustain_policy_batch_requested_count` — how many workload identities were included in this policy's batch prefetch this cycle.
- `k8s_sustain_policy_batch_resolved_count` — how many of those identities came back with at least one CPU or memory sample.
- `k8s_sustain_policy_batch_failures_total` — how many identities' fetch genuinely failed (the shard query *and* its per-workload fallback both errored, e.g. Prometheus is unreachable).

`batch_resolved_count` can legitimately sit below `batch_requested_count` with `batch_failures_total` never moving at all: a workload younger than its configured window, or one whose containers haven't emitted samples yet, resolves to zero data on a perfectly healthy Prometheus. Only `batch_failures_total` means "something is actually wrong" — treat a sustained increase in it as a Prometheus-health alert, and treat a persistently low `batch_resolved_count`/`batch_requested_count` ratio (with `batch_failures_total` flat) as a data-maturity signal instead. Never derive one from the other.

#### Series lifetime when a Policy is deleted

Every series carrying a `policy` label is removed when that Policy is deleted, as part of the same finalizer that deletes the policy's `WorkloadRecommendation`s. That covers the per-policy metrics above (`policy_workload_count`, `policy_at_risk_count`, the three `policy_batch_*`, `reconcile_total`, `reconcile_duration_seconds`) and the per-workload gauges that carry the producing policy's name (`recommended_cpu_cores`, `workload_template_cpu_cores`, `recommended_memory_bytes`, `workload_template_memory_bytes`).

This matters because a gauge keeps exporting its last value forever otherwise: a deleted policy would be indistinguishable from a live one that happens to match nothing, and cardinality would grow with every policy the controller had ever seen rather than with the number that exist. Deleting a Policy and recreating it under the same name therefore restarts its counters from zero — expected, and worth knowing before alerting on `increase(k8s_sustain_reconcile_total[...])` across a policy rename.

#### `k8s_sustain_wlr_refresh_total`

`WorkloadRecommendation` (WLR) refresh outcomes by `namespace`, `owner_kind` and
`outcome`, emitted once per reconcile cycle for each **departed** identity —
one that still has a WLR but no live workload object in the current listing (a
completed Job, an Airflow bare-pod group between runs). Cycles are driven by
`--reconcile-interval`, but also by a change to the `Policy` and by an
`OOMKilled` container, so the emission rate is at least one per interval.

That population is exactly what this counter exists for: it is invisible to
every other signal, because nothing lists it. Live workloads never increment
this counter — they are observed by the batch-prefetch counters above and by
`k8s_sustain_recommendation_skipped_total`.

- `computed` — fresh values were written this cycle.
- `nodata` — the identity has never produced a recommendation and still has no
  usable samples. Not terminal; retried next cycle.
- `retained-empty` — **the signal worth alerting on.** The identity *has* a
  recommendation, but this cycle produced nothing, so the previous values were
  deliberately left in place rather than overwritten. This is the moment a
  served recommendation stops having live data behind it: the webhook keeps
  injecting it, the WLR object still looks populated, and nothing else records
  that its samples aged out of the query window. A one-off is expected for an
  ephemeral identity between runs; a sustained `retained-empty` rate for the
  same identity means its data source has gone away for good.
- `no-snapshot` — the identity's WLR carries no `status.observedResources`, so
  there is no container list to compute against and the refresh is skipped
  before it starts. Distinct from `nodata`: that one is Prometheus having
  nothing to say, this one is k8s-sustain not knowing what to ask about. A
  transient blip is normal (a discovery or webhook snapshot write is one cycle
  behind); a *sustained* count for the same identity means the snapshot write
  is failing, and that identity is inert — neither computed nor injectable.
- `error` — the computation itself failed (Prometheus unreachable, write
  rejected). Distinct from `nodata`, which is a healthy Prometheus with
  nothing to say for that identity.

#### `k8s_sustain_workload_drift_ratio`

Largest drift ratio (recommended / current) across the workload's containers, per resource. `1.0` means no drift; `> 1.0` means under-provisioned (recommendation higher than current); `< 1.0` means over-provisioned. The controller pre-aggregates with `max(abs(1 - ratio))` across containers at emit time so this gauge stays one series per (workload, resource) — i.e. constant cardinality regardless of how many containers a pod has. The signed ratio is preserved (not the absolute value) so consumers can still distinguish over- from under-provisioning.

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
| `k8s_sustain_dashboard_panic_total`                      | counter   | `path` |

### Webhook server

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_webhook_request_duration_seconds` | histogram | `path`, `status` |
| `k8s_sustain_webhook_panic_total`              | counter   | `path` |
| `k8s_sustain_webhook_cert_expiry_seconds`      | gauge     | — |
| `k8s_sustain_webhook_recommendation_source_total` | counter | `source` |

#### `k8s_sustain_webhook_recommendation_source_total`

The outcome of every admission's `WorkloadRecommendation` read, by `source`:

| `source` | Meaning |
|----------|---------|
| `hit` | A fresh, usable recommendation was read — the healthy outcome. |
| `retained` | A recommendation **was** injected, but from an object the controller is deliberately keeping for a workload identity that has *departed* — a completed Job, a bare-pod group between runs. Also a success; counted separately because the data is last-known-good rather than fresh, so its age is bounded by `--recommendation-retention` rather than by the staleness budget. The webhook enforces that bound itself (its own `--recommendation-retention`, rendered by the chart from the same value as the controller's) rather than trusting the sweep to have deleted the object — a controller wedged before its sweep would otherwise waive freshness for that identity forever. See [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads). |
| `stale` | The object exists but `observedAt` is older than the 30-minute staleness budget. The controller is falling behind, stuck, or the workload left its policy's scope. Not emitted for a departed identity *within* its retention window: such an identity is still recomputed every cycle, but once its samples age out of the query window the recompute deliberately writes nothing and leaves `observedAt` frozen, so it would report `stale` for the whole window and drown this signal — see `retained`. Past that window it does count as `stale`, and means the same thing it always does: the object should already have been swept, so the controller is stuck or behind. |
| `missing` | No recommendation exists for this identity at all — no object, or one the controller has not yet evaluated even once. The webhook also creates a [stub](../concepts/workload-recommendations.md#cold-start-stub-recommendations) on this path, which is what puts the identity into the controller's work-list. A workload the controller *has* evaluated and found no samples for reports `nodata`, not `missing`: the controller records that outcome on the object, so this bucket stays what it claims to be — a genuinely unknown identity — rather than absorbing every quiet workload and firing a stub write for it on every admission. |
| `nodata` | A recommendation object exists and was evaluated, but the identity produced nothing recommendable — the `nodata` state. Recorded for live and departed identities alike: a workload that is running but whose series never appear (a quiet container, an `owner_name` the recording rules do not match, or simply one younger than the 10-minute minimum age) lands here. **No stub is created**: the object already exists and is recomputed on every reconcile interval, so a create could only return `AlreadyExists` and then re-read an object that already has everything the webhook could add. Takes precedence over `stale`: the `nodata` mark stamps `observedAt` once and is deliberately never refreshed, so it is older than the staleness budget within minutes and would otherwise be reported as `stale` for as long as the identity has no history — swamping that alert with healthy traffic on any cluster running recurring short Jobs or bare-pod groups. |
| `error` | The read itself failed (an apiserver error other than NotFound) — points at apiserver/RBAC/cache trouble rather than an unreconciled workload. |

**Why this matters more than it looks.** Since Prometheus left the admission path, a `WorkloadRecommendation` read is the *only* way a pod can be rightsized at creation. When it fails, the pod starts on its template resources and **nothing in the pod's own spec records that fact** — there is no "I would have been rightsized but the pipeline was unhealthy" marker. This counter is the only place that is visible. Alert on a rising `stale`-or-`missing` rate relative to `hit`.

A transient `missing` for a brand-new workload is normal — it lasts until the controller's next reconcile evaluates the identity for the first time, after which the identity reports `hit` or `nodata`. A *sustained* `missing` rate for the same identity is not normal: it means the controller is never computing it. Cross-check `k8s_sustain_wlr_refresh_total` for that namespace and owner kind — bearing in mind it only reports cycles in which the identity was **departed**, so a workload that is alive at every reconcile will be absent from it however badly it is failing.

Steady `retained` traffic is the expected shape for recurring ephemeral workloads and needs no action. What *is* worth watching is `retained` counts turning into `missing` for the same identity: that means the gap between its runs has outgrown `--recommendation-retention`, so the object is being reaped in between and each run starts cold again.

The webhook shares the same core HTTP middleware as the dashboard (request-ID correlation via `X-Request-Id`, panic recovery, duration telemetry, body-size limit), all living in `internal/httpx`. The dashboard additionally layers CORS and gzip for its SPA + API; the webhook deliberately omits both. Both servers are constructed through `httpx.NewServer`, which applies a single set of hardened timeout defaults (`ReadHeaderTimeout` 5s, `ReadTimeout`/`WriteTimeout` 15s, `IdleTimeout` 60s) so the two can no longer drift apart.

#### About the `path` label

The `path` label is **not** the raw request URL — it is the matched [Go 1.22
route pattern](https://pkg.go.dev/net/http#ServeMux) (for example
`GET /api/policies/{name}`). Requests that don't match any registered route
(404s, attacker probes against random URLs) are bucketed as `unknown` so an
adversary can't blow up Prometheus label cardinality by inventing paths.
Path parameters (`{name}`, `{namespace}`, etc.) collapse into a single
bucket per route, so a workload with thousands of names still produces one
time series, not thousands.

## Recording rules

All rules are evaluated every minute. They live in
`charts/k8s-sustain/templates/prometheusrule.yaml`.

### Workload mapping (existing)

`k8s_sustain:pod_workload` resolves Pod → owner workload via kube-state-metrics.

### Usage rates (existing)

`k8s_sustain:container_cpu_usage:rate1m`, `k8s_sustain:container_cpu_usage_by_workload:rate1m`,
`k8s_sustain:container_memory_working_set:bytes`, `k8s_sustain:container_memory_by_workload:bytes`,
`k8s_sustain:workload_max_pod_cpu:cores`, `k8s_sustain:workload_max_pod_memory:bytes`.

### Resource requests (existing)

Per-workload (max across replicas — used for per-workload dashboard views):
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

`k8s_sustain:workload_oom_24h` (per-container OOM count, labels include `container`), `k8s_sustain:workload_drifted` (boolean: drift > 10%).
