# Dashboard

k8s-sustain includes a built-in web dashboard for exploring policies, viewing workload resource usage, and simulating policy changes before applying them.

## Features

- **Overview Story Flow** — Cluster summary covering savings KPIs, CPU and memory usage-vs-request trends, headroom breakdown, attention queue (at risk / drifted / blocked), policy effectiveness, and recent activity.
- **Workloads** — Cluster-wide list with risk/drift/autoscaler columns, plus filters for namespace, kind, risk state, and autoscaler presence.
- **Workload Detail** — Status snapshot (mode, last recycle, drift, OOM 24h), risk and HPA badges, blocked-state diagnostics, copy-as-YAML, and interactive CPU/memory charts with sliding-window recommendation, historical requests/limits, and OOM markers.
- **Policies** — 4-card stat strip (total policies, active workloads, CPU & memory savings) plus per-policy effectiveness columns.
- **Policy Detail** — Effectiveness time-series, view-as-YAML modal, Datadog-style time range picker, and matched workloads with risk/drift columns.
- **Policy Simulator** — Tweak percentile, headroom, min/max, and limits strategy; supports Argo Rollouts; shows projected savings impact; exports results as YAML, CSV, or Helm `--set` overrides.
- **Health Checks** — The `/healthz` endpoint verifies Prometheus connectivity for reliable readiness probes.
- **Request Logging** — Structured HTTP access logs for debugging and observability.

## Running the Dashboard

### Standalone (CLI)

```bash
k8s-sustain dashboard \
  --bind-address=:8090 \
  --prometheus-address=http://prometheus:9090
```

The dashboard is then available at `http://localhost:8090`.

At startup, the dashboard validates Prometheus connectivity and logs a warning if it is unreachable.

!!! note
    The dashboard requires access to:

    - A **Kubernetes cluster** (via kubeconfig or in-cluster config) to list policies and workloads
    - A **Prometheus server** with the k8s-sustain recording rules to query metrics

### CLI Flags

| Flag                      | Default                      | Description                              |
|---------------------------|------------------------------|------------------------------------------|
| `--bind-address`          | `:8090`                      | Address the dashboard server listens on  |
| `--prometheus-address`    | `http://localhost:9090`      | Prometheus server URL                    |
| `--log-level`             | `info`                       | Log level (debug, info, warn, error)     |
| `--cors-allowed-origins`  | `(empty — same-origin only)` | Allowed CORS origins (comma-separated). Use `*` to allow all (not recommended). |

When a request carries an `Origin` header and a CORS allowlist is configured,
the dashboard appends `Vary: Origin` to the response. This prevents shared
caches and CDNs from serving one origin's `Access-Control-Allow-Origin` header
back to a different origin (which would otherwise break the browser's
same-origin policy when two trusted origins share the same upstream cache).

### Helm Chart

Enable the dashboard in your Helm values:

```yaml
prometheusAddress: http://prometheus.monitoring.svc:9090  # only if using an external Prometheus

dashboard:
  enabled: true
  corsAllowedOrigins:
    - "https://my-domain.example.com"
  service:
    type: ClusterIP
    port: 8090
```

Then access it via port-forward:

```bash
kubectl port-forward svc/<release>-k8s-sustain-dashboard 8090:8090
```

