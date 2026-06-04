<!-- Source of truth: charts/k8s-sustain/values.yaml -->

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
| `controller.metricsBindAddress` | `:8080` | Metrics endpoint address (`:port` or `host:port`); the metrics container port derives from its port part |
| `controller.healthProbeBindAddress` | `:8081` | Health probe address (`:port` or `host:port`); the health container port derives from its port part |
| `controller.leaderElect` | `true` | Enable leader election |
| `controller.concurrencyLimit` | `5` | Maximum number of workloads processed in parallel per reconcile cycle |
| `controller.recycleReplacementTimeout` | `5m` | In the eviction-fallback recycle path, how long to wait for a replacement pod to become Ready before aborting the loop. Increase on clusters where Karpenter / cluster-autoscaler node provisioning regularly takes longer. |
| `controller.logLevel` | `debug` | Log level |
| `controller.service.type` | `ClusterIP` | Service type for the metrics endpoint |
| `controller.service.port` | `8080` | Service port |
| `controller.service.annotations` | `{}` | Extra annotations for the metrics Service (the chart already adds `prometheus.io/scrape`, `prometheus.io/port`, and `prometheus.io/path`) |
| `controller.resources` | see below | Controller container resources |
| `controller.livenessProbe` | see below | Liveness probe timings (`initialDelaySeconds`, `periodSeconds`, `timeoutSeconds`, `successThreshold`, `failureThreshold`). The probe endpoint (`/healthz` on the health port) is fixed by the chart. |
| `controller.readinessProbe` | see below | Readiness probe timings, same fields. The probe endpoint (`/readyz` on the health port) is fixed by the chart. |
| `controller.nodeSelector` | `{}` | Node selector |
| `controller.tolerations` | `[]` | Tolerations |
| `controller.affinity` | `{}` | Affinity rules |

**Default resources:**

```yaml
controller:
  resources:
    requests:
      cpu: 10m
      memory: 128Mi
    limits:
      memory: 256Mi
```

**Default probe timings** (same for `livenessProbe` and `readinessProbe`, and for all components):

```yaml
controller:
  livenessProbe:
    initialDelaySeconds: 10
    periodSeconds: 10
    timeoutSeconds: 1
    successThreshold: 1
    failureThreshold: 3
  readinessProbe:
    initialDelaySeconds: 10
    periodSeconds: 10
    timeoutSeconds: 1
    successThreshold: 1
    failureThreshold: 3
```

---

## Webhook

| Value | Default | Description |
|-------|---------|-------------|
| `webhook.enabled` | `true` | Deploy the admission webhook |
| `webhook.replicaCount` | `1` | Webhook replicas (≥2 recommended for production) |
| `webhook.port` | `9443` | HTTPS server port |
| `webhook.logLevel` | `debug` | Log level |
| `webhook.failurePolicy` | `Ignore` | `Ignore` or `Fail` |
| `webhook.excludedNamespaces` | `[]` | Extra namespaces to exclude from webhook interception (the release namespace, `kube-system`, and `kube-public` are always excluded) |
| `webhook.tlsSecretName` | `k8s-sustain-webhook-tls` | TLS secret name |
| `webhook.caBundle` | `""` | Base64-encoded CA cert (required when `certManager.enabled=false`) |
| `webhook.certManager.enabled` | `false` | Create a cert-manager `Certificate` resource |
| `webhook.certManager.createIssuer` | `true` | Create a self-signed `Issuer` in the release namespace. Set to `false` to use your own. |
| `webhook.certManager.issuerRef.name` | `""` | Issuer name (only used when `createIssuer=false`) |
| `webhook.certManager.issuerRef.kind` | `Issuer` | Issuer kind (only used when `createIssuer=false`) |
| `webhook.resources` | see below | Webhook container resources |
| `webhook.livenessProbe` | same as controller | Liveness probe timings. The probe endpoint (HTTPS `/healthz` on the webhook port) is fixed by the chart. |
| `webhook.readinessProbe` | same as controller | Readiness probe timings. Same fixed endpoint as the liveness probe. |
| `webhook.nodeSelector` | `{}` | Node selector |
| `webhook.tolerations` | `[]` | Tolerations |
| `webhook.affinity` | `{}` | Affinity rules |

