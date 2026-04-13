# Prerequisites

## Kubernetes

| Requirement | Version |
|-------------|---------|
| Kubernetes | ≥ 1.24 |
| Kubernetes (in-place updates) | ≥ 1.27 |

## Helm

Helm 3.10+ is required to deploy the chart.

```bash
helm version
```

## Prometheus

k8s-sustain queries Prometheus for historical usage data. The chart can deploy a standalone Prometheus instance for you (default), or you can point it at an existing one.

If you bring your own Prometheus, make sure **kube-state-metrics** and **cAdvisor** metrics are scraped:

- `kube_pod_owner` — maps pods to their workload owner
- `kube_replicaset_owner` — resolves ReplicaSet → Deployment
- `container_cpu_usage_seconds_total` — CPU usage per container
- `container_memory_working_set_bytes` — memory usage per container

## TLS certificate (webhook only)

The admission webhook requires a valid TLS certificate trusted by the Kubernetes API server. Two options are supported:

- **cert-manager** (recommended) — set `webhook.certManager.enabled=true`
- **Manual secret** — create a `Secret` of type `kubernetes.io/tls` with `tls.crt` and `tls.key`, then set `webhook.tlsSecretName`

If you only use `Ongoing` mode (no `OnCreate`), the webhook is not needed and you can disable it with `webhook.enabled=false`.
