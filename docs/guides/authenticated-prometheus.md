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
| Controller (`start`) | Yes — three queries per reconcile shard | Yes |
| Dashboard (`dashboard`) | Yes — every metrics panel | Yes |
| Webhook (`webhook`) | **No** — it only reads cached `WorkloadRecommendation` objects | No |

The chart wires the controller and the dashboard **identically** from one
top-level `prometheusAuth` block, so the two can never disagree about which
Prometheus — or which tenant — a recommendation came from. The webhook is
deliberately not wired: it has no Prometheus client at all.

!!! note "`prometheusAuth`, not `prometheus`"
    The values block is top-level `prometheusAuth`. The `prometheus:` key
    belongs to the bundled [prometheus subchart](../reference/helm-values.md#prometheus-subchart)
    and configures that server, not k8s-sustain's client.

---

## Files vs inline values

Every credential can be supplied two ways, and the difference matters
operationally:

| | Inline (`--prometheus-bearer-token`, `--prometheus-basic-auth-password`) | File (`--prometheus-bearer-token-file`, `--prometheus-basic-auth-password-file`) |
|---|---|---|
| Read | Once, at startup | **On every request** |
| Credential rotation | Requires a pod restart | Picked up automatically, no restart |
| Where the secret lives | In the pod spec and in `helm get values` | Only in the Secret |

The mTLS key pair (`tls.certFile`/`keyFile`, or `certKey`/`keyKey`) behaves
like the file column: it is re-read on every TLS handshake, so a certificate
renewed in place by cert-manager or a mesh sidecar is used without a restart.

**Prefer the file form.** Two consequences follow from the per-request re-read:

- A Kubernetes projected service-account token is rotated in place by the
  kubelet roughly hourly. A token read once at startup starts returning `401`
  after the first rotation; a token file keeps working indefinitely.
- Updating the backing Secret (external-secrets rotation, a manual
  `kubectl create secret ... --dry-run | kubectl apply`) takes effect on the
  next query, with no rollout.

Files are *also* read once at construction time, purely as a validation step:
a missing or unreadable path, a malformed key pair, or a CA file with no valid
PEM block **fails the process at startup** (visible as `CrashLoopBackOff`)
rather than degrading into per-query errors that look like a Prometheus outage.

---

## Recipe 1 — Thanos / Mimir / Cortex multi-tenant gateway

A tenant-aware query gateway needs two things: a bearer token, and a tenant
header (`X-Scope-OrgID` for Mimir and Cortex; the header name varies across
gateways, which is why it is a generic `headers` map rather than a fixed
field).

Create the Secret holding the token:

```bash
kubectl create secret generic prometheus-credentials \
  --namespace k8s-sustain \
  --from-literal=token="$(cat ./mimir-token)"
```

Then:

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

Only the keys you actually name are projected into the pod, so unrelated keys
living in the same Secret are never exposed to the container.

!!! tip "Header values are not secrets"
    `prometheusAuth.headers` values are rendered into the pod spec verbatim.
    Use them for tenant ids and routing hints — never for credentials. Keys are
    sorted before rendering, so the flag and the pod-template hash stay stable
    across `helm upgrade` runs instead of flapping with map iteration order.

---

## Recipe 2 — In-cluster Prometheus behind an auth proxy (projected token)

When Prometheus sits behind `kube-rbac-proxy`, `oauth2-proxy`, or an
authenticating Ingress that validates a Kubernetes service-account token with a
`TokenReview`, point k8s-sustain at the token the kubelet **already projects
into every pod**:

```text
/var/run/secrets/kubernetes.io/serviceaccount/token
```

The chart never sets `automountServiceAccountToken: false`, so the controller
and the dashboard both carry that file. It is a *projected* token: the kubelet
rewrites it in place roughly hourly, and `--prometheus-bearer-token-file` is
re-read on every query — so rotation needs no restart, no `helm upgrade`, and
no Secret to manage.

```yaml
# values.yaml
prometheus:
  enabled: false
prometheusAddress: https://prometheus-proxy.monitoring.svc:8443

prometheusAuth:
  # A path, not a Secret key: the file is already in the pod, so the chart
  # renders the flag and nothing else — no volume, no volumeMount.
  bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
  tls:
    existingSecret: prometheus-proxy-ca   # whatever signs the proxy's cert
    caKey: ca.crt

extraManifests:
  # What kube-rbac-proxy typically authorizes against.
  - apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: '{{ include "k8s-sustain.fullname" . }}-prometheus-reader'
    roleRef:
      apiGroup: rbac.authorization.k8s.io
      kind: ClusterRole
      name: prometheus-reader          # your proxy's ClusterRole
    subjects:
      # BOTH identities: the controller and the dashboard run under separate
      # ServiceAccounts, so each pod projects a token for a different subject.
      - kind: ServiceAccount
        name: '{{ include "k8s-sustain.serviceAccountName" . }}'
        namespace: '{{ .Release.Namespace }}'
      - kind: ServiceAccount
        name: '{{ include "k8s-sustain.dashboardName" . }}'
        namespace: '{{ .Release.Namespace }}'
```

!!! warning "Two ServiceAccounts, two identities"
    The dashboard has its own ServiceAccount (`<release>-dashboard`), so its
    projected token authenticates as a *different* subject than the
    controller's. Authorize both, or the dashboard gets a `403` from the proxy
    while the controller works — which surfaces as "No metrics data available"
    with no error anywhere else.

This renders, on **both** Deployments:

```yaml
args:
  - --prometheus-bearer-token-file=/var/run/secrets/kubernetes.io/serviceaccount/token
```

and adds no volume for it — the kubelet owns that file.

!!! tip "OpenShift in-cluster monitoring"
    OpenShift projects its service CA into the same directory, so the Thanos
    querier in front of the cluster's Prometheus needs no Secret at all:

    ```yaml
    prometheusAddress: https://thanos-querier.openshift-monitoring.svc:9091
    prometheusAuth:
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      tls:
        caFile: /var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt
    ```

    Bind the release's ServiceAccount to `cluster-monitoring-view` with
    `extraManifests` as above.

`bearerTokenFile` is a **raw path**, and every credential and TLS setting has
one: `basicAuth.passwordFile`, `tls.caFile`, `tls.certFile`, `tls.keyFile`.
They render their flag verbatim, create nothing, and are for files something
*else* put in the pod — the kubelet, a secret-store CSI driver, a service-mesh
sidecar. Each is mutually exclusive with its `*Key` counterpart (and, for the
token and the password, with the inline value): setting both **fails the
template** naming the two values, because both would drive the same flag and
the rendered Deployment could no longer say which source was meant.

!!! note "A custom token `audience` still needs a patch"
    The projected token in the default location carries the API server's own
    audience. A proxy that demands a *specific* audience needs a
    `serviceAccountToken` projected volume with an `audience:` field, which
    this chart cannot express (it has no `extraVolumes`) — that case still
    needs a kustomize / Argo CD patch on both Deployments, adding the volume
    and pointing `bearerTokenFile` at its mount path.

### Legacy variant — a `kubernetes.io/service-account-token` Secret

Before projected tokens, the way to get a token file was to have the token
controller populate a Secret and mount it. The chart still supports it through
`existingSecret` + `bearerTokenKey`:

```yaml
prometheusAuth:
  existingSecret: k8s-sustain-prometheus-token
  bearerTokenKey: token
  tls:
    existingSecret: k8s-sustain-prometheus-token
    caKey: ca.crt                     # the Secret also carries the cluster CA

extraManifests:
  - apiVersion: v1
    kind: Secret
    type: kubernetes.io/service-account-token
    metadata:
      name: k8s-sustain-prometheus-token
      namespace: '{{ .Release.Namespace }}'
      annotations:
        kubernetes.io/service-account.name: '{{ include "k8s-sustain.serviceAccountName" . }}'
```

A Secret-backed token is mounted into both pods, so both authenticate as the
**controller's** ServiceAccount — one identity to authorize instead of two.
That is the only way in which this form is simpler.

!!! warning "Long-lived and non-rotating"
    This token never expires and never rotates; it is a standing credential
    sitting in etcd, and revoking it means deleting the Secret. Reach for it
    only when something **outside** the pod must read the same token — an
    external system or a proxy configuration you have to paste it into.
    Otherwise use the projected token above.

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

The username is not secret material, so `basicAuth.username: k8s-sustain` in
plain values is fine too — `usernameKey` wins when both are set. There is no
`--prometheus-basic-auth-username-file`; the username is passed as a flag (or,
with `usernameKey`, as a `valueFrom.secretKeyRef` env var), never mounted.

!!! info "The CA is appended, not substituted"
    `prometheusAuth.tls.caKey` renders `--prometheus-tls-ca-file`, whose PEM
    bundle is **appended to a copy of the system trust pool**. A publicly
    signed ingress on the same address keeps validating; adding a private CA
    never revokes trust in a certificate the host already accepts.

### Mutual TLS

Set the client key pair alongside (or instead of) the CA. Both must be set
together — a cert without a key is rejected at startup:

```yaml
prometheusAuth:
  tls:
    existingSecret: prometheus-client-tls
    caKey: ca.crt
    certKey: tls.crt
    keyKey: tls.key
```

These are mounted at `/etc/k8s-sustain/prometheus-tls/<key>` — a separate mount
from the credentials, though it may point at the same Secret.

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

!!! danger "The controller and the dashboard read *different* environment variables"
    The controller binds these flags at the top level and reads
    **`K8SSUSTAIN_PROMETHEUS_*`**. The dashboard binds every one of its flags
    under the `dashboard.` Viper key prefix — like `--bind-address` and
    `--log-level` — so it reads **`K8SSUSTAIN_DASHBOARD_PROMETHEUS_*`**.

    An unprefixed variable on the dashboard is **silently ignored**: the
    dashboard starts, queries Prometheus unauthenticated, and reports "no
    metrics data available" while the controller works fine. There is no error
    message for this.

| Setting | Controller (`start`) | Dashboard (`dashboard`) |
|---------|----------------------|-------------------------|
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

The **command-line flag names are identical** on both subcommands — only the
environment variables differ. The chart handles the prefix for you; this table
matters when you set environment variables yourself (raw manifests, a kustomize
patch, `docker run`, local development).

---

## Security guidance

- **Prefer `existingSecret` file mounts over inline values.** An inline
  `prometheusAuth.bearerToken` or `prometheusAuth.basicAuth.password` is
  rendered verbatim into the Deployment and is therefore readable through
  `kubectl get deploy -o yaml`, `helm get values`, and whatever git repository
  holds your values file. The inline values exist as an escape hatch for quick
  tests; treat them as such.
- Inline values are passed as **environment variables, not arguments**, so at
  least they stay out of the container's `argv` (and out of `ps` output and
  crash dumps that capture it). That is a mitigation, not a fix.
