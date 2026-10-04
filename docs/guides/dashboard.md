# Dashboard

k8s-sustain includes a built-in, read-only web dashboard for exploring policies, viewing workload resource usage, and simulating policy changes before applying them.

## Running the Dashboard

### Standalone (CLI)

```bash
k8s-sustain dashboard \
  --bind-address=:8090 \
  --prometheus-address=http://prometheus:9090
```

The dashboard is then available at `http://localhost:8090`.

At startup, the dashboard pings Prometheus and logs an error if it is unreachable; it still starts, and `/readyz` reports not-ready until Prometheus answers.

!!! note
    The dashboard requires access to:

    - A **Kubernetes cluster** (via kubeconfig or in-cluster config) to list policies and workloads
    - A **Prometheus server** with the k8s-sustain recording rules to query metrics

### CLI Flags

| Flag                      | Default                      | Description                              |
|---------------------------|------------------------------|------------------------------------------|
| `--bind-address`          | `:8090`                      | Address the dashboard server listens on  |
| `--prometheus-address`    | `http://localhost:9090`      | Prometheus server URL                    |
| `--log-level`             | `info`                       | Log level (debug, info, warn, error). The Helm chart sets `error`, which drops the startup and access logs. |
| `--cors-allowed-origins`  | `(empty — same-origin only)` | Allowed CORS origins (comma-separated). Use `*` to allow all (not recommended). |
| `--excluded-namespaces`   | `(empty)`                    | Namespaces the controller/webhook never manage (comma-separated). Mirrors their `--excluded-namespaces` flag so the dashboard's policy-scoped workload views stay consistent with what is actually managed. |

