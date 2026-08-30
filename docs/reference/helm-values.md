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
| `excludedNamespaces` | `[]` | Extra namespaces to exclude from k8s-sustain entirely — the controller never recycles pods there and the webhook never mutates them (the release namespace, `kube-system`, and `kube-public` are always excluded) |

---

## Controller

| Value | Default | Description |
|-------|---------|-------------|
| `controller.replicaCount` | `1` | Controller replicas |
| `controller.metricsBindAddress` | `:8080` | Metrics endpoint address (`:port` or `host:port`); the metrics container port derives from its port part |
| `controller.healthProbeBindAddress` | `:8081` | Health probe address (`:port` or `host:port`); the health container port derives from its port part |
| `controller.leaderElect` | `true` | Enable leader election |
| `controller.reconcileInterval` | `5m` | How often each matched Policy is re-evaluated (Prometheus re-queried, recommendations refreshed, stale pods recycled) |
| `controller.workloadConcurrencyLimit` | `5` | Maximum number of workloads processed in parallel per reconcile cycle |
| `controller.policyConcurrencyLimit` | `10` | Maximum number of Policy objects reconciled in parallel |
| `controller.recycleReplacementTimeout` | `5m` | In the eviction-fallback recycle path, how long to wait for a replacement pod to become Ready before aborting the loop. Increase on clusters where Karpenter / cluster-autoscaler node provisioning regularly takes longer. |
| `controller.recommendationRetention` | `168h` | How long a WorkloadRecommendation outlives a workload whose object has disappeared (ephemeral bare pods, argocd-hook Jobs, TTL-deleted Jobs). Not just a dashboard setting: it also decides whether a *recurring* ephemeral identity is rightsized at admission on its next run, so set it above the longest expected gap between runs (`168h` covers weekly batch). See [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads). The dashboard keeps showing retained entries as "inactive" rows until the window lapses. Set to `0s` to sweep them on the next reconcile instead. |
| `controller.prometheusMaxInflight` | `8` | Maximum concurrent Prometheus queries across the whole controller. Kept below Prometheus's own `--query.max-concurrency` (default 20) so k8s-sustain cannot starve the dashboards and alerting sharing that server. Lower this first if the controller is the reason your Prometheus is struggling. |
| `controller.queryShardMaxSamples` | `10000000` | Projected sample budget (containers × window-minutes, summed across a shard's workloads) one batched CPU/memory/OOM query may reach before a new shard is started. Must stay under Prometheus's `--query.max-samples` (default `50000000`): that limit *rejects* an over-budget query outright, failing every workload in the shard rather than just the excess. The default leaves a 5x margin. |
| `controller.logLevel` | `error` | Log level |
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
| `webhook.logLevel` | `error` | Log level |
| `webhook.failurePolicy` | `Ignore` | `Ignore` or `Fail` |
| `webhook.tlsSecretName` | `k8s-sustain-webhook-tls` | TLS secret name |
| `webhook.caBundle` | `""` | Base64-encoded CA cert (required when `certManager.enabled=false`) |
| `webhook.certManager.enabled` | `false` | Create a cert-manager `Certificate` resource |
| `webhook.certManager.createIssuer` | `true` | Create a self-signed `Issuer` in the release namespace. Set to `false` to use your own. |
| `webhook.certManager.issuerRef.name` | `""` | Issuer name (only used when `createIssuer=false`) |
| `webhook.certManager.issuerRef.kind` | `Issuer` | Issuer kind (only used when `createIssuer=false`) |
| `webhook.resources` | see below | Webhook container resources |
| `webhook.startupProbe` | see below | Startup probe timings. Suspends liveness and readiness until the webhook is actually listening. Same fixed endpoint. |
| `webhook.livenessProbe` | same as controller | Liveness probe timings. The probe endpoint (HTTPS `/healthz` on the webhook port) is fixed by the chart. |
| `webhook.readinessProbe` | same as controller | Readiness probe timings. Same fixed endpoint as the liveness probe. |
| `webhook.nodeSelector` | `{}` | Node selector |
| `webhook.tolerations` | `[]` | Tolerations |
| `webhook.affinity` | `{}` | Affinity rules |

**The webhook's startup probe is not optional.** The webhook builds an informer
cache before its HTTPS listener starts, and that build waits up to two minutes
for the `Policy` and `WorkloadRecommendation` CRDs to become servable — the
fresh-install race where Helm has created the CRDs but the API server is not
serving them yet. Nothing answers `/healthz` for that whole period, so the
liveness probe alone would kill the container after
`initialDelaySeconds + periodSeconds × failureThreshold` = 40s and the pod would
end in `CrashLoopBackOff` on exactly the install the wait exists to survive.

The startup probe's budget must therefore stay **larger than that two-minute
wait** (`crdWaitTimeout` in `internal/k8s/client.go`):

```yaml
webhook:
  startupProbe:
    initialDelaySeconds: 5
    periodSeconds: 5      # 5 + 5 × 36 = 185s > 120s
    timeoutSeconds: 1
    successThreshold: 1
    failureThreshold: 36
```

If you shorten it, shorten it to something still comfortably above two minutes.
A Go unit test reads this value out of `values.yaml` and fails if the two sides
drift apart.

The template carries the same numbers as its own defaults, so the probe is
rendered even when the release's values do not contain `webhook.startupProbe`
at all — which is what a `helm upgrade --reuse-values` from a release predating
this key produces, since `--reuse-values` never picks up defaults newly added
to `values.yaml`. Overriding one field keeps the template defaults for the
others. A second Go unit test reads that template copy and checks it against
`crdWaitTimeout` in its own right, and fails if the two copies disagree — so
raising the wait cannot leave the `--reuse-values` path silently under budget
while the `values.yaml` side still passes.

**Default webhook resources:**

```yaml
webhook:
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      memory: 512Mi
```

The webhook's memory defaults are higher than the controller's on purpose. It serves admission reads from a `WorkloadRecommendation` informer cache rather than a per-pod apiserver `Get` — with Prometheus gone from the admission path, the apiserver is the only remaining source of latency there, and an uncached read per admission is a round trip a large cluster does not need. That cache is resident memory: budget roughly 20–50 MB at 10k workloads on top of the base footprint. Raise these if you run substantially more.

---

## Dashboard

| Value | Default | Description |
|-------|---------|-------------|
| `dashboard.enabled` | `true` | Deploy the dashboard |
| `dashboard.replicaCount` | `1` | Dashboard replicas |
| `dashboard.bindAddress` | `:8090` | Server bind address (`:port` or `host:port`); the container port derives from its port part |
| `dashboard.logLevel` | `error` | Log level |
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
| `prometheusRule.groups` | *(see values.yaml)* | The recording-rule groups themselves. Anchored as `&recordingRulesGroups` so the bundled `prometheus` subchart's `serverFiles."recording_rules.yml".groups` aliases this exact list — edits flow to both consumers. The list is consumed regardless of `prometheusRule.enabled`; the toggle only gates the standalone `PrometheusRule` resource. |

---

## CRDs

| Value | Default | Description |
|-------|---------|-------------|
| `installCRDs` | `true` | Install the `Policy` CRD as part of the chart |

---

## Extra manifests

| Value | Default | Description |
|-------|---------|-------------|
| `extraManifests` | `[]` | Arbitrary extra objects rendered with the release, for anything the chart does not template itself |

Use it for objects that belong to the release but have no dedicated chart value — a
`Secret` holding Prometheus credentials, a `NetworkPolicy`, a Grafana dashboard
`ConfigMap`, an `ExternalSecret`, an extra `RoleBinding`.

Each entry is either a YAML map or a multi-line string, and both are passed through
`tpl`, so entries may use template expressions (`.Release.Namespace`, `.Values.*`, the
chart's `k8s-sustain.fullname` helper, ...):

```yaml
extraManifests:
  # Map form — rendered verbatim; quote any value containing a template expression.
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: '{{ include "k8s-sustain.fullname" . }}-grafana-dashboard'
      namespace: '{{ .Release.Namespace }}'
      labels:
        grafana_dashboard: "1"
    data:
      k8s-sustain.json: |
        {"title": "k8s-sustain"}

  # String form — handy for manifests you paste in as-is.
  - |
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    metadata:
      name: {{ include "k8s-sustain.fullname" . }}-allow-webhook
      namespace: {{ .Release.Namespace }}
    spec:
      podSelector:
        matchLabels:
          app.kubernetes.io/name: {{ include "k8s-sustain.name" . }}
      ingress:
        - {}
```

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