**Default webhook resources:**

```yaml
webhook:
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      memory: 256Mi
```

---

## Dashboard

| Value | Default | Description |
|-------|---------|-------------|
| `dashboard.enabled` | `false` | Deploy the dashboard |
| `dashboard.replicaCount` | `1` | Dashboard replicas |
| `dashboard.bindAddress` | `:8090` | Server bind address (`:port` or `host:port`); the container port derives from its port part |
| `dashboard.logLevel` | `debug` | Log level |
| `dashboard.corsAllowedOrigins` | `[]` | Allowed CORS origins. Empty = same-origin only (the safe default). Set to `["https://your-grafana"]` to embed the dashboard cross-origin, or `["*"]` to allow all (not recommended). |
| `dashboard.service.type` | `ClusterIP` | Service type |
| `dashboard.service.port` | `8090` | Service port |
| `dashboard.resources` | see below | Dashboard container resources |
| `dashboard.livenessProbe` | same as controller | Liveness probe timings. The probe endpoint (`/healthz` on the http port) is fixed by the chart. |
| `dashboard.readinessProbe` | same as controller | Readiness probe timings. The probe endpoint (`/readyz` on the http port) is fixed by the chart. |
| `dashboard.nodeSelector` | `{}` | Node selector |
| `dashboard.tolerations` | `[]` | Tolerations |
| `dashboard.affinity` | `{}` | Affinity rules |

**Default dashboard resources:**

```yaml
dashboard:
  resources:
    requests:
      cpu: 10m
      memory: 128Mi
    limits:
      memory: 256Mi
```

---

## ServiceAccount

| Value | Default | Description |
|-------|---------|-------------|
| `serviceAccount.create` | `true` | Create the controller/webhook ServiceAccount. The dashboard's dedicated ServiceAccount is always created alongside the dashboard (`dashboard.enabled`) |
| `serviceAccount.name` | `""` | Override the ServiceAccount name |
| `serviceAccount.annotations` | `{}` | Annotations on the ServiceAccount (e.g. for IRSA or Workload Identity) |

---

## ServiceMonitor

Only needed when running the Prometheus Operator externally (not the bundled subchart).

| Value | Default | Description |
|-------|---------|-------------|
| `controller.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` for the controller metrics endpoint |
| `controller.serviceMonitor.interval` | `30s` | Scrape interval (controller) |
| `controller.serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout (controller) |
| `controller.serviceMonitor.additionalLabels` | `{}` | Extra labels added to the controller `ServiceMonitor`. Use to match a specific Prometheus operator's `serviceMonitorSelector` (e.g. `{release: kube-prometheus-stack}`). |
| `webhook.serviceMonitor.enabled` | `false` | Create a Prometheus Operator `ServiceMonitor` for the webhook (cert-expiry gauge etc.) |
| `webhook.serviceMonitor.interval` | `30s` | Scrape interval (webhook) |
| `webhook.serviceMonitor.scrapeTimeout` | `10s` | Scrape timeout (webhook) |
| `webhook.serviceMonitor.additionalLabels` | `{}` | Extra labels added to the webhook `ServiceMonitor`. Same purpose as the controller variant. |
| `prometheusRule.enabled` | `false` | Create a Prometheus Operator `PrometheusRule` holding the k8s-sustain recording rules. Leave disabled when using the bundled `prometheus` subchart — the same rules are already embedded in `prometheus.server.serverFiles`, and enabling both would duplicate the series. |
| `prometheusRule.additionalLabels` | `{}` | Extra labels added to the `PrometheusRule`. Use to match a specific Prometheus operator's `ruleSelector` in clusters with multiple Prometheus instances. |
| `prometheusRule.groups` | _(see values.yaml)_ | The recording-rule groups themselves. Anchored as `&recordingRulesGroups` so the bundled `prometheus` subchart's `serverFiles."recording_rules.yml".groups` aliases this exact list — edits flow to both consumers. The list is consumed regardless of `prometheusRule.enabled`; the toggle only gates the standalone `PrometheusRule` resource. |

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
