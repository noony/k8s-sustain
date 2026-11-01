<!-- Source of truth: internal/config/config.go -->

# CLI Reference

The `k8s-sustain` binary exposes three operational subcommands — `start`, `webhook`, and `dashboard` — plus a `version` helper. All are packaged in the same container image.

## Global flags

These flags are available on every subcommand. They carry the cluster-scope settings, which every component must read alike: each is one flag, one config-file key and one environment variable, whichever subcommand reads it, so the controller, the webhook and the dashboard cannot be configured apart. A subcommand ignores the ones it has no use for: the webhook accepts the Prometheus flags and never queries Prometheus.

| Flag | Default | Description |
|------|---------|-------------|
| `--recommend-only` | `false` | Compute recommendations but never recycle pods or mutate pods (dry-run mode) |
| `--excluded-namespaces` | — | Comma-separated list of namespaces no Policy governs, whatever they opt into: the controller never touches their workloads, the webhook never mutates their pods, the OOM watcher ignores their kills, and the dashboard shows their workloads governed by no Policy. The chart also drops them from the `MutatingWebhookConfiguration`'s `namespaceSelector`, so the webhook never sees their pods. |
| `--prometheus-address` | `http://localhost:9090` | Address of the Prometheus server the controller and the dashboard query |
| `--config` | — | Path to a YAML config file. Keys are the flag names, a `webhook` or `dashboard` flag's prefixed with `webhook.` or `dashboard.`. No file is read unless this flag is set, and it has no environment-variable form. |

When `--recommend-only` is enabled, the controller still queries Prometheus and computes recommendations as usual, and the webhook still resolves workloads and reads the cached recommendation (it never queries Prometheus itself, recommend-only or not), but nothing **applies** changes. Computed recommendations are emitted as structured log lines at `info` level, so you can inspect them before switching to active mode.

