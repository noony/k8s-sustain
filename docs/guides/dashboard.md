# Dashboard

k8s-sustain includes a built-in web dashboard for exploring policies, viewing workload resource usage, and simulating policy changes before applying them.

## Features

- **Policy Overview** — List all policies with their status, targeted namespaces, and workload types. Supports auto-refresh.
- **Policy Detail** — View configuration parameters and matched workloads for each policy with namespace filtering and pagination.
- **Workload Metrics** — Interactive CPU and memory usage graphs (time-series) for each container in a workload. Supports auto-refresh.
- **Policy Simulator** — Tweak percentile, headroom, min/max parameters and instantly see how they would affect recommendations against historical data. Export results as YAML or CSV.
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

The overview page provides a cluster-wide summary of resource right-sizing:

- **Workload counts** — Total, automated, and manual workloads
- **CPU and Memory usage vs recommendation** — Aggregated across all automated workloads, showing actual Prometheus usage compared to computed recommendations with the delta percentage
- **Needs Attention table** — Workloads where the CPU or memory delta between usage and recommendation exceeds 5%, sorted by the largest gap. Click any workload to view its detail page.

The delta compares actual workload usage (at the configured percentile) against the recommendation. A negative delta means the recommendation is lower than current usage; a positive delta means the recommendation adds headroom above usage.

### Policies Page

The main page shows all `Policy` resources in your cluster with:

- Current status (Ready / Not Ready)
- Targeted namespaces
- Configured workload types and update modes
- Creation time

Click any policy to view its details. Enable **Auto-refresh** (30s interval) to keep the view up to date.

### Policy Detail

Shows the full configuration (percentile, headroom, min/max, window) for both CPU and memory, plus a table of all workloads matched by the policy with their current resource requests.

- **Namespace filter** — When a policy matches workloads in multiple namespaces, use the dropdown to filter by namespace.
- **Pagination** — Large workload lists are paginated (50 per page) with Previous/Next controls.
- **Auto-refresh** — Toggle to periodically reload data.

Click any workload to view its detail page.

### Workloads Page

Lists all workloads (Deployments, StatefulSets, DaemonSets, CronJobs) across the cluster, regardless of whether they are managed by a policy.

- **Status column** — Shows whether each workload is **Automated** (has a sustain policy) or **Manual**
- **Filters** — Filter by namespace, kind, automation status, or search by name
- **Policy link** — Automated workloads show a clickable link to their policy
- **Pagination** and **Auto-refresh** — Same controls as other pages

Click any workload to view its detail page.

### Workload Detail

Shows a comprehensive view of a single workload:

- **Automation status** — Whether the workload is managed by a policy, with a link to the policy
- **Recommendations** — If automated, shows the computed CPU and memory recommendations per container
- **CPU and Memory charts** — Interactive time-series with a sliding-window recommendation line overlaid (for automated workloads). The recommendation evolves over time, showing how it would have been computed at each point using the policy's configured window and parameters, rather than a flat line.
- **Open in Simulator** — Jump to the simulator with the workload pre-filled

A **time range selector** in the top-right lets you choose how much history to display: 1h, 4h, 12h, 1 day, 3 days, 7 days (default), or 30 days. The step resolution adjusts automatically for each range. You can also **drag to zoom** on any chart to focus on a specific time window — click and drag horizontally to select the region of interest. Zooming on a CPU chart automatically applies the same time range to the corresponding memory chart, and vice versa. A **Reset zoom** button appears in the top-right corner of each chart to return to the original view (resetting one also resets its paired chart). Panning is supported after zooming, and pinch-to-zoom works on touch devices. Each chart overlays the workload's **historical resource request** (amber dashed stepped line) and **limit** (orange dashed line) so you can see how actual usage compares to configured resources over time. The request line reflects real changes (e.g. from k8s-sustain patching or manual edits) rather than a flat snapshot. If historical request data is not available in Prometheus, the dashboard falls back to a static line from the current workload spec. If the workload is automated, the **recommendation** line (red dashed) is also shown.

Memory charts also display **OOM kill events** as red vertical markers with a count badge in the chart header. These are detected via `kube_pod_container_status_restarts_total` correlated with `kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`. If no kube-state-metrics is available, OOM markers are silently omitted.

Enable **Auto-refresh** to keep data current.

### Policy Simulator

The simulator lets you test "what-if" scenarios:

1. Select a **workload target** (namespace, kind, name) — both fields are required
2. Choose a **time range** (1h to 30 days) — controls how much history is displayed on the charts
3. Optionally, use the **Load from policy** dropdown to pre-fill all configuration fields (percentile, headroom, min/max, window) from an existing policy — useful as a starting point before tweaking values
4. Adjust **CPU and Memory parameters** independently:
    - Window (1h to 30 days) — the lookback period used to compute the recommendation, matching the policy CRD structure. This is independent of the chart time range.
    - Percentile (50th to 99th)
    - Headroom percentage (0-100%)
    - Min/Max allowed values

The simulation runs automatically whenever any parameter changes (with a short debounce to avoid excessive queries). There is no manual "Run" button — results update live as you adjust sliders, change windows, or modify min/max values.

The results show:

- Computed recommendation per container (CPU request, memory request)
- Time-series charts with a **sliding-window recommendation line** (red) that shows how the recommendation would have evolved at each point in time, **historical request** (amber stepped), and **current limit** (orange) overlaid on historical usage, making it easy to compare recommendations against both actual consumption and how resource requests evolved over time

#### Exporting Results

After running a simulation, use the export buttons to download recommendations:

- **YAML** — Downloads a Kubernetes resource patch you can apply with `kubectl apply -f`
- **CSV** — Downloads a spreadsheet-compatible file with per-container recommendations

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
