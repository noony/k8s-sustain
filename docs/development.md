# Development

Build, test, and contribute to k8s-sustain locally.

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Go | ≥ 1.26 (see `go.mod`) | Build and test |
| Node.js | 26 | Build and test the dashboard frontend |
| Docker | any | Build container image |
| kubectl | any | Cluster interaction |
| helm | ≥ 3.10 | Chart development |
| kind | any | Local cluster and [end-to-end harness](guides/local-testing.md) |

## Clone and build

```bash
git clone https://github.com/noony/k8s-sustain.git
cd k8s-sustain
go build ./...   # Go only
make build       # dashboard UI (npm ci + build), then bin/k8s-sustain
```

## Run tests

```bash
make test         # go test -shuffle=on ./...
make test-race    # go test -race -shuffle=on ./...   (matches CI)
make coverage     # go test -race -shuffle=on -coverprofile=coverage.out ./...
```

CI runs the race detector and goroutine-leak detection (via `go.uber.org/goleak`
on the `dashboard`, `k8s`, `oomwatch`, `prometheus`, and `webhook` packages), so
flakes or leaks introduced by new tests surface in the test job.

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

### Pinning GitHub Actions

Every `uses:` in every workflow must be pinned to a full 40-character commit
SHA, with the version in a trailing comment:

```yaml
- uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
```

A mutable tag (`@v5`) lets whoever controls the action's repository repoint it
at new code that then runs with your workflow's token. `hack/verify-action-pins.sh`
enforces this: it runs as a `pre-commit` hook locally and as the `supply-chain`
CI job, and fails on any tag- or branch-pinned action as well as on a SHA with
no trailing version comment. `zizmor` runs in the same
job and lints the workflows for other Actions security problems.

Keep the trailing `# vX.Y.Z` comment accurate — Dependabot uses it to bump the
pin, and it is the only human-readable record of what the SHA points at.

Release signing depends on `id-token: write` being granted to exactly the jobs
that sign. If you touch a job's `permissions:` block, check that signing still
runs. The full posture and the verification commands are documented in
[Security](security.md).

## Project structure

```text
k8s-sustain/
├── api/v1alpha1/          # CRD Go types (Policy, WorkloadRecommendation) and deepcopy
├── cmd/
│   ├── controller/        # Root cobra command + start subcommand
│   ├── webhook/           # webhook subcommand
│   └── dashboard/         # dashboard subcommand
├── internal/
│   ├── autoscaler/        # HPA / KEDA detection used by recommender + webhook
│   ├── config/            # Centralized Viper config (flags, env, file)
│   ├── controller/        # Policy reconciler
│   ├── dashboard/         # Dashboard HTTP server + embedded Vue SPA (ui/)
│   ├── httpx/             # Shared HTTP stack: envelope, middleware, hardened NewServer, shutdown
│   ├── k8s/               # client.New helper used by webhook + dashboard
│   ├── logging/           # Shared zap logger setup
│   ├── oomwatch/          # Live OOMKilled detection, re-triggers reconciles
│   ├── policymatch/       # Policy resolution (ResolvePolicy) and selector matching
│   ├── prometheus/        # Prometheus HTTP API client + metric name constants
│   ├── recommender/       # Resource recommendation logic (pure functions)
│   ├── version/           # Build version
│   ├── webhook/           # Admission webhook HTTP handler
│   ├── wlrcache/          # WorkloadRecommendation naming and upsert
│   └── workload/          # Pod recycler, template/owner-ref helpers
├── charts/
│   ├── k8s-sustain/          # Operator chart
│   └── k8s-sustain-policies/ # Policy objects chart
├── hack/
│   ├── docscheck/         # Docs drift guards (run by go test)
│   └── scenarios/         # kind end-to-end scenarios
├── docs/                  # This documentation
├── Dockerfile
├── Makefile
├── Makefile.scenarios     # kind end-to-end harness
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
  --prometheus-address=http://localhost:9090 \
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

The flag names are identical on `go run main.go dashboard`; the environment
variables are not — see
[Environment variables — the prefixes differ per subcommand](guides/authenticated-prometheus.md#environment-variables-the-prefixes-differ-per-subcommand).

### Start the webhook

The webhook must be reachable from the API server over TLS, so develop it
in-cluster: use the [kind harness](guides/local-testing.md) or the manual kind
deployment below and rebuild the image.

## Local end-to-end testing

`Makefile.scenarios` brings up a kind cluster with cert-manager and
metrics-server, installs k8s-sustain from the local image, and applies
synthetic workload scenarios. See [Local end-to-end testing](guides/local-testing.md)
for the targets, variables and the scenario catalog.

## Deploying on kind manually

The manual equivalent of `make test-kind-up`, useful to understand or deviate
from what the harness does.

```bash
kind create cluster --name k8s-sustain