The dashboard also accepts the full set of Prometheus authentication and TLS
flags (`--prometheus-bearer-token-file`, `--prometheus-basic-auth-*`,
`--prometheus-headers`, `--prometheus-tls-*`), with the same names and
semantics as on the controller — see the
[CLI reference](../reference/cli.md#prometheus-authentication-and-tls-flags)
and the [Authenticated Prometheus guide](authenticated-prometheus.md).

!!! danger "Dashboard environment variables carry the `DASHBOARD` prefix"
    The dashboard reads `K8SSUSTAIN_DASHBOARD_*`, not `K8SSUSTAIN_*`; an
    unprefixed variable is silently ignored and every panel reports
    ["No metrics data available"](#no-metrics-data-available). See
    [Environment variables — the prefixes differ per subcommand](authenticated-prometheus.md#environment-variables-the-prefixes-differ-per-subcommand).

When a request carries an `Origin` header and a CORS allowlist is configured,
the dashboard appends `Vary: Origin` to the response so shared caches never
serve one origin's `Access-Control-Allow-Origin` header to another.

### Helm Chart

The dashboard is enabled by default (`dashboard.enabled: true`). All `dashboard.*` values, including `corsAllowedOrigins`, `ingress.*` and `httpRoute.*`, are listed in the [Helm values reference](../reference/helm-values.md#dashboard). If Prometheus needs credentials, the top-level `prometheusAuth` block configures the dashboard and the controller alike, with the correct environment-variable prefix for each.

Access it via port-forward (the Service is named `<fullname>-dashboard`, e.g. `k8s-sustain-dashboard` for a release called `k8s-sustain`):

```bash
kubectl port-forward svc/k8s-sustain-dashboard 8090:8090
```

!!! warning "Authenticate before exposing it"
    The dashboard has **no built-in authentication**. It listens on a `ClusterIP` Service, so it stays cluster-internal until you add an Ingress/Gateway. When you expose it beyond `kubectl port-forward`, never expose it directly — front it with an identity-aware proxy such as **Cloudflare Access**, `oauth2-proxy`, or an authenticating Ingress (OIDC/SSO, mTLS). See [Hardening options](../security.md#hardening-options).

## Using the Dashboard

Every navigation target — table rows, names, policy links, breadcrumbs, the Overview KPI cards and attention queue, **Open in Simulator** — behaves like a regular link: **Cmd/Ctrl-click** or **middle-click** opens it in a new tab, and right-clicking a name offers "Open in new tab".

### Risk state

Every row, badge, KPI and attention list shows one **Risk state** per workload identity (an owner-name group or bare-pod group is one identity), classified once by the backend in this precedence:

1. **Conflicted** — the identity's members opt into different Policies, so no Policy governs it and its recommendation is frozen (see [Conflicted identities](../concepts/workload-recommendations.md#conflicted-identities)). Shown on the Workloads list and detail pages; the Overview does not count it yet.
2. **Blocked** — the controller keeps failing to apply the identity's recommendation and is backing off (any member in retry backoff).
3. **At risk** — an OOM kill in the last 24 hours.
4. **Drift** — pods whose running resources still differ from the recommendation beyond the policy's `downsizeThreshold`; a decrease the policy suppresses is not drift.
5. **Safe** — none of the above.

An identity that is both Blocked and OOM-killed shows as Blocked, everywhere. Blocked, At risk and Drift come from the controller's identity-keyed metrics and the `k8s_sustain:workload_oom_24h` rule (see [Metrics](../reference/metrics.md#what-owner_kind-and-owner_name-name)), so an owner-name group shows its members' combined state under the group's name. Conflicted comes from the identity inventory the dashboard shares with the controller.

### Time range and auto-refresh

Overview, Workload Detail, Policy Detail and Simulator share one **time range picker**.

- **Relative presets** — Past 5 Minutes, 15 Minutes, 30 Minutes, 1 Hour, 4 Hours, 1 Day, 2 Days, 1 Week, 1 Month. They re-anchor to "now" on every load and refresh.
- **Absolute range** — "Select from calendar…" opens a month calendar. Click a start and an end day (in either order; a single day selects that whole day), set the **From**/**To** times, and click **Apply**. Future days are disabled and an end in the future is clamped to now.
- **Timezone** — dates display in the browser's local timezone; API calls use UTC epoch seconds.
- **Shareable URLs** — the range is encoded as `from_ts`/`to_ts` (epoch seconds); relative presets also carry a `window` hint so the URL re-anchors to "now" on reload.
- **Auto-refresh** — every 60 seconds while the tab is visible, paused while it is hidden. Absolute ranges stay fixed.

Charts always span the full selected window: a workload with less history than the range shows its data at the right edge. **Drag horizontally** on a Workload Detail or Simulator chart to zoom; the zoom becomes the active absolute range (URL, picker and every chart update together) and a **Reset zoom** button returns to the previous range.

### Overview Page

The overview is a vertical flow of six bands:

1. **KPI strip** — six cards:
    - **CPU saved** and **Memory saved** — absolute saving, share of cluster requests, and a 7-day sparkline.
    - **Blocked**, **At risk** and **Drifted** — how many identities are in each [Risk state](#risk-state); each identity counts once, under its state. Click one to open the Workloads list filtered to that state.
    - **Coordinated** — workloads whose recommendation is adjusted for an HPA or KEDA ScaledObject (see [Autoscaler coordination](../concepts/autoscaler-coordination.md)).
2. **Savings** — CPU and memory side by side, each plotting three lines over the selected range:
    - **Usage** — measured CPU rate or memory working set, summed across containers in policy-managed workloads.
    - **Current request** — the request on running pods, post-injection.
    - **Original request** — the pod-template request before k8s-sustain rewrote it (`k8s_sustain_workload_template_*`).

    All three are scoped to managed workloads (usage and current-request queries are joined `and on(namespace, owner_kind, owner_name, container) k8s_sustain_workload_template_*`). The gap between *original* and *current request* is the realised saving; the gap between *current request* and *usage* is the remaining headroom.
3. **Cluster headroom** — a stacked bar for CPU and memory split into `used`, `idle` and `free`, from the `k8s_sustain:cluster_cpu_headroom_breakdown` and `..._memory_headroom_breakdown` recording rules.
4. **Needs attention** — three lists, one per non-Safe [Risk state](#risk-state), each identity in exactly one: **Risk** (At risk, most OOM kills first), **Drift** (most stale pods first) and **Blocked** (most retry attempts first), ten rows each. Each row links to the identity's detail page.
5. **Policy effectiveness** — per-policy identity count, CPU/memory saved, and the number of Blocked identities.
6. **Recent activity** — the latest reconcile and pod-recycle events from the controller.

### Workloads Page

Lists every workload identity (Deployments, StatefulSets, DaemonSets, Argo Rollouts, CronJobs, standalone Jobs, and bare Pods carrying `k8s.sustain.io/owner-name`) across the cluster, whether or not a policy governs it. Objects sharing an owner-name are one row, with the union of their containers. Jobs spawned by a CronJob are folded under their CronJob row.

The rows come from the same identity inventory the controller reconciles from (`internal/inventory`), so membership, containers, age, Departed and the governing Policy agree with what the controller does.

- **Stat strip** — Total, Automated (governed by a policy) and Manual counts, plus **Conflicted** when any identity is.
- **Filters** — namespace, kind, status (Automated / Manual), lifecycle (Live / Departed, default any), risk (Safe / Drifted / At risk / Blocked / Conflicted), autoscaler (Has / No autoscaler), and a name search. **Reset filters** clears them all and returns to page 1, keeping the sort order. Filters, sort order and page are kept in the URL query string (e.g. `/workloads?namespace=prod&risk=at-risk&sort=-stalePods`), so browser Back from a workload restores the list and a filtered view can be bookmarked or shared.
- **Columns** — Namespace, Kind, Name, **Risk**, **Drift** (stale/total pods, e.g. `2/5`), **Policy** (links to the governing policy; a Conflicted identity names the policies its members opt into, in red) and container count. Sorted by name by default; click Namespace, Kind, Name, Drift or Policy to sort across all pages, click again to reverse.
- **Risk** — the identity's [Risk state](#risk-state): `Conflicted`, then `Blocked`, then `At risk`, then `Drift`, otherwise `Safe`.
- **Name badges** — **Autoscaler** when an HPA or KEDA ScaledObject targets the workload; **Coordinated** when autoscaler coordination adjusts its recommendation, followed by the non-trivial factors (`×1.15 CPU`, `×1.10 mem` overhead, `· replica ×0.80`); **Departed · last seen X ago** for an identity with no live member.

Departed rows come from a retained `WorkloadRecommendation` whose identity has no live member left (completed bare pods, a deleted or finished standalone Job). They stay listed for the retention window (`--recommendation-retention`, default `168h`); see [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads).

### Workload Detail

- **Header** — kind, namespace, container count, **Automated** + policy link, **Conflicted** with the policies its members opt into, or **Manual**; **Departed** when no member is live; the [Risk state](#risk-state) badge, and the **Coordinated** badge with its factors when autoscaler coordination applies.
- **Status** — three cards: **Mode** (`OnCreate` / `Ongoing`), **Drift** (stale/total pods), **OOM 24h**.
- **Currently blocked** — shown only while a member of the identity is in retry backoff: the failed step (`prometheus`, `patch` or `resize`) and the attempt count across members.
- **Recommendation** — the Recommendation stored in the identity's `WorkloadRecommendation`, current vs. recommended CPU and memory request per container (init containers flagged), with the outcome of the controller's last pass (`Computed`, `No data`, `Too young`, `Fetch failed`, `Conflicted (frozen)`) and when it was last computed. Nothing is recomputed here; use the Simulator for that.
- **Charts** — per container, CPU and memory usage with the workload's **historical request** (amber dashed, stepped) and **limit** (amber dotted), plus the **stored recommendation** (green long-dashed line). Without historical request data in Prometheus, the request line falls back to the current spec. Lines break across gaps longer than ~1.5× the query step (between CronJob runs, scaled to zero). Memory charts show **OOM kills** as red markers with a count, from kube-state-metrics; without kube-state-metrics the markers are omitted.
- **Recent events** — k8s-sustain events about every member of the identity (the controller attributes each event to the member it acted on, so an owner-name group's or bare-pod group's events sit on several objects).
- **Open in Simulator** — jumps to the simulator with the workload pre-filled.

A **Departed** identity's detail page resolves from the retained `WorkloadRecommendation`, so the recommendation and the usage history for the time it ran remain visible.

### Policies Page

A stat strip shows **Total policies**, **Workloads covered**, **Cluster CPU saved** and **Cluster Mem saved**. The table lists each policy's Name, **Status** (Ready condition), **Mode** (per-kind update modes), **Workloads**, **CPU saved**, **Mem saved**, **Blocked** (identities in retry backoff) and **Last applied**. Click a row for the policy detail page.

### Policy Detail

- **Stat strip** — Status, Matched Workloads (every identity the policy governs, Departed ones included, unaffected by the table filters), CPU saved, Memory saved.
- **Configuration** — per resource: window, percentile, headroom, min, max, keep request, and limits strategy. Below: update mode per kind (Deploy, STS, DS, CJ, Job, Rollout), ignore safe-to-evict annotations, exclude init containers, and autoscaler coordination (with `replicaBudgetAnchor` when set). **View as YAML** opens the Policy spec in a modal.
- **Selector** — target namespaces (or "all namespaces"), `matchLabels` and `matchExpressions`.
- **Effectiveness over time** — CPU and memory savings for this policy over the selected range.
- **Matched Workloads** — the identities the policy governs: Namespace, Kind, Name (with Departed badge), Risk, Drift, containers and their current CPU/memory requests; namespace filter, name search, **Reset filters**, and pagination (50 per page). Sorted by name by default; click Namespace, Kind, Name or Drift to sort across all pages. Filters, sort and page live in the URL alongside the time range, so Back from a workload restores the list.
- **Simulate All** — runs the recommender with this policy's configuration over every matched workload (`GET /api/policies/{name}/batch-simulate`) and shows a **Batch Simulation Results** card: aggregate CPU and memory savings (current → recommended) and a per-container table. "Current" is the container's measured usage from Prometheus, not its configured request. A workload that fails to compute shows its error on its own row.

### Policy Simulator

1. Select a **workload** (namespace, kind, name). Kinds: Deployment, StatefulSet, DaemonSet, CronJob, Job, Rollout, Pod.
2. Optionally **Load from policy** to pre-fill every field from an existing policy.
3. Adjust **CPU and memory** independently:
    - Window — the recommendation lookback (e.g. `168h`), independent of the chart time range
    - Percentile (50–100)
    - Headroom (0–100%)
    - Min/Max allowed
    - **Limits strategy** — `keepLimit` (default), `noLimit`, `equalsToRequest`, `requestsLimitsRatio` (with a multiplier), or `keepLimitRequestRatio`. Mirrors `spec.rightSizing.resourcesConfigs.<resource>.limits`.

The simulation re-runs automatically (debounced) whenever a parameter changes. Results show:

- A **Savings impact** band — projected CPU and memory change.
- Per container: CPU/memory request and limit, current vs. recommended (`— removed —` under `noLimit`).
- Charts with the sliding-window recommendation, historical request and current limit over usage.

The simulator, the batch simulation and the controller share one algorithm (`internal/recommender`): percentile over the busiest replica, OOM-aware memory floor, limits derived from the real containers, and **autoscaler coordination** when an HPA or KEDA ScaledObject targets the workload. A Simulation is the only place the dashboard computes a recommendation; what is applied is what the controller stored. The simulator inherits the coordination setting of the workload's governing policy; the `autoscalerCoordination` field of the `POST /api/simulate` body overrides it. It dates the identity as the controller does (its earliest member or its `WorkloadRecommendation`, whichever is older) and reports a Too young identity as `tooYoung` instead of hiding the number; it skips only the live OOM watcher.

When a container OOM'd in the last 24h, its simulated memory recommendation is floored at `max(kernel high-water peak, OOM-time cgroup limit × 1.20)`, exactly as the controller applies it (see [Recommendation pipeline](../concepts/recommendation-pipeline.md)). Sibling containers that did not OOM keep their plain percentile.

## Troubleshooting

### "No metrics data available"

This message appears when Prometheus returns no time-series data for the workload. Common causes:

- **Recording rules not loaded** — k8s-sustain requires recording rules (`k8s_sustain:pod_workload`, `k8s_sustain:container_cpu_usage_by_workload:rate1m`, etc.). Verify they exist by querying `k8s_sustain:pod_workload` in Prometheus. The bundled Prometheus subchart embeds them. With an external Prometheus Operator, set `prometheusRule.enabled=true` to deploy them as a `PrometheusRule`. See [Recording rules](../reference/recording-rules.md).
- **Duplicate kube-state-metrics instances** — the recording rules deduplicate with `max by()`, but a hand-written query joining raw kube-state-metrics series can still fail with "many-to-many matching not allowed". Remove the duplicate kube-state-metrics or deduplicate the same way.
- **Dashboard querying Prometheus unauthenticated** — if the controller produces recommendations but every dashboard panel is empty, the dashboard is probably missing its Prometheus credentials. The chart wires both from the same `prometheusAuth` block, so this shows up when credentials were set by hand with the wrong [environment-variable prefix](authenticated-prometheus.md#environment-variables-the-prefixes-differ-per-subcommand).
- **Missing upstream metrics** — the recording rules depend on `kube_pod_owner`, `kube_replicaset_owner`, `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, and `kube_pod_container_resource_requests` (for historical request lines). Ensure kube-state-metrics and cAdvisor metrics are scraped.

## HTTP API

The dashboard backs every UI page with a small JSON API under `/api/`. The same endpoints are useful for ad-hoc scripts and integrations.

### Routes

| Method | Path | Returns |
|---|---|---|
| `GET` | `/api/policies` | Policies with per-policy rollups (`workloadCount`, savings, `blockedCount`) |
| `GET` | `/api/policies/{name}` | One Policy |
| `GET` | `/api/policies/{name}/workloads` | Identities the policy governs, Departed ones included, Conflicted ones never (`namespace`, `search`, `page`, `pageSize`); `sort` = `name` (default), `namespace`, `kind` or `stalePods`, prefix `-` for descending. `total` counts rows after filters, `matched` before |
| `GET` | `/api/policies/{name}/batch-simulate` | Recommendations for every matched workload with aggregate savings |
| `GET` | `/api/workloads` | Every identity in the cluster (filters: `namespace`, `kind`, `automated`, `departed`, `risk`, `autoscaler`, `search`, `page`, `pageSize`); `sort` = `name` (default), `namespace`, `kind`, `stalePods` or `policyName`, prefix `-` for descending. Each row carries `departed`, `policyName`, and `conflictingPolicies` for a Conflicted identity; `counts` adds `conflicted` |
| `GET` | `/api/workloads/{namespace}/{kind}/{name}` | Identity detail: governing policy (or `conflictingPolicies`), `departed`, mode, `riskState`, drift, OOM 24h, blocked, coordination, events of every member, and the stored `recommendation` (`outcome`, `observedAt`, per-container values). `404` for an unknown identity |
| `GET` | `/api/workloads/{namespace}/{kind}/{name}/metrics` | Usage, request, limit and OOM time-series |
| `POST` | `/api/simulate` | What-if recommendation for one workload |
| `GET` | `/api/summary` | Overview snapshot (savings and Risk-state KPIs, headroom, attention, policy rollups) |
| `GET` | `/api/summary/trend` | Overview savings time-series |
| `GET` | `/api/summary/activity` | Recent controller events |
| `GET` | `/healthz` | Liveness: always `200` |
| `GET` | `/readyz` | Readiness: pings Prometheus; `503` with the error when unreachable |
| `GET` | `/metrics` | Prometheus metrics: `k8s_sustain_dashboard_request_duration_seconds`, `k8s_sustain_dashboard_panic_total` |

Any other path serves the SPA.

### Response envelope

Every successful response is wrapped:

```json
{
  "data": { "...": "endpoint-specific payload" },
  "meta": { "requestId": "a1b2c3d4e5f60718293a4b5c" }
}
```

Errors use a parallel shape:

```json
{
  "error": {
    "code": "BAD_REQUEST",
    "message": "invalid pageSize \"-1\": must be 1..200",
    "field": "pageSize",
    "requestId": "a1b2c3d4e5f60718293a4b5c"
  }
}
```

Error `code` values are stable: `BAD_REQUEST`, `NOT_FOUND`, `METHOD_NOT_ALLOWED`, `SERVICE_UNAVAILABLE`, `INTERNAL`. The `field` key is only present on 400 responses that can be attributed to a single request input (page, pageSize, kind, risk, autoscaler, automated, window, step, limit, …).

### Request correlation

Every request gets an `X-Request-Id` header on the response, generated server-side when the client doesn't supply one. The same value is included in `meta.requestId` (success) or `error.requestId` (failure) and emitted into structured logs, so an operator can grep a single request across UI report, backend logs, and Prometheus telemetry.

Clients may forward their own value by sending `X-Request-Id: <id>` on the request — the dashboard echoes it back instead of generating a new one.

### Compression

Responses are gzip-encoded when the request advertises `Accept-Encoding: gzip` (browsers do this automatically; `curl --compressed` opts in). Large endpoints (`/api/workloads/.../metrics`, `/api/summary/trend`, `/api/policies/{name}/batch-simulate`) typically compress 5–10×.

Responses that must not carry a body (`204`, `304`, `1xx`), responses the handler declares empty with `Content-Length: 0`, and already-compressed content types (images, video, audio, archives) bypass compression entirely rather than being advertised as gzip. A response that *is* committed as gzip always carries valid gzip framing even when its body is empty (`GET /healthz`), so a gzip-aware prober never sees a zero-byte payload labelled `Content-Encoding: gzip`.

### Caching

`/api/summary` is the one cached endpoint: its cluster-wide snapshot is held in an in-memory LRU with a 60s TTL, and a successful response carries `Cache-Control: public, max-age=60`. Error responses drop that header so a single failed recompute is never re-served from a browser or proxy cache. Everything else is computed per request.

Three behaviours matter when the cache misses:

- **One recompute at a time.** The snapshot costs ~19 Prometheus queries, so concurrent misses (a dashboard open in several tabs, or a TTL that just lapsed under load) are collapsed onto a single shared computation instead of each firing its own fan-out. Each request still honours its own context deadline: a caller that gives up does not abort the shared work for the others, and a slow computation cannot make a fast one wait indefinitely.
- **A partial Prometheus failure never poisons the cache.** Only a fully successful recompute is stored. When some queries fail, the dashboard prefers the most recent complete snapshot for up to 10 minutes (10x the TTL) so a brief blip is invisible; past that it serves the fresh-but-partial result rather than pretending an arbitrarily old snapshot is current.
- **A cancelled request** that was waiting on a recompute falls back to that same recent snapshot when one exists, and otherwise returns `503 Service Unavailable` instead of an all-zeroes `200`.

### Routing and methods

Routes use Go 1.22 method-specific patterns, so the wrong HTTP verb returns `405 Method Not Allowed` with an `Allow` header listing the supported method(s). Path parameters (`{name}`, `{namespace}`, `{kind}`) are URL-decoded by the standard library.

An `/api/*` path that matches no registered route returns the JSON 404 error envelope (it is never rewritten to the SPA's `index.html`). Endpoints that fetch a single Kubernetes object return 404 only when the object is actually missing; any other API-server failure surfaces as a 500. `/api/summary/trend` returns `503 Service Unavailable` when every Prometheus query fails (a full outage), while partial failures still return the series that succeeded.

### Validation

Query parameters are validated strictly. Unknown enum values (`?risk=foo`, `?autoscaler=maybe`, `?kind=ReplicaSet`, `?sort=containers`) return 400 with the `field` set, instead of silently filtering out every workload. Likewise out-of-range integers (`?page=-1`, `?limit=10000`) and malformed durations (`?window=junk`) get a 400 pointing at the offending input.