For a dry-run scoped to a single policy instead of the whole installation, set `spec.rightSizing.recommendOnly: true` on that `Policy` — see the [Policy reference](policy.md#specrightsizingrecommendonly). The global flag always wins: when it is set, every policy is dry-run regardless of its own field.

```bash
# via flag
k8s-sustain start --recommend-only

# via environment variable
K8SSUSTAIN_RECOMMEND_ONLY=true k8s-sustain start

# via config file (k8s-sustain start --config /etc/k8s-sustain/config.yaml)
recommend-only: true
```

### Prometheus authentication and TLS flags

Global flags too, so the controller and the dashboard always authenticate to
Prometheus alike, from one set of environment variables
(`K8SSUSTAIN_PROMETHEUS_*`); the webhook accepts them and ignores them. The Helm
chart renders them from the `prometheusAuth` block — see
[Helm values: Prometheus authentication](helm-values.md#prometheus-authentication).
Worked examples live in the
[Authenticated Prometheus guide](../guides/authenticated-prometheus.md).

| Flag | Default | Description |
|------|---------|-------------|
| `--prometheus-bearer-token` | — | Static bearer token sent as `Authorization: Bearer <token>` on every request. Mutually exclusive with `--prometheus-bearer-token-file`. Read once at startup, so a rotating token must use the file form. |
| `--prometheus-bearer-token-file` | — | Path to a file holding the bearer token. **Re-read on every request**, so projected service-account tokens keep working across rotation and a Secret update needs no restart. |
| `--prometheus-basic-auth-username` | — | Username for HTTP basic auth. Required whenever a password or password file is set. |
| `--prometheus-basic-auth-password` | — | Password for HTTP basic auth. Mutually exclusive with `--prometheus-basic-auth-password-file`. |
| `--prometheus-basic-auth-password-file` | — | Path to a file holding the basic-auth password. Re-read on every request. |
| `--prometheus-headers` | — | Extra HTTP header sent on every request, as `Key=Value` (e.g. `X-Scope-OrgID=tenant-a` for a multi-tenant Thanos/Mimir/Cortex gateway). **Repeat the flag** for several headers; each occurrence is taken verbatim, so a value may contain commas or quotes. Only the first `=` splits an entry, so values may contain `=`. The env-var form (`K8SSUSTAIN_PROMETHEUS_HEADERS`) is a single comma-separated string instead, so a value with a comma must use the flag. Header names must be valid HTTP tokens and values must not contain control characters — both are checked at startup. |
| `--prometheus-tls-ca-file` | — | PEM CA bundle used to verify the Prometheus server certificate. **Appended** to a copy of the system trust store rather than replacing it. |
| `--prometheus-tls-cert-file` | — | Client certificate for mutual TLS. Must be set together with `--prometheus-tls-key-file`. The pair is **re-read on every TLS handshake**, so a certificate rotated in place (cert-manager renewing the Secret, a mesh sidecar refreshing its pair) is used without a restart. |
| `--prometheus-tls-key-file` | — | Client private key for mutual TLS. Must be set together with `--prometheus-tls-cert-file`. Re-read on every handshake, like the certificate. |
| `--prometheus-tls-server-name` | — | Overrides the SNI / certificate hostname verified against the server certificate. Reach for this before `--prometheus-tls-insecure-skip-verify`. |
| `--prometheus-tls-insecure-skip-verify` | `false` | Disable server-certificate verification. Insecure — the connection can be intercepted, and every credential above with it. Logs a loud warning at startup. |

Every file named above is also read **once at construction**, so a bad path, a
malformed key pair, or a CA file with no valid PEM block fails the process at
startup (`CrashLoopBackOff`) instead of degrading into per-query errors that
look like a Prometheus outage. Credentials are never logged.

Conflicting combinations are rejected at startup rather than silently resolved:
bearer token + bearer token file, password + password file, bearer + basic
auth, a password without a username, an `Authorization` entry in
`--prometheus-headers` alongside bearer/basic auth, a header name or value
that HTTP would reject, and a TLS cert without its key.

If the system certificate store cannot be read, `--prometheus-tls-ca-file`
becomes the *only* trusted root; the startup log says so with a `WARNING`
line, since a public-CA ingress on the same address would then fail with
"unknown authority".

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
| `--leader-election-id` | `k8s-sustain-leader-election` | Lease name used for leader election. Override when running several installs in the same cluster. |
| `--log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `--reconcile-interval` | `5m` | How often policies are re-evaluated. Must be greater than zero. It also sizes the staleness budget: a live identity's recommendation is served for the larger of 30 minutes and three intervals past its last computation, so any interval keeps recommendations served between passes, and a pass can run late without pods losing them. See [How long a recommendation is served](../concepts/workload-recommendations.md#how-long-a-recommendation-is-served). |
| `--workload-concurrency-limit` | `5` | Maximum number of workloads processed in parallel per reconcile cycle |
| `--policy-concurrency-limit` | `10` | Maximum number of Policy objects reconciled in parallel |
| `--prometheus-max-inflight` | `8` | Maximum concurrent Prometheus queries across the whole controller. Kept below Prometheus's own `--query.max-concurrency` (default 20) so k8s-sustain does not starve dashboards and alerting sharing the same server. A query that cannot get a slot within 2 minutes is abandoned rather than queued indefinitely; that is counted as a batch failure, not as a Prometheus failure, so it never trips the circuit breaker |
| `--recycle-replacement-timeout` | `5m` | In the eviction-fallback recycle path, how long to wait for a replacement pod to become Ready before aborting the loop. Increase on clusters where node autoscaling (Karpenter / cluster-autoscaler) regularly takes longer than the default. |
| `--recommendation-retention` | `168h` | How long a Departed identity's WorkloadRecommendation (bare pods, deleted or finished Jobs) is kept, and its recommendation served, past its last computation; the same for a Conflicted identity's frozen one. Set it above the longest gap between runs of a recurring workload. Must be greater than zero. The webhook has no such flag: it serves what the controller stamped. See [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads). |
| `--query-shard-max-samples` | `10000000` | Projected Prometheus sample budget (containers × samples per container, summed across a shard's workloads: window-minutes for the CPU and memory queries, one per rule for the OOM query) a single batched shard query is allowed to reach before a new shard is started. Keep this under Prometheus's own `--query.max-samples` (default `50000000`): that server-side limit *rejects* an over-budget query outright, failing every workload sharing the shard, not just the excess ones. The default leaves a 5x margin. |

### Log verbosity

- `info` (default) — high-signal events: reconcile cycle start/end with target counts, HPA detection, recommendations computed, in-place update applied, pod evictions, recommendation injection by the webhook.
- `debug` — adds per-container traces: Prometheus query parameters and result counts, raw percentile values, per-resource recommendations, HPA-aware adjustments, retry-backoff skips, eviction skips for non-stale or non-running pods, webhook admit decisions including standalone-pod / no-policy / no-data branches.

Use `debug` when investigating why a workload was or wasn't resized, or why an HPA adjustment behaved unexpectedly.

### Health endpoints

| Path | Port | Description |
|------|------|-------------|
| `/healthz` | `:8081` | Liveness — returns `200 OK` when the process is alive |
| `/readyz` | `:8081` | Readiness — returns `200 OK` when the process is alive (a ping; it does not wait for cache sync) |
| `/metrics` | `:8080` | Prometheus metrics for the controller itself |

---

## `k8s-sustain webhook`

Starts the mutating admission webhook server. Listens for `Pod CREATE` admission requests and injects the cached recommendation for any workload kind the Policy configures, whether `OnCreate` or `Ongoing`.

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

The webhook also honours each Policy's `spec.selector.namespaces` and `spec.selector.labelSelector` (see [Policy reference](./policy.md#specselector)). A pod is admitted unmutated if any of the following holds: its namespace is in `--excluded-namespaces`, its namespace is not in a non-empty `selector.namespaces`, or its pod labels — its pod template's, which the controller matches too — do not satisfy `selector.labelSelector`. A malformed `labelSelector` causes the webhook to fail open (admit without injection, log a warning with reason `selector-invalid`, whichever level the pod opts in at) rather than deny.

Each admission is bounded by a fixed 4s deadline, under the 5s `timeoutSeconds` of the chart's `MutatingWebhookConfiguration`: 3s for the governance decision's reads (the owner chain's apiserver Gets, 2s each at most, and the informer-cache reads of Policies and the Namespace) and 1s for the `WorkloadRecommendation` read from the informer cache. A read that runs out of its share fails open. None of these is a flag; see [Latency budget](../concepts/architecture.md#admission-webhook-k8s-sustain-webhook).

### Health endpoints

| Path | Port | Description |
|------|------|-------------|
| `/healthz` | webhook port | Returns `200 OK` — used as liveness probe (HTTPS) |
| `/metrics` | webhook port | Prometheus metrics for the webhook (HTTPS) |

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
    Setting `failurePolicy: Fail` means **pod creation is blocked** if the webhook is unavailable. Only use this if you have ≥2 webhook replicas. The webhook does not depend on Prometheus at admission time — it only needs the apiserver to read the cached `WorkloadRecommendation` — but it injects a recommendation only until the `servableUntil` the controller stamps (for a live identity, the larger of 30 minutes and three `--reconcile-interval`s past its last computation), so injections stop when the controller stops reconciling, and a webhook outage under `Fail` still blocks pod creation regardless.

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
| `--log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `--cors-allowed-origins` | *(empty)* | Comma-separated list of allowed CORS origins. Empty (default) = same-origin only. Use `*` to allow all (not recommended). |

It queries Prometheus through the global `--prometheus-address` and
[Prometheus authentication and TLS flags](#prometheus-authentication-and-tls-flags),
exactly as the controller does.

### Health endpoints

| Path | Port | Description |
|------|------|-------------|
| `/healthz` | `:8090` | Liveness — returns `200 OK` when the process is alive |
| `/readyz` | `:8090` | Readiness — `200` when Prometheus answers a ping, `503` otherwise |
| `/metrics` | `:8090` | Prometheus metrics for the dashboard, on the same port as the UI |

See the [Dashboard guide](../guides/dashboard.md) for full usage instructions.

---

## Environment variables

Every flag except `--config` can be set with an environment variable: `K8SSUSTAIN_` + the flag's key, upper-cased, with `-` and `.` replaced by `_`. List-valued flags take a comma-separated string.

| Flags | Key | Example |
|---|---|---|
| [global](#global-flags) | flag name | `K8SSUSTAIN_EXCLUDED_NAMESPACES`, `K8SSUSTAIN_PROMETHEUS_ADDRESS` |
| `start` | flag name | `K8SSUSTAIN_RECONCILE_INTERVAL` |
| `webhook` | `webhook.` + flag name | `K8SSUSTAIN_WEBHOOK_PORT` |
| `dashboard` | `dashboard.` + flag name | `K8SSUSTAIN_DASHBOARD_BIND_ADDRESS` |

A global flag's variable reaches every subcommand. A subcommand's own flag is read only under its own key: `--log-level` is the one flag name the three subcommands share, and each reads its own variable (`K8SSUSTAIN_LOG_LEVEL`, `K8SSUSTAIN_WEBHOOK_LOG_LEVEL`, `K8SSUSTAIN_DASHBOARD_LOG_LEVEL`); a variable meant for another subcommand is silently ignored.

```bash
# read alike by start, webhook and dashboard
K8SSUSTAIN_EXCLUDED_NAMESPACES=kube-system,monitoring k8s-sustain webhook
# a live identity's recommendation then stays served for 1h30m (three intervals)
# past its last computation, rather than the 30m floor
K8SSUSTAIN_RECONCILE_INTERVAL=30m k8s-sustain start
K8SSUSTAIN_DASHBOARD_BIND_ADDRESS=:9999 k8s-sustain dashboard
```

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
