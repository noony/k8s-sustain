<!-- Source of truth: internal/config/config.go -->

# CLI Reference

The `k8s-sustain` binary exposes three operational subcommands — `start`, `webhook`, and `dashboard` — plus a `version` helper. All are packaged in the same container image.

## Global flags

These flags are available on every subcommand.

| Flag | Default | Description |
|------|---------|-------------|
| `--recommend-only` | `false` | Compute recommendations but never recycle pods or mutate pods (dry-run mode) |
| `--config` | — | Path to a config file (YAML); all flags can be set there |

When `--recommend-only` is enabled, the controller still queries Prometheus and computes recommendations as usual, and the webhook still resolves workloads and reads the cached recommendation (it never queries Prometheus itself, recommend-only or not), but nothing **applies** changes. Computed recommendations are emitted as structured log lines at `info` level, so you can inspect them before switching to active mode.

For a dry-run scoped to a single policy instead of the whole installation, set `spec.rightSizing.recommendOnly: true` on that `Policy` — see the [Policy reference](policy.md#specrightsizingrecommendonly). The global flag always wins: when it is set, every policy is dry-run regardless of its own field.

```bash
# via flag
k8s-sustain start --recommend-only

# via environment variable
K8SSUSTAIN_RECOMMEND_ONLY=true k8s-sustain start

# via config file (.k8s-sustain.yaml)
recommend-only: true
```

---

## `k8s-sustain start`

Starts the controller. Watches `Policy` objects and periodically reconciles `Ongoing`-mode workloads.

```text
k8s-sustain start [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to |
| `--health-probe-bind-address` | `:8081` | Address the `/healthz` and `/readyz` endpoints bind to |
| `--leader-elect` | `false` | Enable leader election for high-availability deployments |
| `--log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `--prometheus-address` | `http://localhost:9090` | Address of the Prometheus server used for metric queries |
| `--reconcile-interval` | `5m` | How often policies are re-evaluated (e.g. `30m`, `6h`) |
| `--excluded-namespaces` | — | Comma-separated list of namespaces the reconciler should never touch |
| `--workload-concurrency-limit` | `5` | Maximum number of workloads processed in parallel per reconcile cycle |
| `--policy-concurrency-limit` | `10` | Maximum number of Policy objects reconciled in parallel |
| `--prometheus-max-inflight` | `8` | Maximum concurrent Prometheus queries across the whole controller. Kept below Prometheus's own `--query.max-concurrency` (default 20) so k8s-sustain does not starve dashboards and alerting sharing the same server. A query that cannot get a slot within 2 minutes is abandoned rather than queued indefinitely; that is counted as a batch failure, not as a Prometheus failure, so it never trips the circuit breaker |
| `--recycle-replacement-timeout` | `5m` | In the eviction-fallback recycle path, how long to wait for a replacement pod to become Ready before aborting the loop. Increase on clusters where node autoscaling (Karpenter / cluster-autoscaler) regularly takes longer than the default. |
| `--recommendation-retention` | `168h` | How long a WorkloadRecommendation is kept after its workload object disappears (ephemeral bare pods, deleted or terminal Jobs) — that is, how long a departed identity's last-known-good keeps being served. It does **not** affect how often anything is recomputed: a retained identity is recomputed on `--reconcile-interval` like every other cache object. Because the webhook's only source is this object, the window decides whether a *recurring* ephemeral identity is rightsized at admission on its next run, so set it above the longest expected gap between runs — see [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads). The dashboard shows retained entries as inactive workloads. `0` sweeps them on the next reconcile. The window is measured from `status.observedAt`, which keeps being refreshed while the identity's samples remain inside the query window, so the clock only starts once that history ages out — budget object count against roughly `window + retention`. |
| `--query-shard-max-samples` | `10000000` | Projected Prometheus sample budget (containers × window-minutes, summed across a shard's workloads) a single batched CPU/memory/OOM shard query is allowed to reach before a new shard is started. Keep this under Prometheus's own `--query.max-samples` (default `50000000`): that server-side limit *rejects* an over-budget query outright, failing every workload sharing the shard, not just the excess ones. The default leaves a 5x margin. |

### Environment variables

Every flag can be overridden with an environment variable prefixed by `K8SSUSTAIN_` (uppercase, hyphens and dots → underscores). The mapping is:

- `K8SSUSTAIN_` + flag name with `-` and `.` replaced by `_`, upper-cased.

Examples:

```bash
# Top-level flag (controller)
K8SSUSTAIN_RECONCILE_INTERVAL=30m k8s-sustain start
K8SSUSTAIN_LOG_LEVEL=debug k8s-sustain start

# Subcommand-scoped flag (dashboard.bind-address, webhook.excluded-namespaces)
K8SSUSTAIN_DASHBOARD_BIND_ADDRESS=:9999 k8s-sustain dashboard
K8SSUSTAIN_WEBHOOK_EXCLUDED_NAMESPACES=kube-system,monitoring k8s-sustain webhook

# List-valued flag — comma-separated, same syntax as --excluded-namespaces=a,b
K8SSUSTAIN_EXCLUDED_NAMESPACES=kube-system,monitoring k8s-sustain start
```

### Log verbosity

- `info` (default) — high-signal events: reconcile cycle start/end with target counts, HPA detection, recommendations computed, in-place update applied, pod evictions, recommendation injection by the webhook.
- `debug` — adds per-container traces: Prometheus query parameters and result counts, raw percentile values, per-resource recommendations, HPA-aware adjustments, retry-backoff skips, eviction skips for non-stale or non-running pods, webhook admit decisions including standalone-pod / no-policy / no-data branches.

Use `debug` when investigating why a workload was or wasn't resized, or why an HPA adjustment behaved unexpectedly.

### Health endpoints

| Path | Port | Description |
|------|------|-------------|
| `/healthz` | `:8081` | Liveness — returns `200 OK` when the process is alive |
| `/readyz` | `:8081` | Readiness — returns `200 OK` when the controller cache is synced |
| `/metrics` | `:8080` | Prometheus metrics for the controller itself |

---

## `k8s-sustain webhook`

Starts the mutating admission webhook server. Listens for `Pod CREATE` admission requests and injects resources from `OnCreate`-mode policies.

```text
k8s-sustain webhook [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `9443` | Port the HTTPS server listens on |
| `--tls-cert-file` | `/tls/tls.crt` | Path to the TLS certificate file |
| `--tls-key-file` | `/tls/tls.key` | Path to the TLS private key file |
| `--log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `--excluded-namespaces` | — | Comma-separated list of namespaces the webhook must never mutate. Pods in these namespaces are admitted unchanged. Mirrors the controller flag so both components stay in lockstep. |
| `--recommendation-retention` | `168h` | Must match the controller flag of the same name. It bounds the one case where the webhook injects from a recommendation older than the 30 min staleness budget: an identity the controller marked *departed*, whose `status.observedAt` is frozen by design. Past this window the object is one the controller's sweep should already have deleted, so the webhook treats it as stale rather than injecting last-known-good forever — which is what a controller wedged before its sweep would otherwise cause. The chart renders both flags from the single `controller.recommendationRetention` value, so they cannot drift. |

The webhook also honours each Policy's `spec.selector.namespaces` and `spec.selector.labelSelector` (see [Policy reference](./policy.md#specselector)). A pod is admitted without mutation if any of the following holds: its namespace is in `--excluded-namespaces`, its namespace is not in a non-empty `selector.namespaces`, or its pod labels do not satisfy `selector.labelSelector`. A malformed `labelSelector` causes the webhook to fail open (admit without mutation, log a warning) rather than deny.

### Health endpoints

| Path | Port | Description |
|------|------|-------------|
| `/healthz` | webhook port | Returns `200 OK` — used as liveness probe (HTTPS) |

### Webhook endpoint

| Path | Method | Description |
|------|--------|-------------|
| `/mutate` | `POST` | Receives `AdmissionReview` v1 requests from the API server |

### Failure policy

The `MutatingWebhookConfiguration` is set to `failurePolicy: Ignore` by default. This means if the webhook is unreachable or returns an error, the pod is admitted unchanged. The controller will still apply `Ongoing` recommendations independently.

To change the failure policy:

```bash
helm upgrade k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --reuse-values \
  --set webhook.failurePolicy=Fail
```

!!! warning "Using `Fail` in production"
    Setting `failurePolicy: Fail` means **pod creation is blocked** if the webhook is unavailable. Only use this if you have ≥2 webhook replicas. The webhook no longer depends on Prometheus at admission time — it only needs the apiserver to read the cached `WorkloadRecommendation` — but it still needs the controller's cache to stay fresh (within `DefaultCacheStaleness`, 30 min) for injections to happen at all, and a webhook outage under `Fail` still blocks pod creation regardless.

---

## `k8s-sustain dashboard`

Starts the web dashboard server. Provides a UI for policy exploration, workload metrics visualization, and policy simulation.

```text
k8s-sustain dashboard [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--bind-address` | `:8090` | Address the HTTP server listens on |
| `--prometheus-address` | `http://localhost:9090` | Address of the Prometheus server |
| `--log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `--cors-allowed-origins` | *(empty)* | Comma-separated list of allowed CORS origins. Empty (default) = same-origin only. Use `*` to allow all (not recommended). |

### Health endpoints

| Path | Port | Description |
|------|------|-------------|
| `/healthz` | `:8090` | Returns `200 OK` — used as liveness/readiness probe |

See the [Dashboard guide](../guides/dashboard.md) for full usage instructions.

---

## `k8s-sustain version`

Prints the build-time version string and exits. Release builds embed the git tag via `-ldflags`; local and untagged builds report `dev`. The same value is logged at startup by every subcommand and is also available through the global `--version` flag.

```text
k8s-sustain version
```

```bash
# confirm which version is running in-cluster
kubectl exec deploy/k8s-sustain -n k8s-sustain -- /k8s-sustain version
```
