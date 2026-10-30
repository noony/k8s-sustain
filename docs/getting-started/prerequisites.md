# Prerequisites

Cluster, tooling, and metrics requirements before installing k8s-sustain.

## Kubernetes

Kubernetes **1.30 or later**. The CRDs' CEL validation rules use the Kubernetes `quantity()` library, which older API servers reject when the CRD is applied. The Policy CRD's rules also rely on CRD validation ratcheting (`CRDValidationRatcheting`, on by default from 1.30): an update that leaves a field a rule rejects unchanged is still admitted, so a stored Policy can always be updated, its finalizer removed included.

Running pods are resized in place from 1.33; older clusters fall back to eviction. See the [version matrix](../concepts/in-place-updates.md#version-matrix).

## Helm

Helm 3.10+ is required to deploy the chart.

```bash
helm version
```

## Prometheus

k8s-sustain queries Prometheus for historical usage data. The chart bundles a [Prometheus](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus) instance by default, or you can point it at an existing Prometheus.

If you bring your own Prometheus, make sure **kube-state-metrics** and **cAdvisor** metrics are scraped. The bundled Prometheus subchart restricts both jobs to an allowlist via `metric_relabel_configs` to keep TSDB cardinality low; bring-your-own deployments can do the same. [Signal contracts](../reference/recording-rules.md#signal-contracts) lists the series each recommendation signal computes from; the others below feed the dashboard.

From **kube-state-metrics**:

- `kube_pod_owner` — maps pods to their workload owner
- `kube_pod_annotations` — carries the `k8s.sustain.io/owner-name` annotation for [identity grouping](../guides/standalone-pods-and-grouping.md). kube-state-metrics only exports it when allowlisted: start it with `--metric-annotations-allowlist=pods=[k8s.sustain.io/owner-name]` (kube-state-metrics 2.2 or later; `metricAnnotationsAllowList: ["pods=[k8s.sustain.io/owner-name]"]` in its Helm chart). Without it, a pod using the override is mapped to its real owner, or not at all for a bare pod, and its usage never reaches the identity the controller groups it under
- `kube_job_owner` — resolves Job → CronJob
- `kube_replicaset_owner` — resolves ReplicaSet → Deployment (or Rollout)
- `kube_pod_container_resource_requests` / `kube_pod_container_resource_limits` — current CPU/memory requests and limits
- `kube_pod_init_container_resource_requests` / `kube_pod_init_container_resource_limits` — same, for init and sidecar containers (kube-state-metrics reports init containers under these separate metric names)
- `kube_pod_container_status_restarts_total` / `kube_pod_container_status_last_terminated_reason` — OOMKill detection
- `kube_node_status_allocatable` — cluster capacity for the headroom panels

!!! warning "Breaking: the owner-name override moved from `kube_pod_labels` to `kube_pod_annotations`"
    Earlier releases had the webhook copy the `k8s.sustain.io/owner-name` annotation onto a pod label, which the recording rules read from `kube_pod_labels` (`--metric-labels-allowlist`). The webhook no longer writes that label ([ADR 0006](../adr/0006-owner-name-is-read-from-the-annotation.md)). With your own Prometheus, add the annotation allowlist above and keep `kube_pod_annotations` in your scrape keep-list before upgrading; the `kube_pod_labels` allowlist entry and keep-list entry can go. The bundled Prometheus is configured for you.

From **cAdvisor**:

- `container_cpu_usage_seconds_total` — CPU usage per container
- `container_memory_working_set_bytes` — memory usage per container
- `container_memory_max_usage_bytes` (cgroup v1) / `container_memory_peak_working_set_bytes` (cgroup v2) — peak memory between scrapes
- `container_spec_memory_limit_bytes` — the limit that was in effect when an OOMKill fired

The recording rules filter cAdvisor series by `node!=""`, so your scrape config must inject a `node` label onto cAdvisor metrics:

```yaml
relabel_configs:
  - source_labels: [__meta_kubernetes_node_name]
    target_label: node
```

kube-prometheus-stack does this by default; the bundled Prometheus subchart is configured to do it as well.

### Authenticated or multi-tenant Prometheus

If the Prometheus you bring requires credentials, sits behind a private CA, or
is a Thanos / Mimir / Cortex query gateway expecting a tenant header, set the
top-level `prometheusAuth` values block — bearer token (inline or from a
rotating file), basic auth, arbitrary headers, and TLS (custom CA, mTLS, SNI
override) are all supported. The controller and the dashboard are wired
identically; the webhook needs nothing, since it never queries Prometheus. See
the [Authenticated Prometheus guide](../guides/authenticated-prometheus.md).

## TLS certificate

The admission webhook requires a TLS certificate trusted by the Kubernetes API server. Two options are supported:

- **cert-manager** (default) — install [cert-manager](https://cert-manager.io/) before the chart; with `webhook.certManager.enabled=true` (the default) the chart creates the Certificate and cert-manager injects the CA bundle.
- **Manual** — set `webhook.certManager.enabled=false`, create a `Secret` of type `kubernetes.io/tls` with `tls.crt` and `tls.key` in the release namespace, set `webhook.tlsSecretName` to its name, and set `webhook.caBundle` to the base64-encoded CA certificate that signed it. The certificate must carry the webhook Service DNS names as SANs — see the [cert-manager guide](../guides/cert-manager.md).

The webhook cannot be skipped: see [The admission webhook is required](installation.md#the-admission-webhook-is-required).
