<!-- Source of truth: internal/controller/metrics.go, internal/webhook/metrics.go, internal/webhook/cert.go, internal/dashboard/metrics.go -->

# Metrics

Every metric k8s-sustain exposes. Each component serves its own `/metrics`:

| Component | Endpoint |
|---|---|
| Controller | `:8080/metrics` (`--metrics-bind-address`) |
| Webhook | `/metrics` on the webhook port (HTTPS) |
| Dashboard | `/metrics` on the dashboard HTTP port (`--bind-address`), same port as the UI |

The chart can create a `ServiceMonitor` for each (see [Helm values](helm-values.md#servicemonitor)). The recording rules shipped with the chart are documented in [Recording Rules](recording-rules.md).

## Metrics emitted by the controller

### What `owner_kind` and `owner_name` name

Except for the four recommendation gauges below, `namespace`, `owner_kind` and `owner_name` name the workload **identity**: the `k8s.sustain.io/owner-name` override when the pod template carries one, the group name for [bare pods](../guides/standalone-pods-and-grouping.md), the object's own kind and name otherwise. That is the key the `WorkloadRecommendation`, the recording rules and the dashboard use. Members of one owner-name group never get series of their own: their values aggregate into the identity's single series, as described per metric below.

### Reconcile

| Name | Type | Labels | Meaning |
|------|------|--------|---------|
| `k8s_sustain_reconcile_total` | counter | `policy`, `result` | Total reconciliations per policy and outcome. |
| `k8s_sustain_reconcile_duration_seconds` | histogram | `policy` | Reconcile duration. |

### Recommendations

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_recommended_cpu_cores`        | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |
| `k8s_sustain_recommended_memory_bytes`     | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |
| `k8s_sustain_workload_template_cpu_cores`  | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |
| `k8s_sustain_workload_template_memory_bytes` | gauge | `namespace`, `owner_kind`, `owner_name`, `container`, `container_kind`, `policy` |

`container_kind` is `regular` or `init`, identifying whether the container originated as a regular pod container or an init container (including restartable sidecars). Use it to slice dashboards by container kind.

These four gauges are emitted per member object: `owner_kind` and `owner_name` are the real object's kind and name, even inside an owner-name group.

`k8s_sustain_workload_template_cpu_cores` and `k8s_sustain_workload_template_memory_bytes` record the CPU/memory request from the workload's pod-template spec (the pre-injection value). Stable across webhook injection so savings rules can compare against the template.

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_recommendation_skipped_total` | counter | `namespace`, `owner_kind`, `owner_name`, `reason` |
| `k8s_sustain_oom_floor_applied_total`       | counter | `namespace`, `owner_kind`, `owner_name`, `container` |
| `k8s_sustain_oom_observed_total`            | counter | `namespace`, `owner_kind`, `owner_name`, `container` |
| `k8s_sustain_oom_reaction_latency_seconds`  | histogram | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_oom_cache_entries`             | gauge   | — |

- `recommendation_skipped_total` — recommendations not emitted, by `reason`. `workload_too_young`: the identity is younger than the 10-minute [workload-age gate](../concepts/recommendation-pipeline.md#stages).
- `oom_floor_applied_total` — memory recommendations raised above the percentile by the OOM floor (`max(peak_24h, oom_time_limit × 1.20)` plus headroom) after an OOM in the last 24h.
- `oom_observed_total`, `oom_reaction_latency_seconds`, `oom_cache_entries` — from the [Pod OOM watcher](../concepts/architecture.md#pod-oom-watcher): deduped OOM kills, delay from `TerminatedAt` to the responding recommendation, and current cache size.

### Drift, retry, autoscaler

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_workload_pods`             | gauge   | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_workload_stale_pods`       | gauge   | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_workload_retry_state`      | gauge   | `namespace`, `owner_kind`, `owner_name`, `reason` |
| `k8s_sustain_workload_retry_attempts`   | counter | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_policy_workload_count`     | gauge   | `policy` |
| `k8s_sustain_policy_blocked_count`      | gauge   | `policy` |
| `k8s_sustain_policy_batch_requested_count` | gauge | `policy` |
| `k8s_sustain_policy_batch_resolved_count`  | gauge | `policy` |
| `k8s_sustain_policy_batch_failures_total`  | counter | `policy` |
| `k8s_sustain_autoscaler_present`        | gauge   | `namespace`, `owner_kind`, `owner_name`, `kind` |
| `k8s_sustain_autoscaler_target_configured` | gauge | `namespace`, `owner_kind`, `owner_name`, `kind`, `resource` |
| `k8s_sustain_coordination_factor`       | gauge   | `namespace`, `owner_kind`, `owner_name`, `resource`, `kind` |
| `k8s_sustain_recycle_suppressed_total`  | counter | `namespace`, `owner_kind`, `owner_name`, `resource` |
| `k8s_sustain_wlr_refresh_total`         | counter | `namespace`, `owner_kind`, `outcome` |
| `k8s_sustain_group_autoscaler_mismatch_total` | counter | `namespace`, `owner_kind`, `owner_name` |

- `workload_retry_state` — `1` while any member of the identity is **Blocked**: its last reconcile step failed with a transient error and no step has succeeded since. `reason` is the failed step (`prometheus`, `patch` or `resize`) of the first blocked member in name order; the identity has at most one series, absent when nothing is blocked.
- `workload_retry_attempts` — failed steps summed over the identity's members.
- `policy_workload_count` — live identities the policy matches (an owner-name group counts once).
- `policy_blocked_count` — those identities that are Blocked, i.e. have a `workload_retry_state` series.
- `autoscaler_present`, `autoscaler_target_configured` — the autoscaler the identity's recommendation is shaped against: the first member, in name order, that an HPA or KEDA ScaledObject targets.
- `recycle_suppressed_total` — decreases held back by the policy's [`downsizeThreshold`](policy.md#cpudownsizethreshold-memorydownsizethreshold), once per resource per pod per reconcile, summed over the identity's members. Increases are never counted.
- `group_autoscaler_mismatch_total` — once per reconcile for an [owner-name group](../guides/standalone-pods-and-grouping.md) whose members disagree on autoscaler presence or kind. The group's single recommendation follows the first sorted member's autoscaler, so any non-zero rate is a misconfiguration.

#### Batch prefetch coverage vs. failures

Each reconcile fetches Prometheus data for all of a policy's workloads in one sharded batch (see [Architecture](../concepts/architecture.md)):

- `policy_batch_requested_count` — identities in the batch this cycle.
- `policy_batch_resolved_count` — identities that returned at least one CPU or memory sample.
- `policy_batch_failures_total` — identities whose shard query and per-workload fallback both failed.

`resolved < requested` with flat `failures_total` is normal (young or quiet workloads). Alert on `failures_total`; treat a low resolved/requested ratio as a data-maturity signal.

#### Series lifetime when a Policy is deleted

Every series with a `policy` label is removed by the Policy's `k8s.sustain.io/cleanup` finalizer, together with its `WorkloadRecommendation`s. Recreating a Policy under the same name restarts its counters from zero.

#### Series lifetime when an identity departs

When no Policy targets an identity any more (its last member was deleted, opted out or left the selector), its `workload_pods`, `workload_stale_pods`, `workload_retry_state`, `workload_retry_attempts`, `autoscaler_present`, `autoscaler_target_configured`, `coordination_factor` and `recycle_suppressed_total` series are deleted in the next reconcile, so a departed identity never reports a frozen state.

#### `k8s_sustain_wlr_refresh_total`

Refresh outcomes, once per reconcile, for **departed** identities only — a `WorkloadRecommendation` with no live workload object (a finished Job, a bare-pod group between runs). Live workloads are covered by the batch counters above.

| `outcome` | Meaning |
|---|---|
| `computed` | Fresh values written. |
| `nodata` | No recommendation yet and still no samples. Retried next cycle. |
| `retained-empty` | Has a recommendation but this cycle produced nothing, so the previous values are kept and still served. Sustained for one identity means its data is gone for good — alert on this. |
| `no-snapshot` | The object has no `status.observedResources`, so there is nothing to compute. Sustained means the snapshot write is failing. |
| `error` | Computation or write failed. |

#### `k8s_sustain_workload_pods`

Live pods (not terminating, not `Succeeded`/`Failed`) owned by the identity's members at the last apply pass, summed over the members. A member skipped this cycle (retry backoff, a failed pass) contributes its last measured count. For `OnCreate` workloads it is measured with a dry run. Absent in recommend-only mode and before the first recommendation; removed when the identity departs.

#### `k8s_sustain_workload_stale_pods`

Subset of `workload_pods` whose resources still differ from the recommendation beyond `downsizeThreshold` after the apply pass. Zero means every pod runs the recommendation. Same lifecycle as `workload_pods`; the dashboard shows it as **Drift** (`stale/total`).

#### `k8s_sustain_autoscaler_target_configured`

Configured `averageUtilization` (%) of the autoscaler shaping the identity's recommendation, per resource trigger.
`kind` is `HPA` or `KEDA`; `resource` is `cpu` or `memory`.

#### `k8s_sustain_coordination_factor`

Multiplier applied by autoscaler coordination to the per-pod request.
`kind` is `overhead` (`(100 / hpa_target_pct) × 1.10`) or `replica` (CPU-only
replica-budget correction); `1.0` when nothing applied. See
[Autoscaler Coordination](../concepts/autoscaler-coordination.md).

### Dashboard server

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_dashboard_request_duration_seconds` | histogram | `path`, `status` |
| `k8s_sustain_dashboard_panic_total`              | counter   | `path` |

### Webhook server

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_webhook_request_duration_seconds` | histogram | `path`, `status` |
| `k8s_sustain_webhook_panic_total`              | counter   | `path` (see below) |
| `k8s_sustain_webhook_cert_expiry_seconds`      | gauge     | — |
| `k8s_sustain_webhook_recommendation_source_total` | counter | `source` |

#### `k8s_sustain_webhook_panic_total`

Every panic the webhook recovered, by route pattern (e.g. `POST /mutate`), plus two non-route values, `singleflight/ownerRef` and `singleflight/ownerAnnotations`, for panics in the [owner-resolution caches](annotation.md#the-webhooks-cost-control-does-not-fully-bound-itself). Each is contained: that admission fails open. Any non-zero rate is a bug.

#### `k8s_sustain_webhook_recommendation_source_total`

The outcome of every admission's `WorkloadRecommendation` read, by `source`. A pod that misses its recommendation starts on its template resources with nothing in its spec to say so; this counter is the only place that is visible. Alert on a rising `stale` or `missing` rate relative to `hit`.

| `source` | Meaning |
|----------|---------|
| `hit` | A fresh recommendation was injected. |
| `retained` | Injected from a kept recommendation of a departed identity (finished Job, bare-pod group between runs). Its age is bounded by `--recommendation-retention` instead of the staleness budget — see [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads). Steady `retained` is normal for recurring workloads; `retained` turning into `missing` means the gap between runs exceeds the retention window. |
| `stale` | `observedAt` is older than the 30-minute staleness budget (or, for a departed identity, older than the retention window): the controller is behind, stuck, or the workload left its policy's scope. |
| `missing` | No recommendation exists yet. The webhook creates a [stub](../concepts/workload-recommendations.md#cold-start-stub-recommendations) so the controller picks it up. Transient for new workloads; sustained for one identity means the controller never computes it. |
| `nodata` | The recommendation exists and was evaluated but has no usable samples (too young, quiet, or unmatched by the recording rules). No stub is created. Takes precedence over `stale`. |
| `error` | The read failed with an apiserver error other than NotFound. |

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

See [Recording Rules](recording-rules.md).
