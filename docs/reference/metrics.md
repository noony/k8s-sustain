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

These four gauges are emitted per member object: `owner_kind` and `owner_name` are the real object's kind and name, even inside an owner-name group. Every live member of an identity the policy governs has them, for each container its Recommendation names: the one the policy computed this reconcile or, when it computed none (the fetch failed, Prometheus had no data, every member was in retry backoff), the one its `WorkloadRecommendation` kept from the policy's last computation, so a Prometheus outage does not drop the savings. A Recommendation another policy computed is never published under this one.

`k8s_sustain_workload_template_cpu_cores` and `k8s_sustain_workload_template_memory_bytes` record the CPU/memory request from the workload's pod-template spec (the pre-injection value). Stable across webhook injection so savings rules can compare against the template.

| Name | Type | Labels |
|------|------|--------|
| `k8s_sustain_recommendation_skipped_total` | counter | `namespace`, `owner_kind`, `owner_name`, `reason` |
| `k8s_sustain_oom_floor_applied_total`       | counter | `namespace`, `owner_kind`, `owner_name`, `container` |
| `k8s_sustain_oom_observed_total`            | counter | `namespace`, `owner_kind`, `owner_name`, `container` |
| `k8s_sustain_oom_reaction_latency_seconds`  | histogram | `namespace`, `owner_kind`, `owner_name` |
| `k8s_sustain_oom_cache_entries`             | gauge   | — |

