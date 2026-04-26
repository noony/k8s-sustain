# Dashboard

k8s-sustain includes a built-in web dashboard for exploring policies, viewing workload resource usage, and simulating policy changes before applying them.

## Features

- **Overview Story Flow** — Six-band cluster summary covering savings KPIs, 7-day trend, headroom breakdown, attention queue (at risk / drifted / blocked), policy effectiveness, and recent activity.
- **Workloads** — Cluster-wide list with risk/drift/autoscaler columns, plus filters for namespace, kind, risk state, and autoscaler presence.
- **Workload Detail** — Status snapshot (mode, last recycle, drift, OOM 24h), risk and HPA badges, blocked-state diagnostics, copy-as-YAML, and interactive CPU/memory charts with sliding-window recommendation, historical requests/limits, and OOM markers.
- **Policies** — 4-card stat strip (total policies, active workloads, CPU & memory savings) plus per-policy effectiveness columns.
- **Policy Detail** — Effectiveness time-series, view-as-YAML modal, time range selector, and matched workloads with risk/drift columns.
- **Policy Simulator** — Tweak percentile, headroom, min/max parameters; supports Argo Rollouts; shows projected savings impact; exports results as YAML, CSV, or Helm `--set` overrides.
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

| Flag                      | Default                    | Description                              |
|---------------------------|----------------------------|------------------------------------------|
| `--bind-address`          | `:8090`                    | Address the dashboard server listens on  |
| `--prometheus-address`    | `http://localhost:9090`    | Prometheus server URL                    |
| `--log-level`             | `info`                     | Log level (debug, info, warn, error)     |
| `--cors-allowed-origins`  | `*`                        | Allowed CORS origins (comma-separated)   |

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

## Using the Dashboard

### Overview Page

The overview is organised as a vertical "Story Flow" with six bands, each answering a specific operator question — from "what am I saving?" down to "what just happened?".

1. **KPI strip** — Headline savings cards for CPU (cores) and memory (bytes), each showing the absolute saving, the savings ratio versus current requests, and a sparkline of the last 24h. Two complementary cards count workloads currently **at risk** (drift exceeds the policy threshold) and **drifted** (request differs from the latest recommendation).
2. **Trend** — A 7-day cluster-wide chart of CPU and memory consumption, so you can correlate savings against real load.
3. **Headroom breakdown** — A stacked horizontal bar for CPU and memory split into `used`, `idle`, and `free` segments, sourced from the `k8s_sustain:cluster_cpu_headroom_breakdown` and `..._memory_headroom_breakdown` recording rules.
4. **Attention queue** — Three grouped lists: **At risk** (workloads exceeding the drift threshold), **Drifted** (request out-of-date with respect to the recommendation), and **Blocked** (workloads where the controller is in an exponential-backoff retry state). Each row links to the workload detail page.
5. **Policy effectiveness** — Per-policy rollup with the matched workload count, projected CPU/memory savings, and the count of at-risk workloads, so you can spot policies that need tuning.
6. **Activity feed** — Most recent reconcile and pod-recycle events from the controller, with timestamps and outcomes.

### Workloads Page

Lists every workload (Deployments, StatefulSets, DaemonSets, Argo Rollouts, CronJobs) across the cluster, regardless of whether it is governed by a policy.

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
- **Copy as YAML** — Builds a runnable manifest fragment (the `resources:` block keyed by container) that you can paste straight into a Helm values file or a workload spec.
- **CPU and Memory charts** — Interactive time-series with a sliding-window recommendation line overlaid (for automated workloads). The recommendation evolves over time, showing how it would have been computed at each point using the policy's configured window and parameters, rather than a flat line.
- **Open in Simulator** — Jump to the simulator with the workload pre-filled.

A **time range selector** in the top-right lets you choose how much history to display: 1h, 4h, 12h, 1 day, 3 days, 7 days (default), or 30 days. The step resolution adjusts automatically for each range. You can also **drag to zoom** on any chart to focus on a specific time window — click and drag horizontally to select the region of interest. Zooming on a CPU chart automatically applies the same time range to the corresponding memory chart, and vice versa. A **Reset zoom** button appears in the top-right corner of each chart to return to the original view (resetting one also resets its paired chart). Panning is supported after zooming, and pinch-to-zoom works on touch devices. Each chart overlays the workload's **historical resource request** (amber dashed stepped line) and **limit** (orange dashed line) so you can see how actual usage compares to configured resources over time. The request line reflects real changes (e.g. from k8s-sustain patching or manual edits) rather than a flat snapshot. If historical request data is not available in Prometheus, the dashboard falls back to a static line from the current workload spec. If the workload is automated, the **recommendation** line (red dashed) is also shown.

Memory charts also display **OOM kill events** as red vertical markers with a count badge in the chart header. These are detected via `kube_pod_container_status_restarts_total` correlated with `kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`. If no kube-state-metrics is available, OOM markers are silently omitted.

