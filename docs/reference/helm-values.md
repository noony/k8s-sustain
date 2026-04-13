# Helm Values Reference

## Controller

| Value | Default | Description |
|-------|---------|-------------|
| `replicaCount` | `1` | Controller replicas |
| `image.repository` | `ghcr.io/noony/k8s-sustain` | Container image repository |
| `image.tag` | `""` | Image tag; defaults to `Chart.appVersion` |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Image pull secrets |
| `nameOverride` | `""` | Override the chart name |
| `fullnameOverride` | `""` | Override the full release name |
| `manager.metricsBindAddress` | `:8080` | Metrics endpoint address |
| `manager.healthProbeBindAddress` | `:8081` | Health probe address |
| `manager.leaderElect` | `true` | Enable leader election |
| `manager.logLevel` | `info` | Log level |
| `resources` | see below | Controller container resources |
| `nodeSelector` | `{}` | Node selector for all pods |
| `tolerations` | `[]` | Tolerations for all pods |
| `affinity` | `{}` | Affinity rules for all pods |

**Default resources:**

```yaml
resources:
  requests:
    cpu: 10m
    memory: 64Mi
  limits:
    memory: 128Mi
```

---

## Webhook

| Value | Default | Description |
|-------|---------|-------------|
| `webhook.enabled` | `true` | Deploy the admission webhook |
| `webhook.replicaCount` | `1` | Webhook replicas (≥2 recommended for production) |
| `webhook.port` | `9443` | HTTPS server port |
| `webhook.prometheusAddress` | `http://localhost:9090` | Prometheus address |
| `webhook.logLevel` | `info` | Log level |
| `webhook.failurePolicy` | `Ignore` | `Ignore` or `Fail` |
| `webhook.excludedNamespaces` | `[]` | Extra namespaces to exclude from webhook interception (the release namespace, `kube-system`, and `kube-public` are always excluded) |
| `webhook.tlsSecretName` | `k8s-sustain-webhook-tls` | TLS secret name |
| `webhook.caBundle` | `""` | Base64-encoded CA cert (required when `certManager.enabled=false`) |
| `webhook.certManager.enabled` | `false` | Create a cert-manager `Certificate` resource |
| `webhook.certManager.issuerRef.name` | `selfsigned-issuer` | Issuer name |
| `webhook.certManager.issuerRef.kind` | `ClusterIssuer` | Issuer kind |
| `webhook.resources` | see below | Webhook container resources |

**Default webhook resources:**

```yaml
webhook:
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      memory: 64Mi
```

---

## ServiceAccount

| Value | Default | Description |
|-------|---------|-------------|
| `serviceAccount.annotations` | `{}` | Annotations on the ServiceAccount (e.g. for IRSA or Workload Identity) |

---

## Service (metrics)

| Value | Default | Description |
|-------|---------|-------------|
| `service.type` | `ClusterIP` | Service type for the metrics endpoint |
| `service.port` | `8080` | Service port |

---

## ServiceMonitor

| Value | Default | Description |
|-------|---------|-------------|
| `serviceMonitor.enabled` | `true` | Create a Prometheus Operator `ServiceMonitor` and `PrometheusRule` |
| `serviceMonitor.interval` | `30s` | Scrape interval |
| `serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout |

---

## CRDs

| Value | Default | Description |
|-------|---------|-------------|
| `installCRDs` | `true` | Install the `Policy` CRD as part of the chart |

---

## kube-prometheus-stack subchart

Pass any value supported by the [kube-prometheus-stack chart](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) under the `kube-prometheus-stack:` key.

Common overrides:

```yaml
kube-prometheus-stack:
  enabled: true
  prometheus:
    prometheusSpec:
      retention: 15d
      storageSpec:
        volumeClaimTemplate:
          spec:
            resources:
              requests:
                storage: 20Gi
```