- `recommendation_skipped_total` — recommendations not emitted, by `reason`. `workload_too_young`: the identity is younger than the 10-minute [workload-age gate](../concepts/recommendation-pipeline.md#stages).
- `oom_floor_applied_total` — memory recommendations whose final request the OOM floor (`max(peak_24h, oom_time_limit × 1.20)` plus headroom, after an OOM in the last 24h) determined: it beat the percentile and no min/max clamp, before or after autoscaler coordination, replaced it. Read from the `determined` fact of the `oomFloor` stage in the [trace](../concepts/recommendation-pipeline.md#trace).
- `oom_observed_total`, `oom_reaction_latency_seconds`, `oom_cache_entries` — from the [Pod OOM watcher](../concepts/architecture.md#pod-oom-watcher): deduped OOM kills of pods a Policy governs, delay from `TerminatedAt` to the responding recommendation, and current cache size.

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
| `k8s_sustain_recycle_suppressed_total`  | counter | `namespace`, `owner_kind`, `owner_name`, `resource` |
| `k8s_sustain_wlr_refresh_total`         | counter | `namespace`, `owner_kind`, `outcome` |
| `k8s_sustain_group_autoscaler_mismatch_total` | counter | `namespace`, `owner_kind`, `owner_name` |

- `workload_retry_state` — `1` while any member of the identity is **Blocked**: its last apply failed and none has succeeded since. A transient failure (Prometheus errors and timeouts, API 5xx, 404, 409 and 429) backs off; a permanent one (an API rejection such as 403, 401 or Invalid, or a workload the controller refuses to apply to) is retried on every reconcile and stays Blocked until something outside the controller fixes it. `reason` is the failed step of the first blocked member in name order: `prometheus` (its inputs could not be read), `patch` (evicting or resizing its pods), `resize` (resizing the pods of a kind that is never evicted) or `store` (its recommendation could not be stored for the webhook, so it was not applied). The identity has at most one series, absent when nothing is blocked.
- `workload_retry_attempts` — failed applies summed over the identity's members, transient and permanent alike: a permanent failure counts once per reconcile. It counts every failure, whereas a member's backoff, and the attempt number its `ReconciliationRetryScheduled` event names, grow only with its consecutive transient failures.
- `policy_workload_count` — live identities the policy governs: an owner-name or bare-pod group counts once however many members it has, a Departed identity does not count, and a [Conflicted](../concepts/workload-recommendations.md#conflicted-identities) identity counts for no policy.
- `policy_blocked_count` — those identities that are Blocked, i.e. have a `workload_retry_state` series. The policy is not Ready while it is above zero.
- `autoscaler_present`, `autoscaler_target_configured` — the autoscaler the identity's recommendation is shaped against: the first member, in name order, that an HPA or KEDA ScaledObject targets. A reconcile that does not compute the identity (its fetch failed, or every member is in retry backoff) leaves them as the last computation set them. The coordination factors it led to are not a metric: they are in the `WorkloadRecommendation`'s [trace](../concepts/recommendation-pipeline.md#trace).
- `recycle_suppressed_total` — decreases held back by the policy's [`downsizeThreshold`](policy.md#cpudownsizethreshold-memorydownsizethreshold), once per resource per pod per reconcile, summed over the identity's members. Increases are never counted.
- `group_autoscaler_mismatch_total` — once per reconcile for an [owner-name group](../guides/standalone-pods-and-grouping.md) whose members disagree on autoscaler presence or kind. The group's single recommendation follows the autoscaler of its first member, in name order, that has one, so any non-zero rate is a misconfiguration.

#### Batch fetch coverage vs. failures

Each reconcile fetches Prometheus data for all of a policy's workloads in one batched call (see [How the data is fetched](../concepts/recommendation-pipeline.md#how-the-data-is-fetched)):

- `policy_batch_requested_count` — identities in the batch this cycle. An identity whose every workload is in retry backoff is not fetched.
- `policy_batch_resolved_count` — identities that returned at least one CPU or memory sample.
- `policy_batch_failures_total` — identities whose CPU or memory could not be read, even when re-queried on their own.

`resolved < requested` with flat `failures_total` is normal (young or quiet workloads). Alert on `failures_total`; treat a low resolved/requested ratio as a data-maturity signal.

#### Series lifecycle

Each reconcile of a Policy publishes the series it describes and deletes those its previous reconcile published that it no longer does, so no series outlives what it describes:

- **Series with a `policy` label**: the policy's own (`reconcile_*`, `policy_*`) and the four [recommendation gauges](#recommendations) of its members. A member that is deleted, opts out, leaves the selector, moves to another Policy or becomes part of a [Conflicted](../concepts/workload-recommendations.md#conflicted-identities) identity loses its gauges at the policy's next reconcile, and so does a container removed from a member. A workload moved from one Policy to another is therefore published, and summed by the [savings rules](recording-rules.md#k8s_sustainpolicy_cpu_savings_cores), only by the Policy that governs it once both have reconciled.
- **Identity series**, which carry no `policy` label (`workload_pods`, `workload_stale_pods`, `workload_retry_state`, `workload_retry_attempts`, `autoscaler_present`, `autoscaler_target_configured`, `recommendation_skipped_total`, `oom_floor_applied_total`, `oom_observed_total`, `oom_reaction_latency_seconds`, `recycle_suppressed_total`, `group_autoscaler_mismatch_total`): an identity keeps them while the last reconcile of some Policy included it, live or Departed, so one moving between Policies keeps one set. Its pod and retry gauges describe its live members, so a Departed identity has none; its autoscaler gauges, like a live identity's, are those of its last computation, which for a Departed identity has no autoscaler to coordinate with. The counters and the histogram keep accumulating until no Policy includes the identity any more: its last member opted out or left every selector, it became Conflicted, or it stayed Departed past `--recommendation-retention` and its `WorkloadRecommendation` was swept, after which its series go at the next reconcile. They are then deleted, so a counter restarts from zero if the identity comes back; `rate()` and `increase()` treat that as a counter reset.
- `oom_observed_total` is counted by the OOM watcher as a kill happens, between reconciles. It joins the identity's series under the Policy the watcher found governing the pod, and is deleted with them if the reconcile the kill triggers does not include the identity, or finds that Policy deleted.
- **Deleting a Policy** removes every series with its `policy` label and the series of every identity no other Policy includes: through its `k8s.sustain.io/cleanup` finalizer, together with its `WorkloadRecommendation`s, or, when the finalizer was removed by hand, at the reconcile its deletion triggers. Recreating a Policy under the same name restarts its counters from zero.

`wlr_refresh_total` (labelled by namespace and kind only) and `oom_cache_entries` are never deleted. A restarted controller publishes a Policy's series again at its first reconcile.

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

Every panic the webhook recovered, by route pattern (e.g. `POST /mutate`), plus four non-route values: `singleflight/ownerRef` and `singleflight/ownerAnnotations` for panics in the [owner-resolution caches](annotation.md#the-webhooks-cost-control-does-not-fully-bound-itself), `admit/decide` for a panic while deciding an admission, and `stub/request` for one in the detached stub write. Each is contained: the admission fails open (a panic while deciding still admits the pod with its owner-name label), and a later admission of the identity asks for its stub again. Any non-zero rate is a bug.

#### `k8s_sustain_webhook_recommendation_source_total`

The outcome of every admission's `WorkloadRecommendation` read, by `source`: one label per [read verdict](../concepts/workload-recommendations.md#admission-behaviour), plus `error`. A pod that misses its recommendation starts on its template resources with nothing in its spec to say so; this counter is the only place that is visible. Alert on a rising `stale` or `missing` rate relative to `hit`.

| `source` | Meaning |
|----------|---------|
| `hit` | A fresh recommendation was injected. |
| `retained` | Injected from a kept recommendation of a departed identity (finished Job, bare-pod group between runs), or the frozen one of a [Conflicted](../concepts/workload-recommendations.md#conflicted-identities) identity into a pod of the Policy that computed it. Its age is bounded by `--recommendation-retention` instead of the staleness budget — see [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads). Steady `retained` is normal for recurring workloads; `retained` turning into `missing` means the gap between runs exceeds the retention window. |
| `stale` | `observedAt` is older than the 30-minute staleness budget (or, for a departed or Conflicted identity, older than the retention window): the controller is behind, stuck, or the workload left its policy's scope. |
| `missing` | No recommendation exists yet. The webhook creates a [stub](../concepts/workload-recommendations.md#cold-start-stub-recommendations) so the controller picks it up. Transient for new workloads; sustained for one identity means the controller never computes it. |
| `undecided` | The object exists but the controller has decided nothing for it yet: a stub awaiting its first reconcile, or one the controller's claim just created. No stub is created. Sustained for one identity means its Policy's reconcile is not reaching it. |
| `nodata` | The recommendation exists and has an outcome but holds no values yet (too young, quiet, unmatched by the recording rules, or its fetch failed). No stub is created. Takes precedence over `stale`. |
| `other-policy` | The recommendation was withheld: the object, or the values it holds, belong to a Policy other than the one the pod opts into ([ADR 0003](../adr/0003-a-recommendation-is-served-only-to-its-policy.md)). The other side of a [Conflicted](../concepts/workload-recommendations.md#conflicted-identities) identity, or an identity moving between Policies until the new one computes its own values. Admitted unchanged; no stub is created. |
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
