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

## Prometheus authentication

Authentication and TLS for the Prometheus connection. One top-level block wires
the **controller** and the **dashboard** identically, so the two can never
disagree about which Prometheus — or which tenant — a recommendation came from.
The webhook is deliberately not wired: it never queries Prometheus, it only
reads cached `WorkloadRecommendation` objects.

See the [Authenticated Prometheus guide](../guides/authenticated-prometheus.md)
for worked examples (Thanos/Mimir tenant gateway, service-account token behind
an auth proxy, basic auth with a private CA).

!!! note "`prometheusAuth` is not `prometheus`"
    This block configures k8s-sustain's *client*. The separate top-level
    `prometheus:` key configures the [bundled Prometheus subchart](#prometheus-subchart).

| Value | Default | Description |
|-------|---------|-------------|
| `prometheusAuth.existingSecret` | `""` | Name of an existing Secret in the release namespace holding the bearer token and/or basic-auth credentials. Required for any of the `*Key` values below to take effect. |
| `prometheusAuth.bearerTokenKey` | `""` | Key in `existingSecret` holding the bearer token. Mounted at `/etc/k8s-sustain/prometheus-auth/<key>` and passed as `--prometheus-bearer-token-file`. |
| `prometheusAuth.bearerTokenFile` | `""` | Absolute path to a bearer token file that **already exists in the pod**, passed verbatim as `--prometheus-bearer-token-file`. No volume or mount is rendered. Canonical value: `/var/run/secrets/kubernetes.io/serviceaccount/token`, the kubelet's rotating projected token. Mutually exclusive with `bearerTokenKey` and `bearerToken` — setting either alongside it fails the template. |
| `prometheusAuth.bearerToken` | `""` | Inline bearer token. **Discouraged** — rendered verbatim into the pod spec. Ignored when `existingSecret` + `bearerTokenKey` are set. |
| `prometheusAuth.basicAuth.username` | `""` | Basic-auth username, passed as `--prometheus-basic-auth-username`. Not secret material, so a plain value is fine. |
| `prometheusAuth.basicAuth.usernameKey` | `""` | Key in `existingSecret` holding the username, injected with `valueFrom.secretKeyRef` (there is no `--prometheus-basic-auth-username-file` flag). Wins over `username` when both are set. |
| `prometheusAuth.basicAuth.passwordKey` | `""` | Key in `existingSecret` holding the password. Mounted at `/etc/k8s-sustain/prometheus-auth/<key>` and passed as `--prometheus-basic-auth-password-file`. |
| `prometheusAuth.basicAuth.passwordFile` | `""` | Absolute path to a password file that **already exists in the pod** (a secret-store CSI mount, say), passed verbatim as `--prometheus-basic-auth-password-file`. No volume is rendered. Mutually exclusive with `passwordKey` and `password`. |
| `prometheusAuth.basicAuth.password` | `""` | Inline password. **Discouraged** — rendered verbatim into the pod spec. Ignored when `existingSecret` + `passwordKey` are set. |
| `prometheusAuth.headers` | `{}` | Extra HTTP headers sent with every query, rendered as one `--prometheus-headers=Key=Value` flag per entry, so a value may contain commas or quotes. Header **values land in the pod spec** — use for tenant ids, not credentials. Values **must be strings**: an unquoted number such as `1234567` is parsed as a float and rejected by the values schema (write `"1234567"`). Example: `{X-Scope-OrgID: tenant-a}`. Keys are sorted so the rendered flags and the pod-template hash stay stable. |
| `prometheusAuth.tls.existingSecret` | `""` | Secret holding the CA bundle and/or client key pair. May be the same Secret as `prometheusAuth.existingSecret`; it is mounted separately, at `/etc/k8s-sustain/prometheus-tls`. |
| `prometheusAuth.tls.caKey` | `""` | Key holding the CA bundle that signs the Prometheus server certificate (`--prometheus-tls-ca-file`). Appended to the system trust store, never substituted for it. |
| `prometheusAuth.tls.caFile` | `""` | Absolute path to a CA bundle that **already exists in the pod**, passed verbatim as `--prometheus-tls-ca-file`. No volume is rendered. On OpenShift, `/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt` is the service CA that signs the in-cluster monitoring endpoint. Mutually exclusive with `caKey`. |
| `prometheusAuth.tls.certKey` | `""` | Key holding the client certificate for mTLS (`--prometheus-tls-cert-file`). Must be set with `keyKey`. The pair is re-read on every TLS handshake, so a renewed certificate in the Secret is used without a restart. |
| `prometheusAuth.tls.certFile` | `""` | Absolute path to a client certificate that **already exists in the pod** (a service-mesh sidecar's key pair, say), passed verbatim as `--prometheus-tls-cert-file`. No volume is rendered. Mutually exclusive with `certKey`; still needs a key half (`keyFile` or `keyKey`). |
| `prometheusAuth.tls.keyKey` | `""` | Key holding the client private key for mTLS (`--prometheus-tls-key-file`). Must be set with `certKey`. |
| `prometheusAuth.tls.keyFile` | `""` | Absolute path to a client private key that **already exists in the pod**, passed verbatim as `--prometheus-tls-key-file`. No volume is rendered. Mutually exclusive with `keyKey`. |
| `prometheusAuth.tls.serverName` | `""` | Overrides the SNI / certificate name verified against the server certificate (`--prometheus-tls-server-name`). |
| `prometheusAuth.tls.insecureSkipVerify` | `false` | Disable server-certificate verification (`--prometheus-tls-insecure-skip-verify`). Debug only — it makes every credential above interceptable. Logs a loud warning at startup. |

### How the values are rendered

- **Secret-backed keys become read-only file mounts**, projected with
  `defaultMode: 288` (`0440`) and with an explicit `items:` list, so only the
  keys you name are exposed — an unrelated key in the same Secret never reaches
  the container. Credentials at `/etc/k8s-sustain/prometheus-auth/<key>`, TLS
  material at `/etc/k8s-sustain/prometheus-tls/<key>`.
- **A secret-backed file always wins** over its inline counterpart. Setting
  both is not an error; the inline value simply is not rendered.
- **Inline values become environment variables**, not arguments — so they stay
  out of the container's `argv` — with the per-component prefix the binary
  expects: `K8SSUSTAIN_PROMETHEUS_*` on the controller,
  `K8SSUSTAIN_DASHBOARD_PROMETHEUS_*` on the dashboard.
- Naming a `*Key` without the matching `existingSecret` renders **nothing** —
  no flag, no volume, no mount.
- **A `*File` value is a raw path, not a source the chart creates.** It renders
  the flag verbatim and adds **no volume and no volumeMount** — use it for a
  file the pod already carries (the kubelet's projected service-account token,
  a secret-store CSI mount, a mesh sidecar's key pair). The path must be
  absolute, or the template fails.
- **A `*File` and its `*Key` are mutually exclusive and fail the template**
  when both are set, rather than being ranked by precedence: both drive the
  *same* flag with different values, so the rendered Deployment would not show
  which source was meant. `bearerTokenFile` / `basicAuth.passwordFile` conflict
  with their inline counterparts too — the binary rejects an inline credential
  together with its file at construction, so rendering both would only defer
  the crash to startup. The error names both values.
- **The binary's own cross-credential rules are checked at template time too**,
  on the *effective* sources (a `*Key` only counts when its Secret is named):
  a bearer token together with basic auth, a password without a username, and
  a client certificate without its key (or vice versa) all fail the template
  instead of surfacing as a `CrashLoopBackOff` after a green `helm upgrade`.
- **Every arg carrying a user value is quoted**, so a header value or username
  containing a colon followed by a space, a space followed by `#`, or a `"`
  renders as the exact string, instead of turning the args entry into a YAML
  mapping or being truncated at a comment marker.

!!! warning "Prefer `existingSecret` over inline values"
    An inline `bearerToken` or `basicAuth.password` is readable through
    `kubectl get deploy -o yaml`, `helm get values`, and whatever git
    repository holds the values file. The file form additionally re-reads the
    credential on **every request**, so a rotated Secret — or a kubelet-rotated
    service-account token — is picked up with no pod restart. Create the Secret
    with external-secrets, sealed-secrets, a secret-store CSI driver, or this
    chart's [`extraManifests`](#extra-manifests).

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
