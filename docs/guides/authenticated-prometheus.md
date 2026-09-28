<!-- Source of truth: internal/prometheus/transport.go, internal/config/config.go, charts/k8s-sustain/values.yaml -->

# Authenticated Prometheus

k8s-sustain can reach a Prometheus that is behind authentication, a private CA,
or a multi-tenant query gateway (Thanos, Mimir, Cortex). Bearer tokens, HTTP
basic auth, arbitrary headers and TLS client settings are all configurable, on
the CLI, through environment variables, and through the chart's
`prometheusAuth` values block.

## Which components need it

| Component | Queries Prometheus | Needs `prometheusAuth` |
|-----------|--------------------|------------------------|
| Controller (`start`) | Yes — batched queries every reconcile | Yes |
| Dashboard (`dashboard`) | Yes — every metrics panel | Yes |
| Webhook (`webhook`) | **No** — it only reads cached `WorkloadRecommendation` objects | No |

The chart wires the controller and the dashboard **identically** from one
top-level `prometheusAuth` block, so the two can never disagree about which
Prometheus — or which tenant — a recommendation came from.

!!! note "`prometheusAuth`, not `prometheus`"
    The values block is top-level `prometheusAuth`. The `prometheus:` key
    belongs to the bundled [prometheus subchart](../reference/helm-values.md#prometheus-subchart)
    and configures that server, not k8s-sustain's client.

---

## Recipe 1 — Thanos / Mimir / Cortex multi-tenant gateway

A tenant-aware query gateway needs a bearer token and a tenant header
(`X-Scope-OrgID` for Mimir and Cortex; the header name varies across gateways,
which is why `headers` is a generic map).

```bash
kubectl create secret generic prometheus-credentials \
  --namespace k8s-sustain \
  --from-literal=token="$(cat ./mimir-token)"
```

```yaml
# values.yaml
prometheus:
  enabled: false                                            # no bundled Prometheus
prometheusAddress: https://mimir-gateway.mimir.svc/prometheus

prometheusAuth:
  existingSecret: prometheus-credentials
  bearerTokenKey: token
  headers:
    X-Scope-OrgID: tenant-a
```

```bash
helm upgrade --install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --version <VERSION> \
  --namespace k8s-sustain --create-namespace \
  -f values.yaml
```

This renders, on **both** the controller and the dashboard Deployment:

```yaml
args:
  - "--prometheus-bearer-token-file=/etc/k8s-sustain/prometheus-auth/token"
  - "--prometheus-headers=X-Scope-OrgID=tenant-a"
volumeMounts:
  - name: prometheus-auth
    mountPath: /etc/k8s-sustain/prometheus-auth
    readOnly: true
volumes:
  - name: prometheus-auth
    secret:
      secretName: prometheus-credentials
      defaultMode: 288        # 0440
      items:
        - key: token
          path: token
```

Only the keys you name are projected into the pod, so unrelated keys in the
same Secret are never exposed.

!!! tip "Header values are not secrets"
    `prometheusAuth.headers` values are rendered into the pod spec verbatim.
    Use them for tenant ids and routing hints — never for credentials. Keys are
    sorted before rendering, so the pod-template hash stays stable across
    `helm upgrade` runs.

---

## Recipe 2 — In-cluster Prometheus behind an auth proxy (projected token)

When Prometheus sits behind `kube-rbac-proxy`, `oauth2-proxy`, or an
authenticating Ingress that validates a Kubernetes service-account token with a
`TokenReview`, point k8s-sustain at the token the kubelet already projects into
every pod (`/var/run/secrets/kubernetes.io/serviceaccount/token`). The kubelet
rotates it in place roughly hourly and `--prometheus-bearer-token-file` is
re-read on every query, so rotation needs no restart and no Secret.

```yaml
# values.yaml
prometheus:
  enabled: false
prometheusAddress: https://prometheus-proxy.monitoring.svc:8443

prometheusAuth:
  # A path, not a Secret key: the chart renders the flag and mounts nothing.
  bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
  tls:
    existingSecret: prometheus-proxy-ca   # whatever signs the proxy's cert
    caKey: ca.crt

extraManifests:
  - apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: '{{ include "k8s-sustain.fullname" . }}-prometheus-reader'
    roleRef:
      apiGroup: rbac.authorization.k8s.io
      kind: ClusterRole
      name: prometheus-reader          # your proxy's ClusterRole
    subjects:
      - kind: ServiceAccount
        name: '{{ include "k8s-sustain.serviceAccountName" . }}'
        namespace: '{{ .Release.Namespace }}'
      - kind: ServiceAccount
        name: '{{ include "k8s-sustain.dashboardName" . }}'
        namespace: '{{ .Release.Namespace }}'
```

!!! warning "Two ServiceAccounts, two identities"
    The dashboard runs under its own ServiceAccount (`<fullname>-dashboard`),
    so its projected token authenticates as a *different* subject than the
    controller's. Authorize both, or the dashboard gets a `403` from the proxy
    while the controller works — which surfaces as "No metrics data available"
    with no error anywhere else.

`bearerTokenFile` is a **raw path**, as are `basicAuth.passwordFile`,
`tls.caFile`, `tls.certFile` and `tls.keyFile`. They render their flag
verbatim, create nothing, and are for files something *else* put in the pod —
the kubelet, a secret-store CSI driver, a service-mesh sidecar. See
[Rejected configurations](#rejected-configurations) for how they combine with
`*Key` values.

!!! tip "OpenShift in-cluster monitoring"
    OpenShift projects its service CA into the same directory, so the Thanos
    querier needs no Secret at all:

    ```yaml
    prometheusAddress: https://thanos-querier.openshift-monitoring.svc:9091
    prometheusAuth:
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      tls:
        caFile: /var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt
    ```

    Bind both ServiceAccounts to `cluster-monitoring-view` with
    `extraManifests` as above.

!!! note "A custom token `audience` needs a patch"
    The default projected token carries the API server's audience. A proxy
    that demands a specific audience needs a `serviceAccountToken` projected
    volume with `audience:`, which the chart cannot express (no
    `extraVolumes`) — add it with a kustomize / Argo CD patch on both
    Deployments and point `bearerTokenFile` at its mount path.

!!! note "Legacy `kubernetes.io/service-account-token` Secret"
    A token Secret populated by the token controller also works through
    `existingSecret` + `bearerTokenKey` (create it with `extraManifests`, the
    `kubernetes.io/service-account.name` annotation naming
    `{{ include "k8s-sustain.serviceAccountName" . }}`; the Secret's `ca.crt`
    can serve as `tls.caKey`). Both pods then authenticate as the
    controller's ServiceAccount. The token never expires or rotates — use it
    only when something outside the pod must read the same token.

---

## Recipe 3 — Basic auth behind a private CA

```bash
kubectl create secret generic prometheus-credentials \
  --namespace k8s-sustain \
  --from-literal=username=k8s-sustain \
  --from-file=password=./password.txt

kubectl create secret generic prometheus-ca \
  --namespace k8s-sustain \
  --from-file=ca.crt=./corp-root-ca.pem
```

```yaml
# values.yaml
prometheus:
  enabled: false
prometheusAddress: https://prometheus.corp.internal:9090

prometheusAuth:
  existingSecret: prometheus-credentials
  basicAuth:
    usernameKey: username             # injected via secretKeyRef (env var)
    passwordKey: password             # mounted as a file, re-read per request
  tls:
    existingSecret: prometheus-ca
    caKey: ca.crt
```

The username is not secret, so `basicAuth.username: k8s-sustain` in plain
values is fine too — `usernameKey` wins when both are set. There is no
username file; it is passed as a flag or, with `usernameKey`, a
`secretKeyRef` env var.

!!! info "The CA is appended, not substituted"
    `--prometheus-tls-ca-file` is **appended to a copy of the system trust
    pool**, so a publicly signed ingress on the same address keeps validating.

### Mutual TLS

Set the client key pair alongside (or instead of) the CA; cert and key must be
set together:

```yaml
prometheusAuth:
  tls:
    existingSecret: prometheus-client-tls
    caKey: ca.crt
    certKey: tls.crt
    keyKey: tls.key
```

These are mounted at `/etc/k8s-sustain/prometheus-tls/<key>`, a separate mount
from the credentials (it may point at the same Secret).

### When the certificate name does not match the address

```yaml
prometheusAuth:
  tls:
    existingSecret: prometheus-ca
    caKey: ca.crt
    serverName: prometheus.corp.internal   # SNI / verified hostname override
```

Reach for `serverName` before `insecureSkipVerify`.

---

## Environment variables — the prefixes differ per subcommand

Each subcommand binds its flags under its own Viper key prefix, so the same
flag name maps to a **different environment variable** per subcommand:

| Subcommand | Prefix | Example |
|---|---|---|
| Controller (`start`) | `K8SSUSTAIN_` | `K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN_FILE` |
| Webhook (`webhook`) | `K8SSUSTAIN_WEBHOOK_` | `K8SSUSTAIN_WEBHOOK_LOG_LEVEL` |
| Dashboard (`dashboard`) | `K8SSUSTAIN_DASHBOARD_` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_BEARER_TOKEN_FILE` |

The global `--recommend-only` flag is unprefixed (`K8SSUSTAIN_RECOMMEND_ONLY`)
on every subcommand; `--config` has no environment variable.

!!! danger "A wrong prefix is silently ignored"
    `K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN_FILE` set on the dashboard does
    nothing: the dashboard starts, queries Prometheus unauthenticated, and
    reports "No metrics data available" while the controller works fine.
    There is no error message. The same applies to shared flags such as
    `--log-level` and `--excluded-namespaces` on the webhook.

The Prometheus settings (the webhook has none):

| Setting | Controller | Dashboard |
|---------|------------|-----------|
| Bearer token | `K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_BEARER_TOKEN` |
| Bearer token file | `K8SSUSTAIN_PROMETHEUS_BEARER_TOKEN_FILE` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_BEARER_TOKEN_FILE` |
| Basic auth username | `K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_USERNAME` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_BASIC_AUTH_USERNAME` |
| Basic auth password | `K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_PASSWORD` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_BASIC_AUTH_PASSWORD` |
| Basic auth password file | `K8SSUSTAIN_PROMETHEUS_BASIC_AUTH_PASSWORD_FILE` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_BASIC_AUTH_PASSWORD_FILE` |
| Headers | `K8SSUSTAIN_PROMETHEUS_HEADERS` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_HEADERS` |
| TLS CA file | `K8SSUSTAIN_PROMETHEUS_TLS_CA_FILE` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_TLS_CA_FILE` |
| TLS cert file | `K8SSUSTAIN_PROMETHEUS_TLS_CERT_FILE` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_TLS_CERT_FILE` |
| TLS key file | `K8SSUSTAIN_PROMETHEUS_TLS_KEY_FILE` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_TLS_KEY_FILE` |
| TLS server name | `K8SSUSTAIN_PROMETHEUS_TLS_SERVER_NAME` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_TLS_SERVER_NAME` |
| TLS insecure skip verify | `K8SSUSTAIN_PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | `K8SSUSTAIN_DASHBOARD_PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` |

The chart renders the right prefix for each component; this table matters when
you set environment variables yourself (raw manifests, a kustomize patch,
`docker run`, local development).

---

## Credential sources and security

| | Inline (`--prometheus-bearer-token`, `--prometheus-basic-auth-password`) | File (`--prometheus-bearer-token-file`, `--prometheus-basic-auth-password-file`) |
|---|---|---|
| Read | Once, at startup | **On every request** |
| Credential rotation | Requires a pod restart | Picked up automatically |
| Where the secret lives | In the pod spec, `kubectl get deploy -o yaml`, `helm get values`, your values repo | Only in the Secret |

**Prefer the file form** (`existingSecret` + `*Key`, or a raw `*File` path):

- A projected service-account token rotates roughly hourly; read once at
  startup it starts returning `401` after the first rotation.
- Updating the backing Secret (external-secrets rotation, a manual
  `kubectl apply`) takes effect on the next query, with no rollout.
- The mTLS key pair is re-read on every TLS handshake, so a certificate renewed
  in place by cert-manager or a mesh sidecar is used without a restart.
- Inline values exist as an escape hatch for quick tests. The chart passes them
  as environment variables, not arguments, which keeps them out of `argv` —
  a mitigation, not a fix.
- Mounted credential files use mode `0440`, read-only; the pod's `fsGroup`
  (65532) is the only identity that can read them.
- **Credentials are never logged**, at any log level. Startup errors name the
  *path* that failed, never the contents.
- Create the Secret out of band — external-secrets, sealed-secrets, a secret
  store CSI driver — or through
  [`extraManifests`](../reference/helm-values.md#extra-manifests).

Files are also read once at startup as a validation step: a missing path, a
malformed key pair, or a CA file with no valid PEM block **fails the process
at startup** (`CrashLoopBackOff`) rather than degrading into per-query errors
that look like a Prometheus outage.

---

## Rejected configurations

The binary rejects these at construction time, so it fails fast rather than
querying with the wrong (or no) credential:

| Combination | Why |
|-------------|-----|
| Bearer token **and** bearer token file | Ambiguous source |
| Basic auth password **and** password file | Ambiguous source |
| Bearer token (or file) **and** basic auth | Two competing `Authorization` schemes |
| Password (or password file) without a username | Basic auth needs both |
| An `Authorization` entry in `--prometheus-headers` together with bearer/basic auth | The header would be overwritten by the auth layer |
| TLS cert file without key file (or vice versa) | A key pair needs both halves |

The chart checks the same rules in `helm template` / `helm upgrade`, on the
effective sources (a `*Key` only counts when its Secret is named). Between chart
values:

- A Secret-backed `*Key` **wins** over the inline value for the same credential;
  only the file is rendered.
- A raw `*File` path is **exclusive** with its `*Key` and, for the token and the
  password, with the inline value. Both would drive the same flag, so the
  render fails with "`… are mutually exclusive`", naming both values. A raw path
  must also be absolute.

A malformed `--prometheus-headers` entry (no `=`, an empty key, a name that is
not a valid HTTP token, or a value with control characters) is reported with
the offending entry at startup. Only the **first** `=` splits an entry, so
values may contain `=` (a base64 tenant id). The flag is **repeatable**, one
header per occurrence, so a value may also contain a comma; the chart renders
one flag per `prometheusAuth.headers` entry. The environment-variable form is a
single comma-separated string and cannot express such a value.

!!! warning "`insecureSkipVerify`"
    `prometheusAuth.tls.insecureSkipVerify` / `--prometheus-tls-insecure-skip-verify`
    disables server-certificate verification entirely, which makes every
    credential above interceptable. It is honoured, but logs a loud warning at
    startup. Use `serverName` or a `caKey` instead.

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| `CrashLoopBackOff` with `reading prometheus bearer token file …: no such file or directory` | With `bearerTokenKey`: the key does not exist in `existingSecret`, or the Secret is in another namespace. With `bearerTokenFile`: the path does not exist in the pod — it mounts nothing |
| `contains no valid PEM certificate` at startup | `tls.caKey` points at a key holding something other than a PEM bundle |
| Controller works, dashboard shows "No metrics data available" | Dashboard credentials missing: a `K8SSUSTAIN_PROMETHEUS_*` variable that should be `K8SSUSTAIN_DASHBOARD_PROMETHEUS_*`, a patch applied to only one Deployment, or (projected token) the dashboard ServiceAccount not authorized |
| Queries return `401` about an hour after startup | A rotating token was supplied inline — switch to `bearerTokenFile` or `bearerTokenKey` |
| `helm upgrade` fails with "`… are mutually exclusive`" | A `*File` path set alongside its `*Key` or inline value, or a bearer token combined with basic auth. Keep exactly one |
| `helm upgrade` fails with "`must be a string`" or "`Invalid type. Expected: string`" on a header | The header value is an unquoted number; write it as `"1234567"` |
| Startup log shows `system certificate pool unavailable` | The image's trust store could not be read, so the configured CA is the only root; a public-CA ingress on the same address fails verification |
| Multi-tenant gateway returns `no org id` / empty results | `prometheusAuth.headers` missing, or the gateway expects a header other than `X-Scope-OrgID` |

See also the [CLI reference](../reference/cli.md) for the full flag list and
the [Helm values reference](../reference/helm-values.md#prometheus-authentication)
for the complete `prometheusAuth` schema.