- A secret-backed file **always wins** over its inline counterpart. Setting
  both does not conflict — the file is used and the inline value is not
  rendered at all.
- A **raw `*File` path is not ranked, it is exclusive**: setting it together
  with the matching `*Key` (or, for the token and the password, with the inline
  value) fails `helm template` / `helm upgrade` with both value names in the
  message. The two would drive one flag from two sources, and nothing in the
  rendered Deployment would say which one was meant.
- Mounted credential files are projected with mode `0440` (`defaultMode: 288`)
  and mounted read-only; the pod's `fsGroup` (65532) is the only identity that
  can read them.
- **Credentials are never logged**, at any log level. Startup errors name the
  *path* that failed, never the contents.
- Create the Secret out of band — external-secrets, sealed-secrets, a cloud
  secret store CSI driver — or through
  [`extraManifests`](../reference/helm-values.md#extra-manifests) as shown
  above.

---

## Rejected configurations

These are rejected at construction time, so the process fails fast at startup
rather than resolving a conflict silently and querying with the wrong (or no)
credential:

| Combination | Why |
|-------------|-----|
| Bearer token **and** bearer token file | Ambiguous source |
| Basic auth password **and** password file | Ambiguous source |
| Bearer token (or file) **and** basic auth | Two competing `Authorization` schemes |
| Password (or password file) without a username | Basic auth needs both |
| An `Authorization` entry in `--prometheus-headers` together with bearer/basic auth | The header would be overwritten by the auth layer |
| TLS cert file without key file (or vice versa) | A key pair needs both halves |
| `WithTransportConfig` **and** `WithRoundTripper` (Go API only) | The latter replaces the whole transport |

The chart checks the same rules *before* the binary ever starts, on the
effective sources (a `*Key` only counts when its Secret is named): a bearer
token together with basic auth, a password without a username, and a client
certificate without its key all fail `helm template` / `helm upgrade`. It also
rejects a raw `*File` path set alongside its `*Key` or its inline counterpart,
naming both values (see
[Recipe 2](#recipe-2--in-cluster-prometheus-behind-an-auth-proxy-projected-token)).

A malformed `--prometheus-headers` entry (anything without an `=`, an empty
key, a name that is not a valid HTTP token, or a value with control
characters) is reported with the offending entry before the client is built.
Only the **first** `=` splits an entry, so header values may contain `=`
themselves — a base64 tenant id, for instance. The flag is **repeatable**, one
header per occurrence, so a value may also contain a comma; the chart renders
one flag per entry of `prometheusAuth.headers`. The environment-variable form
is a single comma-separated string and cannot express such a value.

!!! warning "`insecureSkipVerify`"
    `prometheusAuth.tls.insecureSkipVerify` / `--prometheus-tls-insecure-skip-verify`
    disables server-certificate verification entirely, which makes every
    credential above interceptable. It is honoured, but logs a loud warning at
    startup. Use `serverName` or a `caKey` instead for anything that outlives a
    debugging session.

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| Pod is in `CrashLoopBackOff` with `reading prometheus bearer token file …: no such file or directory` | The key named in `bearerTokenKey` does not exist in `existingSecret`, or the Secret is in another namespace |
| `contains no valid PEM certificate` at startup | `tls.caKey` points at a key holding something other than a PEM bundle |
| Controller works, dashboard shows "No metrics data available" | The dashboard is missing its credentials — check for a `K8SSUSTAIN_PROMETHEUS_*` variable that should be `K8SSUSTAIN_DASHBOARD_PROMETHEUS_*`, or a patch applied to only one Deployment |
| Queries return `401` about an hour after startup | A rotating token was supplied inline instead of as a file — switch to `bearerTokenFile` (or `bearerTokenKey`) |
| `helm upgrade` fails with "`… are mutually exclusive`" | A `*File` raw path is set alongside its `*Key` or its inline value, or a bearer token is combined with basic auth. Keep exactly one; the message names both |
| `helm upgrade` fails with "`must be a string`" or "`Invalid type. Expected: string`" on a header | The header value is an unquoted number in the values file; write it as `"1234567"` |
| Startup log shows `system certificate pool unavailable` | The container image's trust store could not be read, so `tls.caKey`/`caFile` is the only root. A public-CA ingress in front of the same address will fail verification until the image's CA bundle is fixed |
| Pod is in `CrashLoopBackOff` with `reading prometheus bearer token file …: no such file or directory` and `bearerTokenFile` is set | The path does not exist in the pod. `bearerTokenFile` mounts nothing — only a file something else already projects (the kubelet's token, a CSI mount) is readable |
| Multi-tenant gateway returns `no org id` / empty results | `prometheusAuth.headers` missing, or the gateway expects a different header name than `X-Scope-OrgID` |
| `helm upgrade` restarts the pods with no value change | Should not happen — header keys are sorted before rendering. If it does, something else changed the pod template |

See also the [CLI reference](../reference/cli.md) for the full flag list and
the [Helm values reference](../reference/helm-values.md#prometheus-authentication)
for the complete `prometheusAuth` schema.