Enable **Auto-refresh** to keep data current.

### Policies Page

The Policies page leads with a **4-card stat strip** summarising the cluster-wide picture:

- **Total policies** — number of `Policy` resources in the cluster
- **Active workloads** — total workloads currently matched by any policy
- **CPU savings** — aggregated cluster-wide CPU saved (cores)
- **Memory savings** — aggregated cluster-wide memory saved (bytes)

Below the strip, the policy table replaces the previous Ready/Namespace columns with **effectiveness columns**: matched **workload count**, **CPU savings** and **memory savings** per policy, **at-risk** workload count, and **last applied** timestamp. The Ready status indicator is still shown alongside the policy name. Click any row to view the policy detail page.

### Policy Detail

Shows the full configuration (percentile, headroom, min/max, window) for both CPU and memory, plus the matched workloads table.

- **Header rows** — In addition to the existing summary fields, the header now displays the policy's **Update mode** so you can see at a glance how the policy is configured.
- **Effectiveness card** — A dedicated band with two time-series charts (CPU and memory) showing how this policy's savings have evolved over the selected time range.
- **TimeRangeSelector** — A range picker (1h to 30 days) drives the Effectiveness charts, matching the selector used elsewhere in the dashboard.
- **View as YAML modal** — Renders the entire `Policy` resource (sanitised of managed fields) inside a modal with a copy button — handy for sharing or storing in version control.
- **Matched workloads table** — Each row now shows **Risk** and **Drift %** columns alongside the existing namespace/kind/name and current resource requests, so you can prioritise which workloads to investigate from inside the policy view.
- **Namespace filter**, **pagination** (50 per page), and **auto-refresh** controls remain unchanged.

Click any workload to view its detail page.

### Policy Simulator

The simulator lets you test "what-if" scenarios:

1. Select a **workload target** (namespace, kind, name). The kind picker now includes **Argo Rollout** alongside Deployment, StatefulSet, and DaemonSet.
2. Choose a **time range** (1h to 30 days) — controls how much history is displayed on the charts.
3. Optionally, use the **Load from policy** dropdown to pre-fill all configuration fields (percentile, headroom, min/max, window) from an existing policy — useful as a starting point before tweaking values.
4. Adjust **CPU and Memory parameters** independently:
    - Window (1h to 30 days) — the lookback period used to compute the recommendation, matching the policy CRD structure. This is independent of the chart time range.
    - Percentile (50th to 99th)
    - Headroom percentage (0-100%)
    - Min/Max allowed values

The simulation runs automatically whenever any parameter changes (with a short debounce to avoid excessive queries). There is no manual "Run" button — results update live as you adjust sliders, change windows, or modify min/max values.

The results show:

- Computed recommendation per container (CPU request, memory request)
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

- **Recording rules not loaded** — k8s-sustain requires recording rules (`k8s_sustain:pod_workload`, `k8s_sustain:container_cpu_usage_by_workload:rate5m`, etc.). Verify they exist by querying `k8s_sustain:pod_workload` in Prometheus. If using the bundled Prometheus subchart, they are embedded automatically. If using an external Prometheus with the Prometheus Operator, set `controller.serviceMonitor.enabled=true` to deploy `PrometheusRule` resources.
- **Duplicate kube-state-metrics instances** — If multiple kube-state-metrics are scraped, the workload mapping rules can fail with "many-to-many matching not allowed". Either remove the duplicate kube-state-metrics or upgrade the chart (the recording rules deduplicate series automatically since v0.3).
- **Missing upstream metrics** — The recording rules depend on `kube_pod_owner`, `kube_replicaset_owner`, `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, and `kube_pod_container_resource_requests` (for historical request lines). Ensure kube-state-metrics and cAdvisor metrics are scraped.

## Helm Values Reference

| Key                              | Default                    | Description                              |
|----------------------------------|----------------------------|------------------------------------------|
| `dashboard.enabled`              | `false`                    | Enable the dashboard deployment          |
| `dashboard.replicaCount`         | `1`                        | Number of dashboard replicas             |
| `dashboard.port`                 | `8090`                     | Container port                           |
| `dashboard.bindAddress`          | `:8090`                    | Server bind address                      |
| `dashboard.logLevel`             | `info`                     | Log level                                |
| `dashboard.corsAllowedOrigins`   | `["*"]`                    | Allowed CORS origins                     |
| `dashboard.service.type`         | `ClusterIP`                | Service type                             |
| `dashboard.service.port`         | `8090`                     | Service port                             |
| `dashboard.resources`            | 10m CPU / 32-64Mi memory   | Pod resource requests/limits             |
| `dashboard.nodeSelector`         | `{}`                       | Node selector                            |
| `dashboard.tolerations`          | `[]`                       | Tolerations                              |
| `dashboard.affinity`             | `{}`                       | Affinity rules                           |
