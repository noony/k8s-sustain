# Development

Build, test, and contribute to k8s-sustain locally.

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Go | ≥ 1.26 | Build and test |
| Docker | any | Build container image |
| kubectl | any | Cluster interaction |
| helm | ≥ 3.10 | Chart development |
| minikube / kind / k3d | any | Local cluster |

## Clone and build

```bash
git clone https://github.com/noony/k8s-sustain.git
cd k8s-sustain
go build ./...
```

## Run tests

```bash
make test         # go test -shuffle=on ./...
make test-race    # go test -race -shuffle=on ./...   (matches CI)
make coverage     # go test -race -shuffle=on -coverprofile=coverage.out ./...
```

CI runs the race detector and goroutine-leak detection (via `go.uber.org/goleak`
on the `oomwatch`, `prometheus`, `dashboard`, and `webhook` packages), so flakes
or leaks introduced by new tests will surface in the test job.

## Lint

```bash
make lint   # golangci-lint run
```

`.golangci.yml` enables the standard linters plus `bodyclose`, `errorlint`,
`nilerr`, `copyloopvar`, `durationcheck`, `unconvert`, `unparam`, `gocritic`,
`nolintlint`, `gosec`, and the `gofumpt` formatter. `paralleltest` is
intentionally disabled until the suite is audited for shared state (Viper
globals, controller-runtime registries); adding `t.Parallel()` mechanically
risks hard-to-debug races on those singletons.

## Security & dependency scans

CI runs `gosec` on every push.

Dependabot is configured for `gomod`, GitHub Actions, the dashboard's npm
modules, and Docker base images — PRs are opened weekly.

## Project structure

```text
k8s-sustain/
├── api/v1alpha1/          # CRD Go types and deepcopy
│   ├── policy_types.go
│   └── zz_generated.deepcopy.go
├── cmd/
│   ├── controller/        # Root cobra command + start subcommand
│   ├── webhook/           # webhook subcommand
│   └── dashboard/         # dashboard subcommand
├── internal/
│   ├── autoscaler/        # HPA / KEDA detection used by recommender + webhook
│   ├── config/            # Centralized Viper config (flags, env, file)
│   ├── controller/        # Policy reconciler
│   ├── dashboard/         # Dashboard HTTP server
│   ├── httpx/             # Shared HTTP stack: envelope, middleware, hardened NewServer, shutdown
│   ├── k8s/               # client.New helper used by webhook + dashboard
│   ├── logging/           # Shared zap logger setup
│   ├── prometheus/        # Prometheus HTTP API client + metric name constants
│   ├── recommender/       # Resource recommendation logic (pure functions)
│   ├── webhook/           # Admission webhook HTTP handler
│   └── workload/          # Pod recycler, template/owner-ref helpers
├── charts/k8s-sustain/    # Helm chart
├── docs/                  # This documentation
├── Dockerfile
├── Makefile
└── main.go
```

## Running locally against a cluster

### Start the controller

```bash
# Point KUBECONFIG at your cluster
export KUBECONFIG=~/.kube/config

# Forward Prometheus if needed
kubectl port-forward -n k8s-sustain svc/k8s-sustain-prometheus-server 9090:80 &

go run main.go start \
  --prometheus-address=http://localhost:9090 \  # port-forwarded from the cluster
  --reconcile-interval=1m \
  --log-level=debug
```

To develop against an authenticated Prometheus (a shared Thanos/Mimir gateway,
a staging cluster behind an auth proxy), pass the same auth flags the
deployment uses:

```bash
go run main.go start \
  --prometheus-address=https://mimir-gateway.example.com/prometheus \
  --prometheus-bearer-token-file=$HOME/.config/mimir/token \
  --prometheus-headers=X-Scope-OrgID=tenant-a \
  --log-level=debug
```