# The webhook needs a serving certificate; cert-manager issues it.
helm repo add jetstack https://charts.jetstack.io --force-update
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true --wait

make docker-build IMG=k8s-sustain:dev
kind load docker-image k8s-sustain:dev --name k8s-sustain

helm dependency build charts/k8s-sustain
helm upgrade --install k8s-sustain ./charts/k8s-sustain \
  --namespace k8s-sustain --create-namespace \
  --set image.repository=k8s-sustain \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --wait
```

`image.pullPolicy=Never` makes Kubernetes use the locally loaded image. The
dashboard is enabled by default and `prometheusAddress` resolves to the bundled
Prometheus subchart. Open the dashboard with
`kubectl port-forward -n k8s-sustain svc/k8s-sustain-dashboard 8090:8090`, then
create a Policy (see [Quick start](getting-started/quick-start.md)).

After changing code, rebuild, reload and restart:

```bash
make docker-build IMG=k8s-sustain:dev
kind load docker-image k8s-sustain:dev --name k8s-sustain
kubectl -n k8s-sustain rollout restart deploy/k8s-sustain deploy/k8s-sustain-webhook deploy/k8s-sustain-dashboard
```

Tear down with `kind delete cluster --name k8s-sustain`.

## Regenerating code

After modifying types in `api/v1alpha1/`:

```bash
make generate      # deepcopy methods
make manifests     # CRD YAML into charts/k8s-sustain/files/crds/
make verify-crds   # what CI checks
```

`controller-gen` is a Go tool dependency pinned in `go.mod` and run with
`go tool controller-gen`; there is nothing to install.

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

`make help` lists every target with its description (`make test-help` for the
kind harness). The ones used most:

| Target | Description |
|--------|-------------|
| `make build` | Build the dashboard UI and the binary |
| `make test-race` | Run all tests with the race detector, as CI does |
| `make lint` | Run golangci-lint |
| `make helm-unittest` | Run Helm chart unit tests |

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

Every reader of workload objects goes through one table, so supporting a new
kind (say `Rollout` from Argo) is mostly a matter of registering it there:

1. Add `<Kind> *UpdateMode` to `UpdateTypes` in `api/v1alpha1/policy_types.go`, add the case to `UpdateTypes.ModeForKind`, then run `make generate` and `make manifests`
2. Add the kind to the `ownerKinds` table in `internal/workload/kindobjects.go` (object and list constructors plus its `GroupResource`) and, if the controller and dashboard should list it, to `workload.SupportedKinds`. This single table drives the controller's target listing, the dashboard's listing and NotFound errors, the workload-level annotation reads in the webhook and the OOM watcher, and the retention sweep's existence check. A kind missing here silently loses workload-level opt-in (namespace-level and pod-template-level keep working, since neither depends on this table)
3. Add the kind's pod template and selector to `workload.PodTemplateOf` in `internal/workload/templates.go`. Return a nil selector for kinds whose pods must never be recycled (see Job and CronJob)
4. Add the same kind to `k8s.OwnerChainDisableFor()` in `internal/k8s/client.go`. Every kind in `ownerKinds` must appear here too, or its first Get on the admission hot path stands up a cluster-wide informer over every object of that kind instead of costing one Get. `TestDisableForCoversOwnerAnnotationKinds` (`internal/webhook/optin_test.go`) cross-checks the two lists and fails if one is missing from the other
5. If the kind is a CRD, register its scheme in `internal/config/config.go` and `internal/dashboard/server.go`
6. If the kind's pods cannot be evicted (job-like workloads), add an in-place-only branch in `reconcileWorkload` (`internal/controller/workload_reconcile.go`) alongside the Job, CronJob and bare-pod ones; selector-based kinds need nothing more
7. Add RBAC markers (`+kubebuilder:rbac:...`) to the controller and the Helm RBAC rule in `charts/k8s-sustain/templates/rbac.yaml`

## Dashboard frontend

The dashboard frontend is a Vue 3 + TypeScript SPA built with Vite in
`internal/dashboard/ui/frontend/`. `make build-ui` (part of `make build`) runs
`npm ci && npm run build` into `internal/dashboard/ui/dist/`, which is
embedded into the Go binary via `go:embed`. The Docker build runs the frontend
build in its own stage.

```bash
cd internal/dashboard/ui/frontend
npm install
npm run dev        # Vite dev server on http://localhost:5173
npm run test       # vitest
npm run typecheck  # vue-tsc --noEmit
npm run lint       # eslint src/
```

The dev server proxies `/api` and `/healthz` to `localhost:8090`: run the
backend with `go run main.go dashboard --bind-address=:8090`, or port-forward
the in-cluster dashboard Service to `:8090`.

### Styling

The dashboard's look lives in one stylesheet,
`internal/dashboard/ui/frontend/src/style.css`, driven by CSS custom
properties declared for `[data-theme='dark']` and `[data-theme='light']`.
Restyle by changing tokens, not by adding per-view colours.

- **Fonts** are self-hosted through the `@fontsource-variable/inter` and
  `@fontsource-variable/jetbrains-mono` packages (imported in `src/main.ts`)
  and end up embedded in the binary. Never load fonts from a CDN — the
  dashboard has to work in clusters without internet egress.
- **Chart series colours** are the `--series-*` tokens (`cpu`, `mem`, `config`,
  `rec`, `baseline`). Views pass the key (`color: 'rec'`) and `lib/chart.ts`
  resolves it per theme and recolours live charts on theme change. Colour
  follows the entity: request and limit share `config` and differ by dash
  pattern, which the legend keys reproduce. When changing a series colour,
  re-check it against both card surfaces for colour-vision separation.

## Documentation site

The docs site (this site) is built with mkdocs-material and versioned with
[mike](https://github.com/jimporter/mike). Two things deploy it:

- Every push to `main` (that touches `docs/**` or `mkdocs.yml`) updates the
  `main` version — a rolling snapshot of in-progress docs.
- Every pushed tag `vX.Y.Z` that is **not** a prerelease (no `-` in the tag,
  so `v0.1.0-rc.1` is excluded) publishes a permanent, immutable version
  `X.Y.Z` and re-points the `latest` alias at it. This runs as the `docs` job
  in `.github/workflows/release.yml`, alongside the Docker/Helm/GitHub
  release jobs.

The version dropdown in the site header is generated by mike from
`versions.json` on the `gh-pages` branch — it only appears on the deployed
site, not in a local `mkdocs serve`/`mkdocs build` preview.

To preview docs locally:

```bash
pip install -r requirements-docs.txt
mkdocs serve
```

CI builds with `mkdocs build --strict` and lints with markdownlint. The
`validation` block in `mkdocs.yml` turns broken links and anchors into
warnings, which `--strict` fails on. `hack/docscheck` (run by `make test`)
fails when `docs/reference` drifts from the flags, metrics, recording rules
and Helm values it documents.

### Theme

The site uses the stock Material theme with a single stylesheet,
`docs/stylesheets/extra.css`. Its colour tokens mirror the dashboard's
(`internal/dashboard/ui/frontend/src/style.css`) so the docs and the product
share one palette; change them in both places or they drift. The palette
follows the visitor's system preference by default, with a manual toggle.

Diagrams are written as ` ```mermaid ` fences and rendered client-side by
Material, so they pick up the active colour scheme. Prefer them over ASCII
art, which cannot adapt to dark mode. The landing page (`docs/index.md`) uses
a few `ks-*` classes defined in the same stylesheet; every other page is plain
Markdown.

The live site is published through GitHub's Actions-based Pages deployment,
not the legacy "deploy from a branch" mechanism — `gh-pages` is where mike
stores version history, but pushing to it does not trigger a site rebuild by
itself. The last step of the `deploy`/`docs` jobs above is the
in-repo composite action `.github/actions/deploy-pages-site`, which uploads
the `gh-pages` checkout as the Pages artifact and creates the deployment
through the REST API with the `gh-pages` commit as `pages_build_version`.
Stock `actions/deploy-pages` uses the workflow's own commit for that field,
and Pages silently skips a version it has already served — a release tag sits
on a commit the `main` deploy already published, so a tag-triggered deploy
would report success without updating the site
([actions/deploy-pages#383](https://github.com/actions/deploy-pages/issues/383)).

## Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feat/my-feature`
3. Commit with a clear message
4. Open a pull request against `main`

Please ensure `go build ./...` and `go test ./...` pass before opening a PR.

## Internals

Implementation notes for maintainers. Behaviour users can observe is
documented in the guides and references; this section explains why the code
is shaped the way it is.

### Prometheus transport

`internal/prometheus.New(addr, opts...)` takes functional options. The
transport-related ones:

| Option | Purpose |
|--------|---------|
| `WithTransportConfig(TransportConfig)` | Bearer token (inline or file), basic auth (inline or password file), arbitrary headers, and TLS (`CAFile`, `CertFile`/`KeyFile`, `ServerName`, `InsecureSkipVerify`). The `start` and `dashboard` call sites use it: `config.LoadControllerConfig` / `LoadDashboardConfig` fill a `PrometheusTransport` field of this type and return an error on a malformed `--prometheus-headers` entry. |
| `WithRoundTripper(http.RoundTripper)` | Escape hatch for a transport this package does not model (SigV4, an in-memory key pair, a recording transport in tests). **Mutually exclusive** with `WithTransportConfig` — passing both is an error from `New`. |

Before touching `internal/prometheus/transport.go`:

- The zero `TransportConfig` is a no-op: `New(addr)` builds the client with no
  `RoundTripper` at all.
- `RoundTrip` operates on a **clone** of the request, as the
  `http.RoundTripper` contract requires; mutating the shared header map races
  under `-race`.
- Credential **files are re-read on every request** so a kubelet-rotated
  projected token keeps working. Do not cache: query volume is bounded by
  `--prometheus-max-inflight`. The mTLS key pair reloads through
  `tls.Config.GetClientCertificate` on every handshake; a static entry in
  `tls.Config.Certificates` would shadow the callback.
- Every configured file is also read once in `newTransportRoundTripper`, so a
  bad path fails `New` and the process instead of looking like a Prometheus
  outage per query. `TransportConfig.validate` checks header names and values
  with `httpguts` for the same reason: `http.Transport` would reject them on
  every request and trip the circuit breaker.
- `CAFile` is appended to a **copy** of the system pool, and the base transport
  is a `Clone()` of `api.DefaultRoundTripper`, which is shared process-wide and
  must never be mutated.
- This is a deliberate subset of `prometheus/common/config.HTTPClientConfig`:
  that package *replaces* the root pool with `CAFile`, and pulls in JWT/OAuth2
  and conntrack dependencies. Revisit if OAuth2 or SigV4 becomes a requirement.
- `--prometheus-headers` is a pflag **StringArray** (one element per flag
  occurrence, no CSV splitting); the env-var path (`getStringSlice`) still
  splits on commas.
- A new transport flag goes in `bindPrometheusTransportFlags` (called with `""`
  for the controller and `"dashboard."` for the dashboard, which keeps the two
  flag sets from drifting) and `loadPrometheusTransport` in
  `internal/config/config.go`, plus the field on `promclient.TransportConfig`.
  Mirror it in `charts/k8s-sustain/templates/_helpers.tpl`
  (`k8s-sustain.prometheusAuthArgs` / `…Env` / `…AuthSecretVolumes`),
  `values.yaml`, `values.schema.json`, and
  `charts/k8s-sustain/tests/prometheus-auth_test.yaml`.

### Controller

#### Work-list union and informer lag

The controller's reads go through a watch-populated informer cache, so a `WorkloadRecommendation` that discovery created moments earlier in the same reconcile is often not yet visible to a list, and nothing watches `WorkloadRecommendation` to re-trigger a reconcile when it becomes visible. The computation work-list is therefore the union of the listed cache objects and the identities discovery just ensured. Without the union a freshly matched workload would wait a full `--reconcile-interval` before being computed. Discovery already holds the container set for those identities, so the union costs no extra API calls.

#### Per-identity computation under owner-name grouping

The computation unit is the identity, not the workload object: a group of workloads sharing an owner-name produces exactly one computation and one write, against the union of the members' containers. Computing per member would give the single shared `WorkloadRecommendation` several competing answers, with the survivor decided by whichever member's goroutine finished last. Retry backoff is also per member: an identity is withheld from the batch prefetch only when every member is backed off, so one failing member cannot deny its healthy siblings their inputs.

#### Owner-name group: merge order and write cost

For an owner-name group, the union snapshot in `status.observedResources` is built from the members' containers. Where two members declare the same container name with different requests/limits, the entry from the member whose `kind/namespace/name` sorts first is kept whole, rather than mixing one member's request with another's limit. The recommendation is computed against that union, using the autoscaler of the first member (same sort order) that has one, and treating the identity as being as old as its oldest member.

Every choice is decided by the members' names or by an aggregate over all of them, so the stored snapshot and recommendation do not depend on the order the API server lists members in, nor on which member's work finishes first. Discovery (`EnsureExists`) and the computation phase (`wlrcache.Upsert`) both write `status.observedResources`, but always the same merged value computed once per identity, so the two writers never disagree, and a group whose members and metrics are unchanged costs no status write on subsequent reconciles rather than one write per member per cycle.

#### Bare-pod grouping implementation

Bare-pod groups are built by `workload.GroupBarePods`, used by both the controller and the dashboard. It resolves each pod's policy across the pod's own annotations and the namespace annotation map it is passed, so Namespace-level opt-in works for bare pods. The dashboard displays the containers and annotations of the most recently created pod in the group.

### OOM watcher

#### Cache: per-field merge

The OOM cache is keyed by `(namespace, ownerKind, ownerName, container)`, so every pod of a workload writes to the same slot, and pods are reconciled in parallel. The slot is a per-field merge rather than last-write-wins:

- **Identity and timestamps** (pod name/UID, terminated-at, observed-at, restart count) come from the newest observation, ordered by `(terminated-at, restart-count)`. Restart count only breaks ties on an equal timestamp, which `metav1.Time`'s one-second resolution makes common; across two pods the counters are unrelated, so a cross-pod tie picks arbitrarily between two equally recent stamps.
- **The OOM-time memory limit** (the bump anchor for the memory floor) is the largest limit seen across those pods, not the most recent. A Deployment bumped 128Mi → 256Mi can have an old un-resized pod (128Mi) OOM after an already-resized one (256Mi); anchoring on the newer 128Mi would bump the floor to a value 256Mi already proved insufficient. The recommender `max()`es this record against the Prometheus anchor for the same reason.

An out-of-order observation still counts as a new kill for triggering an immediate reconcile; it contributes its limit, not its identity. The accepted cost is on a deliberate downsize: an older, larger limit keeps anchoring the floor high until it ages out of the cache (bounded by the TTL and the recommender's freshness window). That errs toward brief over-provisioning rather than an under-bump into another OOM, and it cannot compound, because every merged value is a limit the kernel actually applied.

The watcher captures the limit from `ContainerStatus.Resources` (falling back to the pod spec when the status carries none). The spec holds the desired limit, which the recommender itself rewrites on every in-place resize, so anchoring on the spec would feed the OOM floor its own previous output and compound on each kill.

#### Predicate and dedup

The watcher's event predicate is local (no apiserver calls): a pod is queued when it carries the `k8s.sustain.io/policy` annotation itself or has ever had an `OOMKilled` container status. The OOM arm is sticky — `LastTerminationState.Terminated` persists for the container's lifetime — so every later event for such a pod re-queues a reconcile. Before resolving the owner or Namespace, the watcher checks whether this exact termination (pod UID + container + restart count + terminated-at) was already resolved, so a chronically restarting pod's owner resolution runs once per distinct kill, not once per status write, including for pods managed by no Policy.

### Webhook

#### Informer cache and shutdown ordering

The webhook serves `Policy`, `WorkloadRecommendation` and `Namespace` reads from an informer cache (`k8s.NewCached`); budget roughly 20–50 MB resident at 10k workloads. Owner-chain kinds (`ReplicaSet`, `Job`, and the workload kinds read for workload-level opt-in) stay uncached: they are read at most once per pod CREATE, and a cluster-wide informer per kind would cost more memory than the Gets it saves.

The three informers are registered and synced during startup, before the webhook receives traffic. Registering them lazily would push a blocking cluster-wide LIST into the first admission's 2s per-call timeout, so on large clusters the first pods after every restart would fail open. Startup is all-or-nothing: if it fails (most often because the CRDs are not yet established on a fresh install, which `k8s.NewCached` waits up to two minutes for), the informers already started are stopped before the error is returned, so a crash-looping webhook does not leak a cache per attempt. The cache lives on a run context kept alive past SIGTERM so draining admissions still read fresh data.

#### Owner-resolution cost and caching

The webhook pre-gates the owner and Namespace reads of [policy resolution](reference/annotation.md#resolution-order) behind `anyPolicyCovers`: if no Policy's `selector` could claim a pod at all, it skips them entirely. This *is* the cost control for a cluster with at least one selector-scoped Policy — most pod creates in most namespaces are skipped before any extra read.

It does **not** bound cost on a cluster where a Policy has an empty `selector` (no `namespaces`, no `labelSelector`) — the default, and what [Quick Start](getting-started/quick-start.md) produces. Such a Policy covers every pod in the cluster, so the pre-gate always passes and every pod CREATE it admits pays for owner resolution: `resolveCachedPodOwner` (via a Deployment/Rollout pod's ReplicaSet, or a CronJob pod's Job) does one Get to find the top-level owner. This Get is not scoped to the multi-level opt-in chain — `admit()` needs the same top-level owner to look up the `WorkloadRecommendation` for **every** pod it injects into, pod-template-annotated (the common case, since every existing user annotates the pod template) or not. A pod with no annotation of its own additionally goes through the opt-in chain (`resolveOptIn`), which needs a second Get — `ownerAnnotations`, of the resolved top-level object's own `metadata.annotations` — to evaluate the workload level; that second Get *is* scoped to unannotated pods, since an annotated pod's own annotation already decides the policy without ever reading its owner's. A rolling restart of a large Deployment therefore issues up to two owner Gets per replica in the worst case — not one.

The webhook absorbs this with two small in-memory caches (`internal/webhook/ownercache.go`), both TTL ~30s and both consulted before their respective Get: `Handler.ownerRefCache`, keyed by the pod's own immediate controller ownerRef (`namespace/kind/name/UID` of its ReplicaSet or Job), bounds the first Get — shared by **every** pod CREATE that reaches owner resolution at all, whether it took the pod-template-annotated path or the multi-level opt-in chain; `Handler.ownerAnnCache`, keyed by `(namespace, kind, name)` of the resolved top-level object, bounds the second Get and is reached only via the multi-level opt-in chain. N pods created behind the same owner in a burst — exactly what a rolling restart produces — share both cache keys, so the pair collapses from up to 2N Gets to at most 2.

Both caches also collapse a *concurrent* cold-start burst, not just the steady-state (already-warm) case: N pods admitted at the same instant behind one owner — the exact shape of a rolling restart — issue one in-flight Get via [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight), with every other admission waiting on that Get's result instead of each starting its own. A waiter still respects its own admission deadline: if it times out before the in-flight Get returns, only that one admission gives up and fails open (with the pod resolved on template resources); the Get itself keeps running regardless, to completion or its own 2s `apiCallTimeout` budget (whichever comes first) — not just for as long as some other admission is still waiting on it, but also to populate the cache for the next burst even if every waiter has already given up.

A panic inside that shared Get is contained rather than fatal. Running it on a `singleflight` goroutine puts it outside the HTTP handler's panic-recovery middleware, and `singleflight` deliberately re-raises a leader panic on a bare goroutine that nothing can recover — so, unguarded, one nil deref under an owner Get would abort the whole webhook process and (under `failurePolicy: Fail`) block every Pod CREATE in the cluster until it restarted. The webhook therefore recovers such a panic itself, logs it with its stack, counts it on [`k8s_sustain_webhook_panic_total`](reference/metrics.md#k8s_sustain_webhook_panic_total) under `singleflight/ownerRef` or `singleflight/ownerAnnotations`, and hands it to the leader and every waiter as an ordinary error — which they fail open on like any other failed Get. Nothing is cached, and the next admission retries.

The trade-off: a workload that gains its opt-in annotation at the workload level may take up to the cache's TTL to be picked up by admission. This is acceptable because the controller reconciles independently of admission on its own interval regardless of what the webhook does.

#### Stub writes: dedup, bounds and shutdown

A stub becomes visible to the webhook's informer only after the create and watch propagation, so inside that window every pod of a scaling workload reads `missing` and would fire its own create (a 500-replica scale-out means 500 creates of one name, 499 rejected). Stub requests are therefore deduplicated per identity for 30s and bounded to 16 concurrent writes. Over-capacity requests queue rather than drop: a run-once Job has no later admission to retry on.

Stub goroutines outlive the admission that started them (up to 30s queued plus 5s for the write), so on shutdown SIGTERM cancels in-flight stub writes and the process waits, bounded, for them to unwind before stopping the informer cache they read through. An abandoned request is re-issued by the next admission for the same identity.

The status snapshot (`status.observedResources`) is also written on the `AlreadyExists` path, so an object lacking one can be filled in by a later admission; a snapshot discovery already wrote from the pod template wins. Only the missing case creates a stub: on a stale object a create is a guaranteed no-op.

The `k8s.sustain.io/stub` label is provenance, not control flow. The controller's own write path must `Create` before it can patch status (the status subresource discards status supplied at create), so every controller-written recommendation is transiently empty-status too.

#### Cache staleness constant

The staleness window is `webhook.Handler.CacheStaleness` (default `DefaultCacheStaleness = 30m`, `internal/webhook/recommendations.go`). It is not exposed as a flag or Helm value.

#### Startup probe budget

The webhook builds an informer
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

### WorkloadRecommendation lifecycle

#### GC preconditions

- **Per-cycle sweep** deletes with `uid` + `resourceVersion` preconditions, because it decides on a copy read from the informer cache. With several reconciles running at once, a workload re-annotated from one policy to another is re-labelled and rewritten by the new policy while the old policy's sweep still holds the pre-rewrite copy; the precondition turns that delete into a conflict (left alone, re-judged next cycle) instead of destroying a fresh recommendation. A conflict is an expected outcome, not an error.
- **Policy-deletion finalizer** deletes on `uid` alone. Its rule ("this object belongs to the policy being deleted") cannot be invalidated by a rewrite, so a `resourceVersion` precondition would only let a WLR rewritten inside the informer propagation window survive. The `uid` still refuses a name reused by a delete-and-recreate. Any failure, conflicts included, is returned so the finalizer stays on and the deletion is retried.
- **Orphan reaper** keeps the strict `uid` + `resourceVersion` preconditions: its rule reads `spec.policy`, which is rewritten in place when a re-annotated workload's WLR is re-pointed at its new policy, so a stale copy can call a just-adopted object an orphan. It guarantees nothing, so it can afford the conflict.

#### Freshness grace

Independently of retention, a `WorkloadRecommendation` created within the last 10 minutes is never swept, nor is one whose workload object was created in that window; it protects an identity first written just after a reconcile built its target list. The grace keys off creation timestamps, not `status.observedAt`: the computation phase refreshes `observedAt` for every object in its list each cycle, departed identities included, so an `observedAt`-based guard would be self-satisfying and an opted-out workload would keep its recommendation (and its share of Prometheus load) indefinitely.

#### Departed waiver bound

The webhook bounds the `departed` staleness waiver with its own `--recommendation-retention` (rendered from the same `controller.recommendationRetention` Helm value as the controller's). Relying on the sweep to delete a lapsed object would leave the waiver unbounded whenever the controller stops sweeping: both the sweep and the clearing of `departed` live inside the reconcile, which returns early when a workload listing fails (RBAC revoked on one kind, an unreachable API group, a removed CRD). `departed` is only set on a positively confirmed absence, never when the existence check errors.
