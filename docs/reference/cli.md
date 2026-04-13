# CLI Reference

The `k8s-sustain` binary exposes two subcommands. Both are packaged in the same container image.

---

## `k8s-sustain start`

Starts the controller manager. Watches `Policy` objects and periodically reconciles `Ongoing`-mode workloads.

```
k8s-sustain start [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to |
| `--health-probe-bind-address` | `:8081` | Address the `/healthz` and `/readyz` endpoints bind to |
| `--leader-elect` | `false` | Enable leader election for high-availability deployments |
| `--zap-log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `--prometheus-address` | `http://localhost:9090` | Address of the Prometheus server used for metric queries |
| `--reconcile-interval` | `1h` | How often policies are re-evaluated (e.g. `30m`, `6h`) |
| `--config` | — | Path to a config file (YAML); all flags can be set there |

### Environment variables

Every flag can be overridden with an environment variable prefixed by `K8SSUSTAIN_` (uppercase, hyphens → underscores):

```bash
K8SSUSTAIN_RECONCILE_INTERVAL=30m k8s-sustain start
```

### Health endpoints

| Path | Port | Description |
|------|------|-------------|
| `/healthz` | `:8081` | Liveness — returns `200 OK` when the process is alive |
| `/readyz` | `:8081` | Readiness — returns `200 OK` when the controller cache is synced |
| `/metrics` | `:8080` | Prometheus metrics for the controller itself |

---

## `k8s-sustain webhook`

Starts the mutating admission webhook server. Listens for `Pod CREATE` admission requests and injects resources from `OnCreate`-mode policies.

```
k8s-sustain webhook [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `9443` | Port the HTTPS server listens on |
| `--tls-cert-file` | `/tls/tls.crt` | Path to the TLS certificate file |
| `--tls-key-file` | `/tls/tls.key` | Path to the TLS private key file |
| `--prometheus-address` | `http://localhost:9090` | Address of the Prometheus server |
| `--zap-log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |
| `--health-probe-bind-address` | `:8082` | Address the `/healthz` endpoint binds to |
| `--config` | — | Path to a config file (YAML) |

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
helm upgrade k8s-sustain k8s-sustain/k8s-sustain \
  --reuse-values \
  --set webhook.failurePolicy=Fail
```

!!! warning "Using `Fail` in production"
    Setting `failurePolicy: Fail` means **pod creation is blocked** if the webhook is unavailable. Only use this if you have ≥2 webhook replicas and are confident in the availability of Prometheus.