!!! warning "Authenticate before exposing it"
    The dashboard has **no built-in authentication**. It listens on a `ClusterIP` Service, so it stays cluster-internal until you add an Ingress/Gateway. When you expose it beyond `kubectl port-forward`, never expose it directly — front it with an identity-aware proxy such as **Cloudflare Access**, `oauth2-proxy`, or an authenticating Ingress (OIDC/SSO, mTLS). See [Hardening options](../concepts/workload-recommendations.md#hardening-options).

## Using the Dashboard

### Time Range and Auto-refresh

Every view in the dashboard — Overview, Workload Detail, Policy Detail, and Simulator — shares the same **time range picker** in the top-right corner.

**Relative presets** re-anchor to "now" on every load and refresh:
Past 5 Minutes, 15 Minutes, 30 Minutes, 1 Hour, 4 Hours, 1 Day, 2 Days, 1 Week, 1 Month.

**Absolute range** — click "Select from calendar…" to open a month calendar. Click a start day then an end day to select the span (click days in any order — they sort automatically; a single day selects that whole day), navigate months with the ‹ › arrows, and set the **From** and **To** times of day with the time fields below the grid. The current day is highlighted, and future days are disabled (there is no data ahead of now); an end that lands in the future is clamped back to the present. Click **Apply** to commit. Absolute ranges stay fixed regardless of when you load the page.

The picker displays dates in the browser's **local timezone** (for display only; all API calls use UTC epoch seconds).

**Shareable URLs** — the selected range is encoded in the URL as `from_ts`/`to_ts` (epoch seconds). Relative presets also carry a `window` hint so the URL re-anchors to "now" on reload. Copying and sharing a URL reproduces the same view: relative = latest window; absolute = exact frozen range.

**Auto-refresh** — the dashboard refreshes automatically every 60 seconds while the browser tab is visible. It pauses when the tab is backgrounded or hidden, and resumes when you return. There is no manual toggle. Relative ranges re-anchor to "now" on each refresh; absolute ranges stay fixed.

### Overview Page

The overview is organised as a vertical "Story Flow" with six bands, each answering a specific operator question — from "what am I saving?" down to "what just happened?".

1. **KPI strip** — Headline savings cards for CPU (cores) and memory (bytes), each showing the absolute saving, the savings ratio versus current requests, and a sparkline of the last 24h. Two complementary cards count workloads currently **at risk** (drift exceeds the policy threshold) and **drifted** (request differs from the latest recommendation).
2. **Savings** — A single card splits CPU and memory side-by-side, each plotting three lines over the selected time range so you can see the savings story directly:
    - **Usage** — actual measured working set (memory) or CPU rate, summed across containers in policy-managed workloads.
    - **Current request** — the request currently set on running pods, post-injection.
    - **Original request** — the user's pod-template request before k8s-sustain rewrote it (`k8s_sustain_workload_template_*`).

    All three lines are scoped to managed workloads (those covered by a Policy) so they are directly comparable — usage and current-request queries are filtered with `and on(namespace, owner_kind, owner_name, container) k8s_sustain_workload_template_*` so unmanaged pods don't inflate them. The gap between *original* and *current request* is the realised saving; the gap between *current request* and *usage* is the remaining headroom.
3. **Headroom breakdown** — A stacked horizontal bar for CPU and memory split into `used`, `idle`, and `free` segments, sourced from the `k8s_sustain:cluster_cpu_headroom_breakdown` and `..._memory_headroom_breakdown` recording rules.
4. **Attention queue** — Three grouped lists: **At risk** (workloads exceeding the drift threshold), **Drifted** (request out-of-date with respect to the recommendation), and **Blocked** (workloads where the controller is in an exponential-backoff retry state). Each row links to the workload detail page.
5. **Policy effectiveness** — Per-policy rollup with the matched workload count, projected CPU/memory savings, and the count of at-risk workloads, so you can spot policies that need tuning.
6. **Activity feed** — Most recent reconcile and pod-recycle events from the controller, with timestamps and outcomes.

### Workloads Page

Lists every workload (Deployments, StatefulSets, DaemonSets, Argo Rollouts, CronJobs, standalone Jobs) across the cluster, regardless of whether it is governed by a policy. Jobs spawned by a CronJob are folded under their owning CronJob row to avoid double-counting.

- **Filters** — Filter by namespace, kind, **risk state** (healthy, drifted, at risk, blocked), and **autoscaler presence** (with autoscaler / without autoscaler). The free-text name search remains.
- **Columns** — A **Risk** badge summarises the workload's state at a glance, a **Drift %** column shows the gap between current request and recommendation, and an **Autoscaler** column indicates whether the workload is paired with an HPA or KEDA ScaledObject. The previous CPU/Memory request columns have been removed because the workload detail view now displays them in context.
- **Status column** — Still shows whether the workload is **Automated** (has a sustain policy) or **Manual**, with a link to the policy when applicable.

Click any workload to view its detail page.

### Workload Detail

Shows a comprehensive view of a single workload:

- **Status snapshot band** — A row of four KPI cards at the top of the page: **Update mode** (`OnCreate` / `Ongoing`), **Last recycled** (timestamp of the last controller-driven pod recycle), **Drift** (current request vs. recommendation as a percentage), and **OOM (24h)** (count of OOM kills observed in the last 24 hours).
- **Header badges** — A **Risk** badge mirrors the value shown in the Workloads list. When the workload has a paired autoscaler (HPA or KEDA ScaledObject), an **Autoscaler** badge is shown.
- **Blocked card** — Visible only when the controller has a retry record for this workload; surfaces the failure **reason**, the number of **attempts**, the **next retry** time, and the **last error** message. Hidden once retries clear.
- **Recommendations** — If automated, shows the computed CPU and memory recommendations per container.
- **CPU and Memory charts** — Interactive time-series with a sliding-window recommendation line overlaid (for automated workloads). The recommendation evolves over time, showing how it would have been computed at each point using the policy's configured window and parameters, rather than a flat line.
- **Open in Simulator** — Jump to the simulator with the workload pre-filled.

A **time range picker** in the top-right controls how much history to display. It offers relative presets — Past 5 Minutes, 15m, 30m, 1 Hour, 4 Hours, 1 Day, 2 Days, 1 Week, 1 Month — and a "Select from calendar…" option for an absolute From→To range. The picker shows the browser's local timezone (display only). The step resolution adjusts automatically for each range. You can also **drag to zoom** on any chart to focus on a specific time window — click and drag horizontally to select the region of interest. Zooming sets the shared time range to the selected window: the URL, the time range picker, and every chart on the page all update together, and the data is re-fetched at a finer step resolution for the zoomed span. Because the zoom becomes the active (absolute) range, a **Reset zoom** button appears next to each chart's title while zoomed — click it to return to the previous range. Each chart overlays the workload's **historical resource request** (amber dashed stepped line) and **limit** (orange dashed line) so you can see how actual usage compares to configured resources over time. The request line reflects real changes (e.g. from k8s-sustain patching or manual edits) rather than a flat snapshot. If historical request data is not available in Prometheus, the dashboard falls back to a static line from the current workload spec. If the workload is automated, the **recommendation** line (red dashed) is also shown.

Usage, request, limit, and recommendation lines all **break across gaps** where no metric samples were emitted (e.g. between CronJob runs, while a workload is scaled to zero, or after pod deletion). The chart inserts an explicit gap whenever the spacing between consecutive samples exceeds ~1.5× the query step, so a continuous line never implies activity that wasn't there.

Memory charts also display **OOM kill events** as red vertical markers with a count badge in the chart header. These are detected via `kube_pod_container_status_restarts_total` correlated with `kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`. If no kube-state-metrics is available, OOM markers are silently omitted.

When a container has OOM'd in the last 24h, that container's displayed **memory recommendation** is floored at `max(kernel high-water peak, OOM-time cgroup limit × 1.20)` (containers that did not OOM keep their pure percentile line, even when a sibling in the same pod OOMed) (same `OOM floor` step described in [Recommendation pipeline](../concepts/recommendation-pipeline.md)), so what the dashboard's recommendation card and chart line show matches what the controller would actually apply — including the bump anchor that takes over when the kernel peak underreports on cgroup v2.

The dashboard **auto-refreshes every 60 seconds** while the browser tab is visible; it pauses automatically when the tab is backgrounded or hidden. Relative presets re-anchor to "now" on each refresh so they always show the latest window; absolute ranges stay fixed.

### Policies Page

The Policies page leads with a **4-card stat strip** summarising the cluster-wide picture:

- **Total policies** — number of `Policy` resources in the cluster
- **Active workloads** — total workloads currently matched by any policy
- **CPU savings** — aggregated cluster-wide CPU saved (cores)
- **Memory savings** — aggregated cluster-wide memory saved (bytes)

Below the strip, the policy table replaces the previous Ready/Namespace columns with **effectiveness columns**: matched **workload count**, **CPU savings** and **memory savings** per policy, **at-risk** workload count, and **last applied** timestamp. The Ready status indicator is still shown alongside the policy name. Click any row to view the policy detail page.

### Policy Detail

Shows the full configuration for both CPU and memory, plus the matched workloads table.

- **Configuration card** — Per-resource (CPU and memory) cards display **window**, **percentile**, **headroom**, **min**/**max allowed**, **keepRequest** flag, and the active **limits** strategy (`equalsToRequest`, `keepLimit`, `keepLimitRequestRatio`, `noLimit`, or `requestsLimitsRatio`). Underneath, a meta row shows the **update mode** badges for all supported workload kinds (Deploy, STS, DS, CJ, Job, Argo Rollout), the **eviction** policy (`ignoreAutoscalerSafeToEvictAnnotations`), the **excludeInitContainers** flag, and the **autoscaler coordination** state (enabled / `replicaBudgetAnchor`).
- **Selector card** — Lists the policy's `spec.selector` (target namespaces and any `matchLabels` / `matchExpressions`) so you can immediately see which workloads it scopes to.
- **Effectiveness card** — A dedicated band with two time-series charts (CPU and memory) showing how this policy's savings have evolved over the selected time range.
- **Time range picker** — The Datadog-style popover (relative presets: 5m to 1 Month; or an absolute From→To calendar range) drives the Effectiveness charts. The browser's local timezone is shown for display purposes. The selected range is encoded in the URL as `from_ts`/`to_ts` (epoch seconds); relative presets also carry a `window` hint so they re-anchor to "now" on reload. Copying the URL reproduces the same view — relative ranges show the latest window, absolute ranges show the exact frozen range.
- **View as YAML modal** — Renders the entire `Policy` resource (sanitised of managed fields) inside a modal with a copy button — handy for sharing or storing in version control.
- **Matched workloads table** — Each row now shows **Risk** and **Drift %** columns alongside the existing namespace/kind/name and current resource requests, so you can prioritise which workloads to investigate from inside the policy view.
- **Namespace filter** and **pagination** (50 per page) remain unchanged.

Click any workload to view its detail page.

### Policy Simulator

The simulator lets you test "what-if" scenarios:

1. Select a **workload target** (namespace, kind, name). The kind picker covers Deployment, StatefulSet, DaemonSet, CronJob, and standalone Job, plus **Argo Rollout**.
2. Choose a **time range** via the time range picker — relative presets or an absolute From→To calendar range — controls how much history is displayed on the charts.
3. Optionally, use the **Load from policy** dropdown to pre-fill all configuration fields (percentile, headroom, min/max, window, and limits strategy) from an existing policy — useful as a starting point before tweaking values.
4. Adjust **CPU and Memory parameters** independently:
    - Window (1h to 30 days) — the lookback period used to compute the recommendation, matching the Policy CRD structure. This is independent of the chart time range.
    - Percentile (50th to 99th)
    - Headroom percentage (0-100%)
    - Min/Max allowed values
    - **Limits strategy** — pick one of `keepLimit` (default; existing pod limits stay unchanged), `noLimit`, `equalsToRequest`, `requestsLimitsRatio` (with a numeric multiplier), or `keepLimitRequestRatio`. Mirrors `spec.rightSizing.resourcesConfigs.<resource>.limits` on the Policy CRD.

The simulation runs automatically whenever any parameter changes (with a short debounce to avoid excessive queries). There is no manual "Run" button — results update live as you adjust sliders, change windows, or modify min/max values.

The results show:

- Computed recommendation per container (CPU/memory request, and CPU/memory limit when a limits strategy is selected; `— removed —` is rendered when `noLimit` is active)
- A **savings impact band** that summarises the projected CPU and memory delta as both a percentage change and an absolute saving (cores / bytes), so you can immediately see whether the candidate parameters reduce or increase footprint
- Time-series charts with a **sliding-window recommendation line** (red) that shows how the recommendation would have evolved at each point in time, **historical request** (amber stepped), and **current limit** (orange) overlaid on historical usage

#### Exporting Results

After running a simulation, use the export buttons to download recommendations:

- **YAML** — Downloads a Kubernetes resource patch you can apply with `kubectl apply -f`
- **CSV** — Downloads a spreadsheet-compatible file with per-container recommendations
- **Helm export** — Generates a block of `--set` overrides (or values-file fragment) you can copy/paste into a Helm install/upgrade command, mapping the simulated requests/limits onto the workload's container paths

## Development

The dashboard frontend is a Vue 3 + TypeScript SPA built with Vite, located in `internal/dashboard/ui/frontend/`. The compiled output goes to `internal/dashboard/ui/dist/` and is embedded into the Go binary via `go:embed`.

### Local development

```bash
cd internal/dashboard/ui/frontend
npm install
npm run dev    # starts Vite dev server with API proxy to localhost:8090
```

Run the Go dashboard backend separately (`k8s-sustain dashboard --bind-address=:8090`), and access the Vite dev server (default `http://localhost:5173`).

### Building

```bash
make build-ui   # builds the frontend (npm ci + npm run build)
make build      # builds frontend then Go binary
```

The Docker build automatically handles the frontend build in a separate stage.

## Troubleshooting

### "No metrics data available"

This message appears when Prometheus returns no time-series data for the workload. Common causes:

- **Recording rules not loaded** — k8s-sustain requires recording rules (`k8s_sustain:pod_workload`, `k8s_sustain:container_cpu_usage_by_workload:rate1m`, etc.). Verify they exist by querying `k8s_sustain:pod_workload` in Prometheus. If using the bundled Prometheus subchart, they are embedded automatically. If using an external Prometheus with the Prometheus Operator, set `prometheusRule.enabled=true` to deploy the recording rules as a `PrometheusRule` resource.
- **Duplicate kube-state-metrics instances** — If multiple kube-state-metrics are scraped, the workload mapping rules can fail with "many-to-many matching not allowed". Either remove the duplicate kube-state-metrics or upgrade the chart (the recording rules deduplicate series automatically since v0.3).
- **Missing upstream metrics** — The recording rules depend on `kube_pod_owner`, `kube_replicaset_owner`, `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, and `kube_pod_container_resource_requests` (for historical request lines). Ensure kube-state-metrics and cAdvisor metrics are scraped.

## HTTP API

The dashboard backs every UI page with a small JSON API under `/api/`. The same endpoints are useful for ad-hoc scripts and integrations.

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

### Routing and methods

Routes use Go 1.22 method-specific patterns, so the wrong HTTP verb returns `405 Method Not Allowed` with an `Allow` header listing the supported method(s). Path parameters (`{name}`, `{namespace}`, `{kind}`) are URL-decoded by the standard library.

An `/api/*` path that matches no registered route returns the JSON 404 error envelope (it is never rewritten to the SPA's `index.html`). Endpoints that fetch a single Kubernetes object return 404 only when the object is actually missing; any other API-server failure surfaces as a 500. `/api/summary/trend` returns `503 Service Unavailable` when every Prometheus query fails (a full outage), while partial failures still return the series that succeeded.

### Validation

Query parameters are validated strictly. Unknown enum values (`?risk=foo`, `?autoscaler=maybe`, `?kind=Pod`) return 400 with the `field` set, instead of silently filtering out every workload. Likewise out-of-range integers (`?page=-1`, `?limit=10000`) and malformed durations (`?window=junk`) get a 400 pointing at the offending input.

## Helm Values Reference

| Key                              | Default                    | Description                              |
|----------------------------------|----------------------------|------------------------------------------|
| `dashboard.enabled`              | `true`                     | Enable the dashboard deployment          |
| `dashboard.replicaCount`         | `1`                        | Number of dashboard replicas             |
| `dashboard.bindAddress`          | `:8090`                    | Server bind address (`:port` or `host:port`); the container port derives from its port part |
| `dashboard.logLevel`             | `info`                     | Log level                                |
| `dashboard.corsAllowedOrigins`   | `[]`                       | Allowed CORS origins. Empty = same-origin only. |
| `dashboard.service.type`         | `ClusterIP`                | Service type                             |
| `dashboard.service.port`         | `8090`                     | Service port                             |
| `dashboard.resources`            | 10m CPU / 32-64Mi memory   | Pod resource requests/limits             |
| `dashboard.nodeSelector`         | `{}`                       | Node selector                            |
| `dashboard.tolerations`          | `[]`                       | Tolerations                              |
| `dashboard.affinity`             | `{}`                       | Affinity rules                           |
