# Helm Values Reference

## Global

| Value | Default | Description |
|-------|---------|-------------|
| `image.repository` | `ghcr.io/noony/k8s-sustain` | Container image repository |
| `image.tag` | `""` | Image tag; defaults to `Chart.appVersion` |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `imagePullSecrets` | `[]` | Image pull secrets |
| `nameOverride` | `""` | Override the chart name |
| `fullnameOverride` | `""` | Override the full release name |
| `recommendOnly` | `false` | Compute recommendations without recycling or mutating pods (dry-run mode) |
| `prometheusAddress` | `""` | Prometheus server URL, shared by all components. Leave empty to auto-detect the bundled subchart service. |

---

## Controller

| Value | Default | Description |
|-------|---------|-------------|
| `controller.replicaCount` | `1` | Controller replicas |
| `controller.metricsBindAddress` | `:8080` | Metrics endpoint address |
| `controller.healthProbeBindAddress` | `:8081` | Health probe address |
| `controller.leaderElect` | `true` | Enable leader election |
| `controller.logLevel` | `info` | Log level |
| `controller.service.type` | `ClusterIP` | Service type for the metrics endpoint |
| `controller.service.port` | `8080` | Service port |
| `controller.resources` | see below | Controller container resources |
| `controller.nodeSelector` | `{}` | Node selector |
| `controller.tolerations` | `[]` | Tolerations |
| `controller.affinity` | `{}` | Affinity rules |

**Default resources:**

```yaml
controller:
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
| `webhook.logLevel` | `info` | Log level |
| `webhook.failurePolicy` | `Ignore` | `Ignore` or `Fail` |
| `webhook.excludedNamespaces` | `[]` | Extra namespaces to exclude from webhook interception (the release namespace, `kube-system`, and `kube-public` are always excluded) |
| `webhook.tlsSecretName` | `k8s-sustain-webhook-tls` | TLS secret name |
| `webhook.caBundle` | `""` | Base64-encoded CA cert (required when `certManager.enabled=false`) |
| `webhook.certManager.enabled` | `false` | Create a cert-manager `Certificate` resource |
| `webhook.certManager.createIssuer` | `true` | Create a self-signed `Issuer` in the release namespace. Set to `false` to use your own. |
| `webhook.certManager.issuerRef.name` | `""` | Issuer name (only used when `createIssuer=false`) |
| `webhook.certManager.issuerRef.kind` | `Issuer` | Issuer kind (only used when `createIssuer=false`) |
| `webhook.resources` | see below | Webhook container resources |
| `webhook.nodeSelector` | `{}` | Node selector |
| `webhook.tolerations` | `[]` | Tolerations |
| `webhook.affinity` | `{}` | Affinity rules |

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

## Dashboard

| Value | Default | Description |
|-------|---------|-------------|
| `dashboard.enabled` | `false` | Deploy the dashboard |
| `dashboard.replicaCount` | `1` | Dashboard replicas |
| `dashboard.port` | `8090` | Container port |
| `dashboard.bindAddress` | `:8090` | Server bind address |
| `dashboard.logLevel` | `info` | Log level |
| `dashboard.service.type` | `ClusterIP` | Service type |
| `dashboard.service.port` | `8090` | Service port |
| `dashboard.resources` | see below | Dashboard container resources |
| `dashboard.nodeSelector` | `{}` | Node selector |
| `dashboard.tolerations` | `[]` | Tolerations |
| `dashboard.affinity` | `{}` | Affinity rules |

**Default dashboard resources:**

```yaml
dashboard:
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
| `serviceAccount.create` | `true` | Create a ServiceAccount |
| `serviceAccount.name` | `""` | Override the ServiceAccount name |
| `serviceAccount.annotations` | `{}` | Annotations on the ServiceAccount (e.g. for IRSA or Workload Identity) |

---

## ServiceMonitor

Only needed when running the Prometheus Operator externally (not the bundled subchart).

| Value | Default | Description |
|-------|---------|-------------|
| `controller.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` and `PrometheusRule` |
| `controller.serviceMonitor.interval` | `30s` | Scrape interval |
| `controller.serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout |

---

## CRDs

| Value | Default | Description |
|-------|---------|-------------|
| `installCRDs` | `true` | Install the `Policy` CRD as part of the chart |

---

## Prometheus subchart

Pass any value supported by the [prometheus chart](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus) under the `prometheus:` key. Recording rules for k8s-sustain are embedded in `prometheus.server.serverFiles` by default.

Common overrides:

```yaml
prometheus:
  enabled: true
  server:
    retention: 15d
    persistentVolume:
      size: 20Gi
```