The flag names are identical on `go run main.go dashboard`. The **environment
variables are not**: the dashboard binds its flags under the `dashboard.` Viper
key prefix, so it reads `K8SSUSTAIN_DASHBOARD_PROMETHEUS_*` while the
controller reads `K8SSUSTAIN_PROMETHEUS_*`. See the
[CLI reference](reference/cli.md#prometheus-authentication-and-tls-flags).

### Prometheus client options

`internal/prometheus.New(addr, opts...)` takes functional options. The
transport-related ones:

| Option | Purpose |
|--------|---------|
| `WithTransportConfig(TransportConfig)` | Bearer token (inline or file), basic auth (inline or password file), arbitrary headers, and TLS (`CAFile`, `CertFile`/`KeyFile`, `ServerName`, `InsecureSkipVerify`). This is what the `start` and `dashboard` call sites use: `config.LoadControllerConfig` / `LoadDashboardConfig` fill a `PrometheusTransport` field of this type directly, and return an error on a malformed `--prometheus-headers` entry so no call site can start with the headers silently dropped. |
| `WithRoundTripper(http.RoundTripper)` | Escape hatch installing a transport this package does not model (SigV4, an in-memory key pair, a recording transport in tests). **Mutually exclusive** with `WithTransportConfig` — passing both is an error from `New`, not a silent precedence rule. |

Implementation notes worth knowing before touching `internal/prometheus/transport.go`:

- The zero `TransportConfig` is a no-op: `New(addr)` with no options builds the
  client with no `RoundTripper` at all, byte-identical to the pre-auth
  behaviour.
- `RoundTrip` operates on a **clone** of the request. That is required by the
  `http.RoundTripper` contract and avoids a data race on the shared header map
  under `-race`.
- Credential **files are re-read on every request** so a kubelet-rotated
  projected service-account token keeps working. Do not add a cache here — the
  client issues a handful of queries per reconcile, bounded by
  `--prometheus-max-inflight`. The mTLS key pair gets the same property
  through `tls.Config.GetClientCertificate`, which reloads `CertFile`/`KeyFile`
  on every handshake; do not put a static entry in `tls.Config.Certificates`,
  it would shadow the callback.
- `TransportConfig.validate` checks header names and values with
  `httpguts` at construction. `http.Transport` would otherwise reject them on
  every request, tripping the circuit breaker and reading as an outage.
- This is a deliberate hand-rolled subset of
  `github.com/prometheus/common/config.HTTPClientConfig`, kept because that
  package *replaces* the root pool with `CAFile` where this one appends to the
  system pool, and because it would compile in JWT/OAuth2 and conntrack
  dependencies the binary does not otherwise use. Revisit if a proxy, OAuth2
  or SigV4 transport becomes a requirement.
- Every configured file is *also* read once inside `newTransportRoundTripper`,
  so a bad path fails `New` and therefore the process, rather than surfacing as
  a per-query error indistinguishable from a Prometheus outage.
- `CAFile` is appended to a **copy** of the system pool, and the base transport
  is a `Clone()` of `api.DefaultRoundTripper` — the package-level default is
  shared with every other `client_golang` consumer in the process and must
  never be mutated.
- Adding a new flag means editing, in `internal/config/config.go`,
  `bindPrometheusTransportFlags` (called with `""` for the controller and
  `"dashboard."` for the dashboard, which is what keeps the two flag sets from
  drifting) and `loadPrometheusTransport`, plus the field on
  `promclient.TransportConfig`. Mirror it in
  `charts/k8s-sustain/templates/_helpers.tpl` (`k8s-sustain.prometheusAuthArgs`
  / `…Env` / `…AuthSecretVolumes`), `values.yaml`, `values.schema.json`, and
  `charts/k8s-sustain/tests/prometheus-auth_test.yaml`. Any future component
  that queries Prometheus adds those four includes rather than re-deriving the
  flags.
- `--prometheus-headers` is a pflag **StringArray**, not a StringSlice: one
  element per flag occurrence, no CSV splitting, so a header value may contain
  a comma. The chart renders one flag per header for that reason. The env-var
  path (`getStringSlice`) still splits on commas and cannot express such a
  value.

### Start the webhook (requires TLS)

The webhook must be reachable from the API server, which makes local development more involved. Use local kind cluster with a self-signed cert.

## Local end-to-end testing

The repository ships a `Makefile.scenarios` harness that brings up a kind
cluster, installs cert-manager and metrics-server, builds & loads the
image, helm-installs k8s-sustain, and runs a small library of synthetic
workload scenarios designed to exercise `Ongoing`-mode recycling
end-to-end. The scenario targets refuse to run unless the current kubectl
context is `kind-k8s-sustain`; bypass with `SKIP_CONTEXT_CHECK=1`.

```bash
make test-kind-up                       # one-shot cluster + helm install
make test-scenario-steady               # apply a scenario
make test-scenario-status               # current vs. recommended table
make test-scenario-clean                # delete every scenario namespace
make test-kind-down                     # delete the kind cluster
```

See [`docs/guides/local-testing.md`](guides/local-testing.md) for the
scenario catalog and the expected outcomes.

The remainder of this section ("Deploying on kind") describes the
manual equivalent, which is useful when you want to understand exactly
what the harness does or deviate from it.

## Deploying on kind

A full local deployment with Prometheus, the controller, webhook, and dashboard:

### 1. Create a kind cluster

```bash
kind create cluster --name k8s-sustain
```

### 2. Build and load the image

```bash
make docker-build IMG=k8s-sustain:dev
kind load docker-image k8s-sustain:dev --name k8s-sustain
```

### 3. Install with Helm

```bash
helm install k8s-sustain ./charts/k8s-sustain \
  --set image.repository=k8s-sustain \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --set dashboard.enabled=true
```

`image.pullPolicy=Never` ensures Kubernetes uses the locally loaded image. The `prometheusAddress` is auto-resolved to the bundled prometheus subchart service.

### 4. Verify pods are running

```bash
kubectl get pods -w
```

### 5. Access the dashboard

```bash
kubectl port-forward svc/k8s-sustain-dashboard 8090:8090
```

Open `http://localhost:8090`.

### 6. Create a test policy

```bash
kubectl apply -f - <<'EOF'
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: test-policy
spec:
  selector:
    namespaces: [default]
  rightSizing:
    update:
      types:
        deployment: Ongoing
    resourcesConfigs:
      cpu:
        window: 168h
        requests:
          percentile: 95
      memory:
        window: 168h
        requests:
          percentile: 95
EOF
```

### Rebuilding after changes

After modifying code, rebuild and reload:

```bash
make docker-build IMG=k8s-sustain:dev
kind load docker-image k8s-sustain:dev --name k8s-sustain
kubectl rollout restart deployment k8s-sustain
kubectl rollout restart deployment k8s-sustain-dashboard
```

### Cleanup

```bash
kind delete cluster --name k8s-sustain
```

## Regenerating code

If you modify types in `api/v1alpha1/`, regenerate the deepcopy methods:

```bash
make generate
```

This requires `controller-gen` to be installed:

```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
```

## Building the container image

```bash
make docker-build IMG=ghcr.io/noony/k8s-sustain:dev
```

This builds a single-arch image for the host's native platform (e.g. `linux/arm64` on Apple Silicon, `linux/amd64` on Intel) using `docker buildx --load`. The Dockerfile is multi-arch aware: it honors `TARGETOS`/`TARGETARCH` and runs the Go and Node build stages on `$BUILDPLATFORM` so cross-compilation is native, not emulated.

**Note for Apple Silicon + colima users:** running an emulated `linux/amd64` binary inside colima has produced random stdlib panics (memory-model mismatches under x86-on-ARM translation). Always build natively — the default `make docker-build` does this.

### Multi-arch publish

```bash
docker buildx create --use --name k8s-sustain-builder   # one-time
make docker-buildx IMG=ghcr.io/noony/k8s-sustain:dev    # builds + pushes linux/amd64 + linux/arm64
```

Override `PLATFORMS` to change the matrix, e.g. `PLATFORMS=linux/arm64 make docker-buildx`. CI (`.github/workflows/release.yml`) publishes both `linux/amd64` and `linux/arm64` automatically on tag pushes.

The `helm` job in that same workflow also pushes both charts as OCI artifacts to `oci://ghcr.io/noony/helm-charts/<chart>`, in addition to attaching the packaged `.tgz` files to the GitHub release.

### Release notes

The `gh-release` job uses GitHub's automatic release notes (`generate_release_notes: true`), which are shaped by `.github/release.yml`. That file groups merged PRs into a "General" section and a "Dependencies" section (anything carrying the `dependencies` label, applied automatically by Dependabot), so a release isn't dominated by a long tail of dependency bumps. The `dependencies` exclusion on "General" makes the split independent of category order. PRs labeled `skip-changelog` are omitted from release notes entirely — apply that label manually (it isn't created by default: `gh label create skip-changelog --description "Exclude this PR from release notes" --color ededed`).

## Makefile targets

| Target | Description |
|--------|-------------|
| `make build` | Build the binary |
| `make test` | Run unit tests |
| `make generate` | Regenerate deepcopy code |
| `make docker-build` | Build a native-arch container image and load it into the local daemon |
| `make docker-buildx` | Build and push a multi-arch image (`PLATFORMS` env, default `linux/amd64,linux/arm64`) |
| `make helm-lint` | Lint the Helm chart |
| `make helm-template` | Render templates to stdout |
| `make helm-unittest` | Run Helm chart unit tests (requires the helm-unittest plugin) |

## Helm chart tests

The chart ships a [helm-unittest](https://github.com/helm-unittest/helm-unittest) suite
under `charts/k8s-sustain/tests/` — one `*_test.yaml` file per template, asserting on
default rendering, every enable/disable conditional, and value wiring. Install the plugin
once, then run the suite:

```bash
# --verify=false: helm v4 verifies plugin provenance by default, which git-source installs don't support
helm plugin install https://github.com/helm-unittest/helm-unittest.git --version 1.0.0 --verify=false
make helm-unittest
```

Run a single suite with the plugin's `-f` flag (paths are relative to the chart root):

```bash
helm unittest charts/k8s-sustain -f 'tests/deployment_test.yaml'
```

CI runs the full suite in the `helm` job alongside `helm lint` and `helm template`.
`make helm-promtool` additionally validates the rendered PrometheusRule
recording rules with `promtool` (requires `promtool` and `yq`): it checks that
the rules parse, that the dashboard-required ones are present, and it runs the
`promtool test rules` fixtures in `charts/k8s-sustain/tests/promtool/` against
the rendered output. Those fixtures test rule *semantics* — feed a rule its
input series and assert the recorded value — which `promtool check rules`
cannot do. Add one whenever a rule's correctness depends on timing or on a
join window, since that is exactly what a syntax check misses.

## Adding a new workload kind

To support a new workload kind (e.g. `Rollout` from Argo):

1. Add `ArgoRollout *UpdateMode` to `UpdateTypes` in `api/v1alpha1/policy_types.go` (already present as a placeholder)
2. Add the deepcopy block to `zz_generated.deepcopy.go`
3. Add `RecycleRolloutPods` to `internal/workload/patcher.go`
4. Add `reconcileRollouts` to `internal/controller/policy_controller.go`
5. Add the case to `UpdateTypes.ModeForKind` in `api/v1alpha1/policy_types.go` and to `resolveOwner` in `internal/webhook/handler.go`
6. Add the kind to the `ownerKindObjects` table in `internal/webhook/optin.go`. This is what lets the webhook's multi-level opt-in resolution read the workload-level annotation for the new kind; a kind missing here silently loses workload-level opt-in (namespace-level and pod-template-level keep working, since neither depends on this table)
7. Add the same kind to `k8s.OwnerChainDisableFor()` in `internal/k8s/client.go`. Every kind in `ownerKindObjects` must appear here too, or its first Get on the admission hot path stands up a cluster-wide informer over every object of that kind instead of costing one Get. `TestDisableForCoversOwnerAnnotationKinds` (`internal/webhook/optin_test.go`) cross-checks the two lists and fails if one is missing from the other
8. Add RBAC markers (`+kubebuilder:rbac:...`) to the controller
9. Add the Helm RBAC rule in `charts/k8s-sustain/templates/rbac.yaml`

## Documentation site

The docs site (this site) is built with mkdocs-material and versioned with
[mike](https://github.com/jimporter/mike). Two things deploy it:

- Every push to `main` (that touches `docs/**` or `mkdocs.yml`) updates the
  `main` version — a rolling snapshot of in-progress docs, same as the old
  always-live behavior.
- Every pushed tag `vX.Y.Z` that is **not** a prerelease (no `-` in the tag,
  so `v0.1.0-rc.1` is excluded) publishes a permanent, immutable version
  `X.Y.Z` and re-points the `latest` alias at it. This runs as the `docs` job
  in `.github/workflows/release.yml`, alongside the Docker/Helm/GitHub
  release jobs.

The version dropdown in the site header is generated by mike from
`versions.json` on the `gh-pages` branch — it only appears on the deployed
site, not in a local `mkdocs serve`/`mkdocs build` preview.

To preview docs locally, nothing changes:

```bash
pip install -r requirements-docs.txt
mkdocs serve
```

The live site is published through GitHub's Actions-based Pages deployment
(`actions/upload-pages-artifact` + `actions/deploy-pages` at the end of the
`deploy`/`docs` jobs above), not the legacy "deploy from a branch" mechanism —
`gh-pages` is still where mike stores version history, but pushing to it no
longer auto-triggers a site rebuild by itself.

## Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feat/my-feature`
3. Commit with a clear message
4. Open a pull request against `main`

Please ensure `go build ./...` and `go test ./...` pass before opening a PR.
