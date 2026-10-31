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

### Recommendation inputs in tests

The controller and the dashboard read Prometheus recommendation inputs only
through the `recommender.InputsFetcher` port. Their tests inject
`recommendertest.StaticInputs` instead of serving PromQL from an
`httptest` server:

```go
inputs := recommendertest.NewStaticInputs().
    Set(id,
        recommender.WithCPUUsage(promclient.ContainerValues{"app": 0.1}),
        recommender.WithOOMKills(map[string]recommender.OOM{"app": {Kills: 1, PeakBytes: 300 << 20, HasPeak: true}}),
    ).
    SetHistory(id, recommender.NewHistory([]recommender.HistoryPoint{
        {At: earlier, Inputs: recommender.NewInputs(recommender.WithCPUUsage(promclient.ContainerValues{"app": 0.05}))},
    })).
    Fail(otherID, errors.New("prometheus down"))
```

Each signal's module exports the option that sets its values
(`WithCPUUsage`, `WithMemoryUsage`, `WithOOMKills`). `Set` builds the
identity's inputs with `recommender.NewInputs`, which gives every signal it
is not given its values from no samples, so they read exactly as the inputs
`PromInputs` fetches. `SetHistory` serves a history built with
`recommender.NewHistory`, each instant's inputs built with `NewInputs`, and
`FailHistory` fails only the history. An identity with neither gets empty
inputs, at every instant of a history, whatever the settings it is fetched
under, as Prometheus answers for one with no samples. `Calls()` reports every
fetch, the instant it was evaluated as of, the settings it ran under and its
requests, `HistoryCalls()` every history fetch and its span, and
`Requested(id)` whether an identity was part of any fetch, so a test can
assert what was fetched and how. Prefer driving
`PolicyReconciler.Reconcile` end to end with the fake client and
`StaticInputs`. The Prometheus adapter, `recommender.PromInputs`, is the only
code tested against a fake PromQL server: sharding, retry, the one-identity
fallback, each signal's failure policy, and the range read of a history. A signal's own query and parsing
are tested without a server, from its query text and from `ShardSample`s.

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
│   ├── autoscaler/        # HPA / KEDA lookup of the members of an identity
│   ├── config/            # Centralized Viper config (flags, env, file)
│   ├── controller/        # Policy reconciler: the pass and the publisher of its metric series
│   ├── dashboard/         # Dashboard HTTP server + embedded Vue SPA (ui/)
│   ├── governance/        # One decision per pod or pod template: identity, opt-in, governing Policy or refusal
│   ├── httpx/             # Shared HTTP stack: envelope, middleware, hardened NewServer, shutdown
│   ├── inventory/         # Workload objects → identities (members, containers, age, Departed, governing Policy or Conflicted)
│   ├── k8s/               # client.New helper used by webhook + dashboard
│   ├── ledger/            # Member health: retry backoff, Blocked, Warnings, identity health over its members, the requeue
│   ├── logging/           # Shared zap logger setup
│   ├── oomwatch/          # Live OOMKilled detection, re-triggers reconciles
│   ├── prometheus/        # Prometheus HTTP API client + metric name constants
│   ├── recommender/       # The one Request assembly per identity, recommendation signals (one module each), Compute composing their stages, and the batch-inputs port with its Prometheus adapter
│   │   └── recommendertest/  # In-memory inputs adapter for tests
│   ├── version/           # Build version
│   ├── webhook/           # Admission webhook HTTP handler
│   ├── wlrcache/          # WorkloadRecommendation lifecycle: every write (Request, Ensure, Record), verdict (Read, Held, Expired) and delete (Sweep, Release, Reap)
│   └── workload/          # Applying a Recommendation: the Policy's apply choices, the classified apply result, container injection (requests never above limits), the patcher (one rule per kind), template/owner-ref helpers
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

The Prometheus flags are global, so `go run main.go dashboard` takes the same
ones, and reads the same environment variables — see
[Environment variables](guides/authenticated-prometheus.md#environment-variables).

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

CI runs the full suite in the `helm` job alongside `helm lint`, `helm template`
and `make helm-promtool`. `make helm-promtool` validates the rendered
PrometheusRule recording rules with `promtool` (requires `promtool` and `yq`;
CI installs pinned, checksum-verified releases of both): it checks that the
rules parse, that the dashboard-required ones are present, and it runs the
`promtool test rules` fixtures in `charts/k8s-sustain/tests/promtool/` against
the rendered output. Those fixtures test rule *semantics* — feed a rule its
input series and assert the recorded value — which `promtool check rules`
cannot do. Add one whenever a rule's correctness depends on timing or on a
join window, since that is exactly what a syntax check misses. Every rule a
recommendation signal reads must have one: `hack/signalcontract` fails
otherwise (see [Signal contracts](reference/recording-rules.md#signal-contracts)).
It also fails when the regex with which `k8s_sustain:pod_workload` selects
the owner-name annotation stops matching exactly the values
`workload.ApplyOwnerNameOverride` accepts, when the rules give an overridden
pod without a controller another kind than it does, or when the bundled
kube-state-metrics stops exporting the annotation
(`pod_workload_owner_name_test.yaml` covers the mapping itself).

## Adding a new workload kind

Every reader of workload objects goes through one table, so supporting a new
kind (say `Rollout` from Argo) is mostly a matter of registering it there:

1. Add `<Kind> *UpdateMode` to `UpdateTypes` in `api/v1alpha1/policy_types.go`, add the case to `UpdateTypes.ModeForKind`, then run `make generate` and `make manifests`
2. Add the kind to the `ownerKinds` table in `internal/workload/kindobjects.go` (object and list constructors plus its `GroupResource`) and, if the controller and dashboard should list it, to `workload.SupportedKinds`. This single table drives the inventory's listing (`internal/inventory`, which both the controller and the dashboard read identities from), the dashboard's NotFound errors, and the workload-level annotation reads in the webhook and the OOM watcher. A kind missing here silently loses workload-level opt-in (namespace-level and pod-template-level keep working, since neither depends on this table)
3. Add the kind's pod template to `workload.PodTemplateOf` in `internal/workload/templates.go`: the inventory decides governance on it, so its annotations are the pod-template opt-in level and its labels what a Policy's label selector matches
4. Add the same kind to `k8s.OwnerChainDisableFor()` in `internal/k8s/client.go`. Every kind in `ownerKinds` must appear here too, or its first Get on the admission hot path stands up a cluster-wide informer over every object of that kind instead of costing one Get. `TestDisableForCoversOwnerAnnotationKinds` (`internal/webhook/optin_test.go`) cross-checks the two lists and fails if one is missing from the other
5. If the kind is a CRD, register its scheme in `internal/config/config.go` and `internal/dashboard/server.go`
6. Give the kind its apply rule in `applyRules` (`internal/workload/apply.go`): where a member's pods come from (`selectedPods` with the kind's selector for a workload whose pods it owns through a selector, a function of its own otherwise) and whether the kind is `inPlaceOnly`, its pods never evicted (like Job, CronJob and bare `Pod`). `TestApply_EveryKindHasAnApplyRule` fails for a kind of `SupportedKinds` without a rule, and `TestApply_EveryKindUnderEveryMode` (`internal/workload/apply_test.go`) needs a fixture for it
7. Add RBAC markers (`+kubebuilder:rbac:...`) to the controller and the Helm RBAC rule in `charts/k8s-sustain/templates/rbac.yaml`

## Adding a recommendation signal

A [signal](concepts/recommendation-pipeline.md#signals) is one module of
`internal/recommender` (`signal_usage.go` holds the CPU and memory usage
signals, `signal_oom.go` the OOM floor) implementing the `signal` interface in
`signal.go`: its query and shard cost, what its history reads besides
(`historyQuery`), its Prometheus contract (`contract`),
how it collects samples into its values (`collect`), its failure policy, where
its stage runs (`slot`: base, adjuster or limit adjuster, CPU or memory), its
contribution to a container's request or limit, and its trace stage. Its
values are a type of its own behind the `signalValues` interface
(`inputs.go`): the containers they give something to recommend from
(`observed`), and whether they excuse a Too young identity from the age gate
(`excusesAge`). `WorkloadInputs` holds every signal's values keyed by the
signal, and a signal reads its own back with `valuesOf`, so a new signal adds
no field to any shared type.

`PromInputs` fetches every signal in the `signals` list, as of the instant
the caller gives (`FetchInputs`) or at every instant of a span
(`FetchHistory`), and `Compute` runs
one chain per resource in a fixed order: on the request side base signals,
adjusters, headroom, clamp and coordination; on the limit side the Policy's
limit strategy, limit adjusters and the guard (`limit.go`, whose
`limitStrategies` is the one list of limit strategies, which `WithDefaults`
reads too). Both sides run their signals alike (`contributionsTo`, then
`stagesOf` for their trace stages), and every stage of a signal, its
contribution and its trace entry, sizes with one `stageContext`: the
identity's inputs, the resource's Policy config, the container as the
identity holds it (the union of its members', with the requests and limits it
declares, not a live pod's), the instant the Request computes as of, and on
the limit side the final request. Each role combines its contributions its
own way: base contributions by taking the highest, a tie going to the first in
`signals` order (`requestOf`); an adjuster's only when it is above the value
so far; a limit adjuster's only when, rounded up to a whole millicore or MiB,
it is above the limit the strategy set. The trace marks the contribution that
set the final value as the determining one. Neither side has a line specific
to any signal: the age gate asks every signal's values, and live OOM kills
reach the OOM floor through the inputs port (`WithLiveOOMs`), not through
`Compute`.

Outside the recommender, inputs answer only the questions every reader
shares: `Resolved` (whether any container's usage was observed, the
controller's coverage count) and `Usage` (a container's usage of a resource,
which a Simulation shows). Add another only when a reader needs it, never a
signal's values.

A signal's range form comes for free from its query. `FetchHistory` runs each
signal's own `query` as a Prometheus range query (`Client.QueryShardRange`)
over a `Span`, under the same sharding, retries and fallback, with shards
planned on the signal's samples per container times the span's instants, and
collects each instant's samples with the signal's own `collect`. Only the OOM
signal adds anything: its `historyQuery`, run in the same shard, counts per
step the kills a chart marks, read from the kill rule its contract declares
as a history rule, which no instant of its 24h rules can date. Its `collect`
keeps them in its own values, at the instant ending their step, and a chart
reads them back through `OOMKills`, so no shared type holds them.
`ComputeRange` then runs the whole chain at each instant on that
instant's values, the instant in the `stageContext`, without reading anything
else; it is what a Simulation's charts draw. `ReadRange` makes the same read
for an identity under a Policy's spec, every default explicit, and computes
nothing: it returns, for each container the assembly would size, what the
usage signals read at each instant, and its kills come through `OOMKills` as a
Simulation's do. It is what the workload detail page draws next to the replica
average: the signal line and the OOM markers. To add a signal:

1. Write `internal/recommender/signal_<name>.go`. Build its PromQL from the
   `internal/prometheus` primitives (`Shard.Selector`, `Shard.MetricsSelector`,
   `QuantileOverTime`, `WindowMinutes`); `PromInputs` runs it through
   `Client.QueryShard`, with the batching, retry and one-identity fallback
   every signal shares. Give its values a type of their own implementing
   `signalValues`, whose zero value holds nothing, and export the
   `InputsOption` that sets them (`WithOOMKills` does, through `setting`):
   it is how `recommendertest.StaticInputs` serves them, at one instant
   (`Set`) or at each instant of a history (`SetHistory`). There is nothing
   to do for the Simulator's charts: the signal's history is its query run
   over a range, and its stage runs at every instant of it. Its
   `historyQuery` returns nothing unless that history must date something no
   instant of its query can, as the OOM floor's kill markers
2. List it in `signals` (`internal/recommender/signal.go`). Within a role,
   stages run in list order, which settles a tie between base signals. A base
   signal or adjuster shapes the request; a limit adjuster may only raise the
   limit the strategy set, never remove it or give one to a container
   without, and the guard still raises a limit left below the request
3. Its trace stage is a `TraceStage` it builds itself: its name, the value it
   contributed and any facts as key/values. The trace is a list, so a new
   stage needs no CRD, `api.ts` or dashboard change (the dashboard shows an
   unknown stage under its own name; add a display name to
   `RecommendationTrace.vue` if you want a nicer one). A consumer that needs
   one of its facts reads it through a function of the signal's module, as
   the controller reads the OOM floor's with `TracedOOMFloor`. If it reads new
   Policy settings, add the fields to `api/v1alpha1` (`ResourcesConfigs`),
   then run `make generate manifests`
4. Declare its Prometheus contract next to its query, in its `contract`
   method: the recording rules the query reads, the labels each aggregates by
   (`shardLabels`, the identity and the container), the evaluation interval
   its shard cost assumes (zero if none), any rule only its history read
   reads (`HistoryRules`, as the OOM floor's kill rule), the cAdvisor and
   kube-state-metrics series those rules compute from, through every rule
   they read in turn, and the cAdvisor series a rule reads per container
   instance (`CAdvisorPerInstance`, as the OOM floor's memory limit). Then run
   `go test ./hack/signalcontract`: it names each rule missing from
   `prometheusRule.groups` in `charts/k8s-sustain/values.yaml`, aggregating by
   other labels or evaluated at another interval, each rule no promtool
   fixture evaluates, each series the contract leaves out or the bundled
   scrape keep-lists drop, and each series read per instance whose instances
   the bundled cAdvisor scrape merges. Fix them in turn: the rule name in
   `internal/prometheus/metricnames.go`, the rule in `prometheusRule.groups`, a
   fixture in `charts/k8s-sustain/tests/promtool/` (run `make helm-promtool`),
   the series in the cAdvisor or kube-state-metrics keep-list of the bundled
   Prometheus (`prometheus.scrapeConfigs.kubernetes-nodes-cadvisor` or the
   `kube-state-metrics` job in `prometheus.extraScrapeConfigs`). Document the
   rule and the contract in [Recording Rules](reference/recording-rules.md#signal-contracts),
   and the series in [Prerequisites](getting-started/prerequisites.md#prometheus)
   for users who bring their own Prometheus
5. Test it at its own interface, next to `signal_usage_test.go` and
   `signal_oom_test.go`: the query built from settings, the values collected
   from `ShardSample`s, its failure policy, and its contribution and trace
   stage from a `stageContext` the test builds (`stageFor`), with no
   Request. Test what it does to a whole recommendation through `Compute`,
   from a `Request` the test builds in the package at a fixed instant (`now`;
   outside the package only `Assemble` builds one) with inputs from
   `NewInputs`. `signal_test.go` shows that a signal defined only in a test,
   with values of its own type, is fetched and then computed on the limit
   side with its own trace stage from what was fetched, from its list entry
   alone; that every stage of a signal gets its context; that base signals
   combine by max; and it checks that every listed signal's contract declares
   exactly the rules its query reads and that its `collect` reads a sample of
   each of them, so no series its query returns is dropped for a metric name
   `collect` does not handle
6. Add it to the signals table in
   [Recommendation Pipeline](concepts/recommendation-pipeline.md#signals) and
   describe its stage there

## Dashboard health signals

Every dashboard view — the Overview, both workload lists and the detail page —
is a view of one **board** (`internal/dashboard/board.go`): `Server.board`
takes inventory identities, reads their health — OOM kills in 24h, Blocked,
stale and total pods, whether an autoscaler is present — in one batch through
the `HealthSignals` port (`internal/dashboard/health.go`), sets Conflicted
from the inventory, and returns one status per identity: its health, its Risk
state and the coordination factors of its stored trace. The port's Prometheus
adapter (`health_prometheus.go`) holds the only health PromQL; tests use the
in-memory `memHealthSignals` instead of faking query text. The Risk state is
classified in exactly one place, `riskStateOf`, called only by the board, and
returned in every payload (`riskState`); the SPA renders it and never derives
its own state or count.

The Overview (`/api/summary`) and the Workloads list (`/api/workloads`) read
the board of the same identities (`Server.clusterIdentities`: every kind,
`--excluded-namespaces` applied), so every Overview count equals that list
filtered by the same Risk state: a Prometheus series for an identity the
inventory does not hold (deleted, never inventoried) counts nowhere. The
Overview's Risk-state and Coordinated KPIs, its attention lists (the worst ten
per state, with each state's true total) and its per-Policy Blocked counts all
come from that one board. Its inventory read happens inside the summary
recompute, so it is cached and single-flighted with the rest of
`/api/summary`. The Policies pages still read their Blocked counts from the
controller's `k8s_sustain_policy_blocked_count`.

Every endpoint builds its identities from `internal/inventory`
(`Server.identities` / `Server.identity`), so a row, a detail page, an Overview
count and the controller agree on members, containers, age, Departed and the
governing Policy. The detail page shows the Recommendation stored in the
identity's `WorkloadRecommendation`, its `status.outcome` and its
`status.trace`; only the Simulator recomputes. Its charts draw two usage lines
per resource: the replica average, read through `PromQuerier`, and what the
usage signals read over the chart range under the governing Policy's spec,
read through the `Inputs` port with `recommender.ReadRange` (no PromQL in the
dashboard, nothing computed from it), and the OOM markers come from that same
read (`recommender.OOMKills`). An identity no Policy governs has no signal
line, and its history is read under the recommender defaults for its kills
alone; a read that fails leaves the page standing with a `signalError`. Coordination factors are not a
health signal: the board reads them from the coordination stage of the stored
trace (`coordinationFactorsOf`, through `recommender.TracedCoordination`).

The port works because the controller publishes every health metric under
the identity's labels: the [ledger](#apply-results-and-the-ledger) aggregates the
apply outcomes of an identity's members (pod counts summed, Blocked if any
member is), and the [series publisher](#metric-series) deletes the series of
identities no Policy includes any more. A new health signal therefore needs
both halves: an identity series in the controller's publication and a field
on `identityHealth` filled by the adapter.

## Dashboard Simulations

A Simulation (`runSimulation` in `internal/dashboard/simulate.go`) is the
controller's computation of one identity under what-if settings. It reads the
identity from the inventory (one it does not hold is simulated from what
Prometheus reports), starts from the settings of its governing Policy or of
the Policy the request names (`simulationSettings`: `resourcesConfigs`,
`autoscalerCoordination`, `excludeInitContainers`), merges the request's
`override` onto them as a JSON merge patch, and hands the result to the same
[assembly](#the-request-assembly) the controller uses. Without an override it
is the controller's Request, the live OOM kills aside: the dashboard's inputs
port is undecorated. It returns the effective settings (`Request.Resources`,
every default explicit), the outcome and the trace, so the Simulator shows
the Policy's values and the recommender's defaults without holding either.

Its charts are its history: `recommender.ComputeRange` over a `Span` that
ends where the chart range does, its step derived from the range (`SpanOf`, at
most about 300 instants, the requested step only the finest), returns per container and resource the usage the signal read and the
request and limit the full chain computed at each instant, with the OOM kills.
A range back from now (`window`) ends at the instant the Simulation computes
as of, so its last point is the Simulation itself; an absolute range ends at
its `toTs`. The dashboard only renders them, next to the requests and limits the pods ran
with, which it reads through `PromQuerier`. A history that cannot be read
leaves the Simulation's numbers standing with a `historyError`.

`PromQuerier` (`server.go`) is the dashboard's other Prometheus seam: the
Overview's generic reads (`QueryInstant`, `QueryRange`, `QueryByLabel`) and
`QueryRangeByLabel`, a range read keyed by a label, through which the
dashboard reads per container an identity's running requests and limits
(`running`) and the replica-averaged usage the detail page charts. Nothing a
recommendation is computed from, and no OOM kill, goes through it.

`TestSimulationRecommendsWhatTheControllerStores` holds that parity end to
end. It lives in `internal/controller` as the external `controller_test`
package, which can import the dashboard and reach `Reconcile` through
`ReconcilerForTest` (`export_test.go`): from one fake cluster, one
`StaticInputs` and one fake clock (`PolicyReconciler.Clock`, `Server.Clock`)
at an instant far from the wall clock, it runs a reconcile, then
`POST /api/simulate` and `GET /api/policies/{name}/batch-simulate` through
`Server.Handler`, and compares their Recommendation, outcome and trace with
the stored `WorkloadRecommendation`'s, for an owner-name group coordinated
against one member's HPA and for an identity Too young at that instant, with
init containers excluded and not. It also compares every fetch the dashboard
made with the controller's: the settings (windows, percentiles, every default
explicit), each identity's container count and the instant every fetch was
evaluated as of, and checks that the last instant of each Simulation's
history computes the stored Recommendation, request and limit. A change to
how either side builds the identity, picks the spec or dates the computation
fails it.

The override is validated once, on the server, after the merge
(`simulationSettings.violations`, `simulation_settings.go`): it is rejected
for a rule the merged settings break that the starting Policy did not already
break, since a Policy admitted before the CRD gained a rule is still what the
controller computes with. The rules are a copy of the CRD's markers in
`api/v1alpha1`; `TestOverrideRulesCoverTheCRD` walks the generated CRD and
fails for a rule (a minimum, maximum, pattern, quantity or CEL message on
these settings) that no case of `TestOverrideRejectsWhatTheCRDRejects` breaks,
and for any schema keyword it cannot read, so a rule added to the API types
needs its copy here.

Batch simulate is a Simulation of every live identity a Policy governs with no
override, in one assembly: it reports each identity's outcome, and counts only
`Computed` ones toward the savings, against the requests their containers run
with.

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
npm run typecheck  # vue-tsc -b --noEmit (follows the tsconfig project references)
npm run lint       # eslint src/
```

The dev server proxies `/api` and `/healthz` to `localhost:8090`: run the
backend with `go run main.go dashboard --bind-address=:8090`, or port-forward
the in-cluster dashboard Service to `:8090`.

### Domain vocabulary

The SPA renders what the backend decided and holds each domain vocabulary in
one module under `src/lib/`; views and components import from it and never
spell a label, class or link of their own:

- **`risk.ts`** — every Risk state's label, badge class and link to the
  Workloads list filtered to it, in the backend's precedence; the non-Safe
  states (`attentionStateMeta`) add what puts an identity there and the tone
  of a non-zero count. The state itself always comes from the payload's
  `riskState` and the counts from the payload: the SPA never derives either.
- **`outcomes.ts`** — the label of each `status.outcome` a
  `WorkloadRecommendation` or a Simulation reports (`outcomeLabel`).
- **`limits.ts`** — the limit strategies in the recommender's precedence:
  each one's label, help per resource, and the label the trace table gives the
  stage it runs as; `limitModeOf` reads a Policy's strategy and `limitsPatch`
  writes one as a merge patch.
- **`patch.ts`** — typed key paths into settings and JSON merge patches:
  reading a value at a path, whether a patch names it, and setting it. The
  Simulator edits its override and fills its form from the server's effective
  settings only through these.

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

### Cluster-scope settings

The settings every component must read alike — `--recommend-only`,
`--excluded-namespaces` and the Prometheus connection (`--prometheus-address`
and the transport flags) — are root persistent flags, bound once by
`config.BindGlobalFlags` to one viper key and one environment variable each,
and loaded once into one type each: `config.ClusterScope` (recommend-only,
excluded namespaces), embedded in every subcommand's config, and
`config.PrometheusConnection` (address, transport), held by the controller's
and the dashboard's. A subcommand that has no use for one ignores it: the
webhook accepts the Prometheus flags, and its config has no connection. A new
setting the components must agree on goes there, never in a subcommand's
`Bind*Flags`, where its key would be the subcommand's own.

The chart renders them through one helper, `k8s-sustain.clusterScopeArgs` in
`_helpers.tpl`, which every Deployment includes; the Prometheus connection only
for a component that queries Prometheus. `k8s-sustain.excludedNamespaces` is
the one list (the release namespace, `kube-system`, `kube-public`, then
`excludedNamespaces`) behind every `--excluded-namespaces` and the webhook's
`namespaceSelector`. In the binary, each reader holds the excluded namespaces
in its `governance.PolicyLister` (see [Governance](#governance)).

`hack/docscheck` guards the flags against `docs/reference/cli.md`: each command
has a row for every flag it registers and none for one it does not (a root
persistent flag is documented once, under Global flags), with the flag's
default. It also reads every flag the chart renders straight from a value: the
value's schema must accept the flag's default, and a value rendered only when
set (`with`, `if`) must default to the flag's default, since leaving it unset
falls through to the binary's. Helm value names come from `values.yaml` and its
schema, and a single backticked default in `docs/reference/helm-values.md` must
be the one `values.yaml` holds.

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
- A new transport flag goes in `bindPrometheusTransportFlags` (registered once,
  as a root persistent flag, by `BindGlobalFlags`, so the controller and the
  dashboard read one key and one env var) and `loadPrometheusConnection` in
  `internal/config/config.go`, plus the field on `promclient.TransportConfig`.
  Mirror it in `charts/k8s-sustain/templates/_helpers.tpl`
  (`k8s-sustain.prometheusAuthArgs` / `…Env` / `…AuthSecretVolumes`),
  `values.yaml`, `values.schema.json`, and
  `charts/k8s-sustain/tests/prometheus-auth_test.yaml`.

### Governance

`internal/governance` decides, for one pod template or pod, the identity it reports into, the Policy it opts into and at which level, and the governing Policy or why there is none. Its interface is one call, `governance.Decide(ctx, subject, reader)`. A subject is a workload member's pod template (`PodTemplate`), a pod (`Pod`: the webhook's, the OOM watcher's, the inventory's bare pods) or a Departed identity (`Departed`, governed by the Policy its `WorkloadRecommendation` names; it has no labels to match, so its namespace is checked and its Policy's selector must parse, as for a live member). The identity is a `promclient.WorkloadIdentity`, the key the inventory, the OOM cache, the WorkloadRecommendation and the recording rules all use, and a governing decision carries the mode the Policy manages the identity's kind in. The `Reader` port supplies what the subject does not carry: the Policies (a `governance.Policies`: every Policy with its label selector compiled, and the excluded namespaces), a pod's owner, a workload's own annotations and a Namespace's. Each caller adapts it to its own reads: the inventory serves the Policies it listed and memoised Namespaces, the webhook lists Policies from its informer cache and reads the owner chain through its TTL caches, the OOM watcher reads through the manager's client and leaves a kill unresolved when the owner lookup fails. All three list Policies through `governance.PolicyLister`, which compiles a Policy's selector once per version of it (UID and resourceVersion) and holds the excluded namespaces, given once at construction (`governance.NewPolicyLister(cfg.ExcludedNamespaces)` in each subcommand): no reader passes them on a call. Every caller keeps one lister for the process, so an unchanged Policy set is never recompiled: the webhook and the OOM watcher, which list on every admission and every kill, and the controller and the dashboard, which hand theirs to every snapshot they take (`inventory.Basis`). Only `NewPolicyLister` builds a lister that lists: a nil or zero-value one would silently exclude no namespace, so its `List` fails (`governance.ErrNoPolicyLister`), and so does a snapshot whose `inventory.Basis` holds none. The lister lists without deep copies (`client.UnsafeDisableDeepCopy`), so a Policy it returns shares its fields with the informer cache's object and with concurrent decisions: governance and every caller of `Decide` treat it as read-only.

Behind it: the opt-in walk (pod template, then workload, then Namespace; an opt-out or a non-empty policy decides, an empty value falls through), each level read only when every more specific one is silent and a pod's owner only when the walk needs it; the pre-gate, which before the first read checks that some Policy accepts the subject's namespace and labels by the same rule as the full decision (a Policy whose selector does not parse counts as accepting, so opting into it is refused `selector-invalid` at every level rather than `no-policy-selects` at some); the Policy's consent (it exists, the namespace is neither excluded nor unselected, the label selector is valid and matches the pod template's labels, the kind is managed); and the owner-name override, applied there and nowhere else.

A governance rule changes in this module and its contract table (`internal/governance/governance_test.go`, a map-backed reader that records its reads), never in a caller.

### Controller

#### Identity inventory

`internal/inventory` is the one place workload objects become identities. Its interface is one call, `inventory.Take(ctx, reader, Options)`, plus `inventory.TakeForPolicy(ctx, reader, policy, Basis)`, which narrows it to what a Policy can govern (its selector's namespaces and the kinds it manages, reading nothing for a Policy that manages none). A `Basis` is what a reader decides and dates every snapshot with, its Policy lister and its clock, the reader's own: a snapshot missing either fails, rather than exclude no namespace or fall back on the wall clock; `Options` is a `Basis` and the namespaces and kinds that narrow the snapshot, which `TakeForPolicy` takes from the Policy. Both return a `Snapshot`, dated `TakenAt` by the Basis's clock when it began reading, of `Identity` values (key, live members each with the Policy governing it, union containers, `Since`, governing Policy or `Conflicted`, stored `WorkloadRecommendation`). Behind it: listing every kind in scope (a kind the cluster does not serve, such as Argo Rollouts without its CRD, has no identities rather than failing the snapshot), skipping CronJob-owned Jobs and finished standalone Jobs, owner-name and bare-pod grouping, each member's [governance decision](#governance) (`governance.Decide` on its pod template, reading a Namespace only when the object's own annotations leave it undecided), the Conflicted rule, and attaching each stored `WorkloadRecommendation` (which is what makes a Departed identity).

The controller takes one `TakeForPolicy` snapshot per reconcile through its informer cache, dated by `PolicyReconciler.Clock`, and works on `Snapshot.GovernedBy(policy)`. Every member of an identity shares its namespace and kind, so the narrowing never splits one, and every identity in it has a kind the Policy manages. The dashboard takes snapshots through its uncached client, dated by `Server.Clock`, narrowed to what each view shows: one namespace and kind for a detail page, `TakeForPolicy` for a Policy's list, everything for `/api/workloads`.

A new identity rule goes in the inventory and its contract tests (`internal/inventory/inventory_test.go`), never in a caller.

#### Informer lag

The controller's reads go through a watch-populated informer cache, so a `WorkloadRecommendation` that the pass's claim created moments earlier in the same reconcile is often not yet visible, and nothing watches `WorkloadRecommendation` to re-trigger a reconcile when it becomes visible. The work-list is built from the snapshot's identities, not from the listed objects: a live identity whose object the cache cannot see yet is computed from its members' containers and dated by its earliest member, and its decision is recorded into the object `wlrcache.Ensure` returned, never into one re-read from the cache. Without that a freshly matched workload would wait a full `--reconcile-interval` before being computed.

#### Per-identity computation under owner-name grouping

The computation unit is the identity, not the workload object: a group of workloads sharing an owner-name produces exactly one computation and one write, against the union of the members' containers. Computing per member would give the single shared `WorkloadRecommendation` several competing answers, with the survivor decided by whichever member's goroutine finished last. Retry backoff is also per member: an identity is withheld from the fetch only when every member is backed off, so one failing member cannot deny its healthy siblings their inputs.

#### The Request assembly

`recommender.Assemble(ctx, inputs, autoscalers, now, spec, identities)` is the one place an identity becomes what `recommender.Compute` takes, for the controller's pass, a [Simulation](#dashboard-simulations) and a batch simulation alike. In: a Policy's right-sizing spec, `inventory.Identity` values, the inputs port, an `Autoscalers` port (`autoscaler.NamespacedSnapshot`, which lists each namespace's HPAs and ScaledObjects once) and a clock. Out, per identity: its `Request`, the `autoscaler.Group` of its members, or the error that made its inputs unreadable. Behind it:

- the containers recommended (`ContainersOf`, where the controller and Simulations apply `excludeInitContainers`): the identity's, its init containers too unless the spec excludes them, which also sizes the fetch; an identity with no containers at all (one the inventory has not seen) is recommended for every container its inputs report;
- the autoscaler: each member the identity's containers come from (`Identity.Contributors`) is looked up under its own name, and the first in name order that has one shapes the Recommendation (`ForIdentity`); a departed identity has none. The reads run while the inputs are fetched, each namespace's in order in a goroutine of its own, a few namespaces at once (`autoscalerReadConcurrency`), and the assembly waits for them before it returns. `NamespacedSnapshot` caches no failed read, so the assembly stops reading a namespace once a read of it failed: every identity of that namespace is recommended uncoordinated, at the cost of one failed read and one log line per namespace;
- the spec's resources with every default explicit (`WithDefaults`: the window, the percentile and headroom unless the request is kept, `keepLimit` when no strategy is set), which the fetch and the computation both use and a Simulation returns as its effective settings;
- one instant per batch, read from `now` once, before the fetch, and handed to it: every query is evaluated as of it, and the live decorator merges only the kills at or before it, so the age gate, the inputs and the live OOM window are judged as of one instant. The caller owns the clock: the controller passes `PolicyReconciler.Clock`, the dashboard `Server.Clock`. Both readers must hold one, and a test fixes the instant with a fake one.

`Request` has no exported field: only the assembly builds one outside the package, so every Request carries its instant (a zero one would make every dated identity Too young) and the identity it fetches for, which `ComputeRange` reads the identity's history with. The dashboard never reads a signal's values itself.

#### The Policy pass

`Reconcile` reads the Policy, handles deletion and the finalizer, takes the `inventory.TakeForPolicy` snapshot, and runs one pass over it (`runPass` in `internal/controller/pass.go`). Everything after the pass consumes the **report** it returns, one entry per identity on the work-list (its key, whether it is departed, whether it was fetched and, if so, its outcome, whether its fetch resolved, whether a departed identity failed, one `applyResult` per live member, the Recommendation a live identity holds for the Policy after the pass, and what computing it found: its autoscaler, whether its members disagree on it, and the containers the OOM floor determined): the [ledger](#apply-results-and-the-ledger) settles the member results (`Ledger.Settle`) and answers each live identity's health, its failed members and the Warnings to emit, which `warn` emits, the [series publisher](#metric-series) publishes what the report and that answer describe, the [sweep](#sweep-grace) runs against the snapshot, as of its `TakenAt`, `passCondition` derives the `Ready` condition from the identities that failed (`passReport.failed`) and those Blocked, and the ledger schedules the requeue. The pass keeps no state between passes, and no step reads another's struct fields: each takes what it needs from the earlier steps' return values, in the order the pass calls them.

- **Work-list.** The identities the Policy governs, taken from the inventory as they are: a departed one's containers come from its stored snapshot, and one without a snapshot is on it but not fetched (counted as `no-snapshot` in `wlr_refresh_total`), so it keeps its series, as every departed identity does, until its `WorkloadRecommendation` is swept. Each live member is a `workloadTarget` (`targetsOf`; a bare-pod identity's governed pods form one), and the [ledger](#apply-results-and-the-ledger) is asked once, before the fetch, which members may run: backoff is time-based and the fetch can take minutes, so asking again at apply time could process a member the fetch left out.
- **Claim.** `wlrcache.Ensure` for every live identity, with its members' container union as the observed snapshot. It returns the object each decision is recorded into: the one `Ensure` wrote, or the one the inventory read for a departed identity. A failed claim is logged and kept for that identity alone.
- **Recommend** (`recommendation_pass.go`). One [assembly](#the-request-assembly) of every identity not wholly in backoff, under the Policy's right-sizing spec: one `InputsFetcher` call per policy, so the pass never sees shards, retries or the fallback. The controller's fetcher is `PromInputs` decorated by `recommender.WithLiveOOMs`, which merges the kills the OOM watcher's cache holds as of the pass's instant into each identity's OOM inputs; the dashboard uses the undecorated port. Then `computeIdentity` per fetched identity, in parallel: it keeps, for the [series publisher](#metric-series), the autoscaler the assembly's `autoscaler.Group` shaped the identity with and whether the group's members disagree on theirs, and `recommender.Compute` decides everything about one identity from its Request: the age gate, then every container through the chain of stages of every [signal](#adding-a-recommendation-signal). The OOM floor signal is the one place that decides whether a kill is recent, for both its answer to the age gate and the memory floor; the decorator decides whether a live record is still within `LiveOOMWindow`. The dashboard's simulations assemble and compute the same way, so what they show is what the controller would apply. Alongside each container's values it returns their [trace](concepts/recommendation-pipeline.md#trace), the list of stages that ran on the request and the limit; the record stores it with the values, and the controller's computation metrics (`k8s_sustain_oom_floor_applied_total`, `…_oom_reaction_latency_seconds`) read its OOM floor stage (`recommender.TracedOOMFloor`). Nothing outside the recommender re-runs a stage to find out what it did: a new question about the computation is a new stage or fact. Every identity leaves with one outcome: a recommendation, too young, no data, fetch failed, or not fetched (every member in backoff). Each but the last is the `status.outcome` the record stores; `recommender.Outcome.Recorded` names a computed one that way, for the controller and the dashboard's Simulations alike.
- **Record.** `wlrcache.Record` of every outcome but "not fetched", into the object the claim returned, before anything is applied, so the webhook serves the new value by the time a replacement pod is admitted. It keeps the last Recommendation for every outcome but `Computed`, and marks a departed identity departed. Because the record takes the claim's object as an argument, it cannot record into a copy read before the claim, which would attribute an adopting Policy's numbers to the previous one.
- **Apply** (`workload_reconcile.go`). One task per live member not in backoff. A live identity whose decision could not be stored — its claim failed, or the record was rejected — is not applied that cycle, and each of its members fails on the `store` step: the replacement pods of an eviction would read the old value. That is the one count the failure gets.

The batch metrics are counted from the report's outcomes, not from the fetch's internals. A departed identity has nothing to apply: its report entry carries its failure (a failed fetch or record), which `passReport.failed` counts.

A Conflicted identity never enters the pass's work-list. Each Policy party to the conflict records `status.outcome: Conflicted` (`recordConflicted`, the first thing the pass does) and nothing else; `wlrcache.Ensure`, which rewrites `spec.policy`, is only ever called by the governing Policy, which is what stops two Policies flipping one object between them.

Tests drive `Reconcile` end to end with CRD-valid Policies (their kinds' update modes set) and members with UIDs, as the inventory and the patcher need; only the ledger's own tests feed it hand-built results, as plain values at its interface. What applying does to pods is tested at `workload.Apply` itself, on a fake client holding pods only; the controller's own apply tests cover what it does with the result: events, the ledger, recommend-only.

#### Applying to a member

The apply step is one call per member: `Patcher.Apply(ctx, member, recs, settings)` in `internal/workload/apply.go`. In: the member (its kind and object, or for a bare-pod identity the governed pods the inventory holds, each its own member object, and the containers it declares), the identity's Recommendation, which Apply narrows to those containers by [container injection](#container-injection), and the Policy's choices, `workload.ApplySettingsFor(policy, kind, --recommend-only)`: recommend-only (and what asked for it, the flag or the Policy, which its log names), a dry run for `OnCreate`, the downsize tolerance with its defaults, the `safe-to-evict` override. Out: a `workload.Result`, classified as done (the pods were driven toward the Recommendation and counted), idle (nothing to apply: no Recommendation for the member's containers, or recommend-only; no pod is read), counted-only (a dry run counted the pods still waiting, or could not list them, which is no failure since it touches no pod) or failed at a step (`patch` for kinds that evict, `resize` for in-place-only ones, the kind's family deciding). Alongside: the pod counts, the pods resized in place and evicted, the containers whose resources changed on them (what the resize wrote, or what the evicted pod's replacement runs differently from its running resources), and the decreases the tolerance withheld per resource.

Behind it, one rule per kind (`applyRules`) says where a member's pods come from and whether they may be evicted: the selector plus the ownerRef UID for Deployment, StatefulSet, DaemonSet and Rollout; the active Jobs, then each Job's `batch.kubernetes.io/job-name` label plus its UID for a CronJob; the label plus the UID for a Job; the inventory's pods for a bare-pod identity. CronJob, Job and bare `Pod` are in-place-only. Everything else is the patcher's: the ownership check, StatefulSet ordering, the choice between an in-place resize and an eviction, the replacement wait and the crash-loop halt, the in-place → eviction fallback, the dry run and the counts. A member without a UID, a workload without a valid selector or a kind without a rule is refused with an error wrapping `workload.ErrRefused`, never applied to every labelled pod or, silently, to none; the ledger classifies a refusal as permanent.

One predicate, `planFor` (`internal/workload/match.go`), decides for each pod both what to apply and whether it counts stale, so the two cannot disagree: the Recommendation with the decreases the tolerance withholds on that pod cleared, and the pod's change, none, to limits alone, or to a request (a limit the guard raises with a rising request belongs to the request change). Only a request change makes a pod stale and may evict it. A change to limits alone is resized in place where the cluster supports it and otherwise left to the pod's replacement, and a staged resize the kubelet reports `Infeasible` or `Error` is evicted only when the requests the kubelet enacted differ from the spec's (or are not reported).

The controller keeps what Apply does not decide: shutdown, the fetch failure (`prometheus`) and the unstored Recommendation (`store`), on which it fails the member without calling Apply, the `AutoscalerDetected` and `ResourcesUpdated` events, and the member's result for the ledger.

#### Apply results and the ledger

The apply step keeps no retry or health state. It returns exactly one `applyResult` per live member (`workload_reconcile.go`), which holds the `ledger.Result` the ledger settles and the decreases the downsize threshold withheld, mapped from Apply's `Result` where it called Apply: applied (done or counted-only, with its pod counts), nothing to apply (idle), uncounted (a counted-only result without counts, which is not a failure: backing it off would leave the identity unfetched and freeze the Recommendation the webhook reads), skipped (retry backoff, or shutdown), or failed on a step (`workload.Step`: `prometheus`, `patch`, `resize`, or `store` for a member withheld because its Recommendation could not be stored).

`Reconcile` hands the member results of every live identity in the pass's report to the ledger (`Ledger.Settle` in `internal/ledger`), the one place a verdict is taken. It holds, per member, keyed by kind, namespace and name (`ledger.MemberKey`), whether it is Blocked and on which step, its last measured pod counts and when its retry is due, and, per Policy, the members its last pass reported. It answers whether a member may run this pass (`MayRun`), each identity's [health](#identity-health), the Warning each result calls for, and when the next pass is due (`RequeueAfter`):

- A failure is shutdown, and records nothing, only when the reconcile's own context has ended. The pass decides it once, after the apply step, for live members and departed identities alike: a pass cut short by shutdown turns every failed member into a skipped one before the ledger sees it, and fails no departed identity. An error that merely wraps `context.DeadlineExceeded`, as a Prometheus call that ran out of its own timeout does, is a transient failure.
- `permanentError` classifies the rest: an `Apply` refusal (`workload.ErrRefused`), or an API rejection other than 404, 409 and 429 anywhere in the error tree (one forbidden pod among several pod errors is enough), is permanent; everything else is transient. A 409 Conflict is an optimistic-concurrency loss the next attempt re-reads past, and a 404 an object deleted since it was read: a `WorkloadRecommendation` deleted between its claim and its record is re-created by the next claim.
- A transient failure backs off (30s, doubling with each consecutive transient failure, to 5m) and emits `ReconciliationRetryScheduled` on every attempt, naming the attempt and when the retry is due. A permanent one never backs off and does not grow the backoff: a transient failure after it waits 30s again, so a fixed cause is picked up at the next pass, and emits `ReconciliationFailed` only when the member enters it.
- Every failure counts toward the pass's failures and leaves the member Blocked until a later result other than skipped clears it.
- `Reconcile` requeues at the reconcile interval or at the earliest due retry of the live members the pass reported, whichever comes first, so the time a retry event names is a time a pass runs. A pass that failed to list its workloads (`ListFailed`) has no report: the ledger reads the live members of the Policy's last pass, and requeues no sooner than the base retry delay (30s) (`RequeueAfterListFailure`), since a retry already due cannot run until a pass lists again. Its `Ready` condition is False while an identity failed this pass or any of its identities is Blocked, so a pass that skipped a backed-off member never reports every workload processed. Its counts are of identities: one with any member failed counts once. The `For(&Policy{})` watch passes only generation changes, so the controller's own status write does not start a pass.

The ledger takes an injected clock (`k8s.io/utils/clock`), so tests drive backoff with a fake clock instead of sleeping. It does not import the controller: it takes its own results, `ledger.Result` (a member key, what the pass did as a `ledger.ResultKind`, the pod counts, the failed `workload.Step` and its error) and labels its Warnings and the Blocked reason with the step. Departed identities have no members and never reach it: the pass decides once whether a departed identity failed (its fetch or its record did, and the pass was not cut short by shutdown), and both the count of failed identities (`passReport.failed`) and its `wlr_refresh_total` outcome read that one field of its report entry, so `error` is exactly a departed identity counted as failed.

#### Container injection

`workload.Inject(containers, initContainers, excludeInit, recs)` (`internal/workload/inject.go`) is the one place a Recommendation meets a container list, for the controller, the patcher and the webhook. It narrows the Recommendation to the containers declared (init containers too unless `excludeInit`; a name declared twice gets the one recommendation in both places) and returns, for each container whose resources change, what to set and where. It guarantees requests ≤ limits on what is finally written: when the request a container ends up with exceeds the limit it ends up with, the limit is raised to the request and named in `RaisedLimits`, which `ContainerWrite.LogRaisedLimits` logs at debug for the patcher and the webhook alike. That covers a recommended request above a running limit the recommendation keeps, and a request decrease the downsize tolerance withholds while letting the limit's through. A container's change (`containerChange`) is defined by what applying writes, so the patcher's stale count and its resize agree with what a resize would write and never chase a limit the guard raises back.

`workload.Apply` narrows a member's Recommendation with it; the patcher writes an in-place resize from its writes (sidecar init containers only, a classic one having exited); the webhook's responder turns its writes into the JSON patch. The Policy's `excludeInitContainers` is applied to the identity by the [assembly](#the-request-assembly), so a Recommendation the controller computes never names an excluded init container and Apply narrows with `excludeInit` false; only the webhook still passes the Policy's flag, for a stored Recommendation computed before the Policy excluded them.

#### Owner-name group: merge order and write cost

For an owner-name group, the union snapshot in `status.observedResources` is built from the governed members' containers. Where several members declare the same container name with different requests/limits, the newest member's entry is kept whole (ties broken by name), rather than mixing one member's request with another's limit: an older member's spec may predate a change the newer ones already run. The recommendation is computed against that union, using the autoscaler of the first member, in name order, that has one (`autoscaler.Group`), and treating the identity as being as old as its oldest member or its `WorkloadRecommendation`, whichever is older.

Every choice is decided by the members' names, creation times or an aggregate over all of them, so the stored snapshot and recommendation do not depend on the order the API server lists members in, nor on which member's work finishes first. The pass's claim (`wlrcache.Ensure`) is the only controller writer of `status.observedResources`, from that union computed once per identity by the inventory, and the record never touches it, so a group whose members and metrics are unchanged costs no status write on subsequent reconciles.

A bare-pod identity's governed pods form one apply target (`targetsOf`), attributed to the newest pod for events; its pods are resized together and never evicted.

#### Identity health

An identity's health (its pod counts, the step it is Blocked on, and how many of its members failed this pass) is the ledger's answer to `Settle`, aggregated from its state of each of the identity's members. Settling a pass also makes its members the ones the Policy claims; members are unioned across Policies, so an identity moving between two keeps one aggregate while both claim it, and the ledger forgets a member once no Policy claims it, whether a Policy's pass leaves it out or the Policy is forgotten (`Ledger.Forget`, next to the publisher's, on the Policy's deletion branch and when it is found gone). A Conflicted identity is governed by no Policy, so no Policy reports it; the dashboard shows its Conflicted state from the inventory instead.

#### Metric series

The series publisher (`publisher.go`) is the only code that writes or deletes the controller's per-Policy, per-identity and per-member series. `metrics.go` declares the vectors, and writes only the two series no Policy or identity owns, which nothing deletes: `k8s_sustain_oom_cache_entries` (`SetOOMCacheEntries`, the OOM cache's size observer) and `k8s_sustain_wlr_refresh_total`, labelled by namespace and kind only (`EmitWLRRefresh`, counted from each departed identity's report entry). Each pass hands it a `publication` built from the report and the ledger's answer (`passReport.publication`): the Policy's rollup and fetch coverage, and per identity its health and failed members, what computing it found, whether it was Too young, and per live member the Recommendation it holds narrowed to its containers. Outside a pass, `Reconcile` reports each reconcile's result and duration, and the OOM watcher's handler reports each kill (`PolicyReconciler.ObserveOOM`) under the Policy it found governing the pod. `ObserveOOM` also triggers a pass of that Policy, waiting for room in the trigger channel rather than dropping the trigger: that pass is what forgets a Policy deleted by the time its kill is counted.

The publisher remembers every series it wrote, by owner, and deletes exactly those that nothing desires any more, so there is no hand-kept list of vectors to delete:

- A Policy owns its own series and its members' (`policy` label). Each publication replaces the gauges of the last one; `forget` deletes them all, on the Policy's deletion branch and at every reconcile that finds the Policy gone (its finalizer removed by hand, or a kill counted after it was forgotten).
- An identity owns the series without a `policy` label. They live while the last publication of some Policy included the identity, live or departed: its gauges are replaced by every publication that includes it, its counters accumulate, and all go once no Policy includes it, which resets the counters if it comes back. The publisher does not tell a move from a departure: an identity moving between Policies keeps its series only when the new Policy publishes first, and in the other order its counters reset and its gauges come back with the new Policy's publication. A kill `ObserveOOM` counts makes its Policy include the identity until that Policy's next publication, so a pass already in flight that leaves the identity out drops the kill. The autoscaler gauges are the exception to replacement: an identity the pass did not compute keeps the ones its last computation set. They are the autoscaler the assembly found, departed identities included, which it finds none for: nothing else in the controller special-cases a departed identity's autoscaler.

A member's Recommendation series show what the identity holds for the Policy (`wlrcache.Held` when the pass recorded nothing new), never only what the pass computed, so a fetch failure or a backoff does not drop them. They are narrowed to the containers the Policy recommends of the identity (`recommender.ContainersOf`, where `excludeInitContainers` is applied), so a Recommendation kept from before the Policy excluded init containers does not show them. A new controller series is a field of the publication and a line in `publish`.

### OOM watcher

#### Cache: per-field merge

The OOM cache is keyed by `(namespace, ownerKind, ownerName, container)`, so every pod of a workload writes to the same slot, and pods are reconciled in parallel. The slot is a per-field merge rather than last-write-wins:

- **Identity and timestamps** (pod name/UID, terminated-at, observed-at, restart count) come from the newest observation, ordered by `(terminated-at, restart-count)`. Restart count only breaks ties on an equal timestamp, which `metav1.Time`'s one-second resolution makes common; across two pods the counters are unrelated, so a cross-pod tie picks arbitrarily between two equally recent stamps.
- **The OOM-time memory limit** (the bump anchor for the memory floor) is the largest limit seen across those pods, not the most recent. A Deployment bumped 128Mi → 256Mi can have an old un-resized pod (128Mi) OOM after an already-resized one (256Mi); anchoring on the newer 128Mi would bump the floor to a value 256Mi already proved insufficient. The recommender `max()`es this record against the Prometheus anchor for the same reason.

An out-of-order observation still counts as a new kill for triggering an immediate reconcile; it contributes its limit, not its identity. The accepted cost is on a deliberate downsize: an older, larger limit keeps anchoring the floor high until it ages out of the cache, after `recommender.LiveOOMWindow`, which is both the cache's TTL and how long the OOM floor counts a kill as recent. That errs toward brief over-provisioning rather than an under-bump into another OOM, and it cannot compound, because every merged value is a limit the kernel actually applied.

The watcher captures the limit from `ContainerStatus.Resources` (falling back to the pod spec when the status carries none). The spec holds the desired limit, which the recommender itself rewrites on every in-place resize, so anchoring on the spec would feed the OOM floor its own previous output and compound on each kill.

#### Kills, predicate and dedup

One function, `oomKills`, turns a pod into the OOM kills its status shows, by the definition the `k8s_sustain:container_oom_kills` recording rule counts: a container's last termination that is `OOMKilled` (it restarted after the kill), or, in a pod whose `restartPolicy` is `Never`, its current `OOMKilled` termination. Under `Always` or `OnFailure` a current termination is only the moment before the restart, which counts the kill. Each kill carries its container, restart count, terminated-at and the limit it was killed at; the event predicate, the dedup and the record all read these kills and nothing else of the pod's status.

The event predicate is local (no apiserver calls): a pod is queued when it carries the `k8s.sustain.io/policy` annotation itself or shows an OOM kill. The OOM arm is sticky — a kill stays in the pod's status for the rest of its life — so every later event for such a pod re-queues a reconcile. Before resolving the owner or Namespace, the watcher checks whether each kill (pod UID + container + restart count + terminated-at) was already resolved, so a chronically restarting pod's owner resolution runs once per distinct kill, not once per status write, including for pods managed by no Policy.

A kill is marked resolved only once every read its pass needed succeeded. When the pod's owner chain cannot be read (its ReplicaSet or Job Get fails), the watcher records nothing and marks nothing, and requeues the pod (`ownerRetryAfter`): a terminated `restartPolicy: Never` pod may get no further status event to retry on. The requeue waits as long as the newest unresolved kill has waited so far, at least 5 seconds, so retries back off geometrically without counting attempts, and stops once that kill is older than `recommender.LiveOOMWindow` (`Watcher.Window`), past which a kill adds nothing to what the recording rules counted. Whichever of the requeue and a status event comes first retries the walk, and the kill is recorded once: the other finds it resolved. It never falls back to the pod's immediate controller, an identity the inventory does not hold for a Deployment's or a CronJob's pods. A failed read of the Policies or an annotation level is returned, so controller-runtime requeues with backoff.

### Webhook

#### Admission decision

`admit` (`internal/webhook/handler.go`) is decode → decide → run effects → respond. `decide` (`decide.go`) works out what admitting a pod does from reads alone; its only writes are the owner caches those reads fill, from `singleflight` leaders that can outlive it. It covers the governance decision, the `WorkloadRecommendation` read and its verdict, and [container injection](#container-injection). The 4s `admissionTimeout` is split by where the reads go (`handler.go`): only governance's owner chain reaches the apiserver, so governance gets 3s (`governanceTimeout`), and the `WorkloadRecommendation` read, an informer-cache read, keeps a 1s reserve (`recommendationReadTimeout`) that a slow owner Get cannot eat into. It returns an `admission`: the reason (one of a closed set: a `governance.Refusal`, the `source` label of a verdict that injects nothing, `no-container-matches`, `recommend-only`, `at-recommendation`, `injected`, `governance-failed`, `recommendation-unreadable`, `panic`, `undecodable`), the identity, the Policy and the mode it manages the kind in, the `source` label `admit` counts on `k8s_sustain_webhook_recommendation_source_total` and the read's verdict, the container writes to inject, and the one effect it can call for, a stub request. `admit` logs it once (`logAdmission`), with the reason, the opt-in and its level, the identity, the mode and the verdict, and the pod's container counts when nothing matches. A panic anywhere after decode (deciding, the stub claim, counting or logging) is recovered into the `panic` reason, counted under `admit`, and responded to like any other admission.

`respond` is the only constructor of an admission response and always allows the pod. It patches only an injected admission, and only the resources of the containers it writes: the webhook writes nothing else to a pod. The `k8s.sustain.io/owner-name` annotation is read where the user set it, by `governance.Decide` and by the recording rules through kube-state-metrics' `kube_pod_annotations`, so nothing depends on the webhook having admitted a pod. The stub request runs on a detached goroutine that recovers its own panics (`stub/request`), since nothing above it would.

A test asserts why a pod went unmutated through the reason: `TestDecide` (`decide_test.go`) is one table keyed on it, with the stub effect and the writes as columns; `TestAdmitPatchesOnlyTheResourcesItInjects` asserts an admission patches nothing else, governed or not.

#### Informer cache and shutdown ordering

The webhook serves `Policy`, `WorkloadRecommendation` and `Namespace` reads from an informer cache (`k8s.NewCached`); budget roughly 20–50 MB resident at 10k workloads. Owner-chain kinds (`ReplicaSet`, `Job`, and the workload kinds read for workload-level opt-in) stay uncached: they are read at most once per pod CREATE, and a cluster-wide informer per kind would cost more memory than the Gets it saves.

The three informers are registered and synced during startup, before the webhook receives traffic. Registering them lazily would push a blocking cluster-wide LIST into the first admission's budget for that read, so on large clusters the first pods after every restart would fail open. Startup is all-or-nothing: if it fails (most often because the CRDs are not yet established on a fresh install, which `k8s.NewCached` waits up to two minutes for), the informers already started are stopped before the error is returned, so a crash-looping webhook does not leak a cache per attempt. The cache lives on a run context kept alive past SIGTERM so draining admissions still read fresh data.

#### Owner-resolution cost and caching

The [governance decision](#governance) pre-gates the owner and Namespace reads of [policy resolution](reference/annotation.md#resolution-order): if no Policy's `selector` could claim a pod at all, it skips them entirely. This *is* the cost control for a cluster with at least one selector-scoped Policy — most pod creates in most namespaces are skipped before any extra read.

It does **not** bound cost on a cluster where a Policy has an empty `selector` (no `namespaces`, no `labelSelector`) — the default, and what [Quick Start](getting-started/quick-start.md) produces. Such a Policy covers every pod in the cluster, so the pre-gate always passes and every pod CREATE it admits pays for owner resolution: `resolveCachedPodOwner` (via a Deployment/Rollout pod's ReplicaSet, or a CronJob pod's Job) does one Get to find the top-level owner. This Get is not scoped to the opt-in walk — the decision needs the same top-level owner for the identity the admission decision looks the `WorkloadRecommendation` up by, for **every** pod it injects into, pod-template-annotated (the common case, since every existing user annotates the pod template) or not. A pod with no annotation of its own additionally reaches the walk's workload level, which needs a second Get — `ownerAnnotations`, of the resolved top-level object's own `metadata.annotations` — to evaluate the workload level; that second Get *is* scoped to unannotated pods, since an annotated pod's own annotation already decides the policy without ever reading its owner's. A rolling restart of a large Deployment therefore issues up to two owner Gets per replica in the worst case — not one.

The webhook absorbs this with two small in-memory caches (`internal/webhook/ownercache.go`), both TTL ~30s and both consulted before their respective Get: `Handler.ownerRefCache`, keyed by the pod's own immediate controller ownerRef (`namespace/kind/name/UID` of its ReplicaSet or Job), bounds the first Get — shared by **every** pod CREATE that reaches owner resolution at all, whether its pod template decided its opt-in or not; `Handler.ownerAnnCache`, keyed by `(namespace, kind, name)` of the resolved top-level object, bounds the second Get and is reached only from the workload level. N pods created behind the same owner in a burst — exactly what a rolling restart produces — share both cache keys, so the pair collapses from up to 2N Gets to at most 2.

Both caches also collapse a *concurrent* cold-start burst, not just the steady-state (already-warm) case: N pods admitted at the same instant behind one owner — the exact shape of a rolling restart — issue one in-flight Get via [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight), with every other admission waiting on that Get's result instead of each starting its own. A waiter still respects its own admission deadline: if it times out before the in-flight Get returns, only that one admission gives up and fails open (with the pod resolved on template resources); the Get itself keeps running regardless, to completion or its own 2s `apiCallTimeout` budget (whichever comes first) — not just for as long as some other admission is still waiting on it, but also to populate the cache for the next burst even if every waiter has already given up.

A panic inside that shared Get is contained rather than fatal. Running it on a `singleflight` goroutine puts it outside the HTTP handler's panic-recovery middleware, and `singleflight` deliberately re-raises a leader panic on a bare goroutine that nothing can recover — so, unguarded, one nil deref under an owner Get would abort the whole webhook process and (under `failurePolicy: Fail`) block every Pod CREATE in the cluster until it restarted. The webhook therefore recovers such a panic itself, logs it with its stack, counts it on [`k8s_sustain_webhook_panic_total`](reference/metrics.md#k8s_sustain_webhook_panic_total) under `singleflight/ownerRef` or `singleflight/ownerAnnotations`, and hands it to the leader and every waiter as an ordinary error — which they fail open on like any other failed Get. Nothing is cached, and the next admission retries.

The trade-off: a workload that gains its opt-in annotation at the workload level may take up to the cache's TTL to be picked up by admission. This is acceptable because the controller reconciles independently of admission on its own interval regardless of what the webhook does.

#### Stub writes: dedup, bounds and shutdown

A stub becomes visible to the webhook's informer only after the create and watch propagation, so inside that window every pod of a scaling workload reads `missing` and would fire its own create (a 500-replica scale-out means 500 creates of one name, 499 rejected). Stub requests are therefore deduplicated per identity for 30s and bounded to 16 concurrent writes. Over-capacity requests queue rather than drop: a run-once Job has no later admission to retry on.

Stub goroutines outlive the admission that started them (up to 30s queued plus 5s for the write), so on shutdown SIGTERM cancels in-flight stub writes and the process waits, bounded, for them to unwind before stopping the informer cache they read through. An abandoned request is re-issued by the next admission for the same identity.

Whether an admission requests anything is `wlrcache.ShouldRequest`: always when there is no object, and otherwise only to add to the snapshot of an object of the pod's Policy that is departed or has none, when the pod runs a container the snapshot lacks (existing entries are kept, so alternating container sets cannot churn it). A live identity's snapshot is the union of every member's containers, written by the controller's claim, which one pod's view would only flip back and forth. A request that loses the create race to another writer does nothing more: the next admission reads the winner's object. Only the absent case creates a stub: an undecided, nodata or stale object already exists, and a create would be a guaranteed no-op.

The `k8s.sustain.io/stub` label is provenance, not control flow. The controller's own write path must `Create` before it can patch status (the status subresource discards status supplied at create), so every controller-written recommendation is transiently empty-status too.

#### Read verdict

The webhook takes `wlrcache.Read`'s verdict on the object it read: a pure function of the object, the pod's resolved Policy and the time, returning one of a closed set (absent, undecided, nodata, withheld, stale, fresh, retained) mapped one-to-one onto the `source` label. It injects only when both `spec.policy` and `status.computedBy` are the pod's Policy: that keeps a Conflicted identity's frozen numbers out of the other Policy's pods, and keeps an identity moving between Policies from receiving numbers computed under the old one until the new one computes its own. It injects only before the object's `status.servableUntil`, which the controller [stamped](#servable-until); the webhook holds no freshness setting.

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

#### One module owns it

`internal/wlrcache` is the only code that writes a `WorkloadRecommendation` or judges one; callers never patch the object themselves. Its interface:

- `Request(ctx, client, known, ref, policy, observed)` — the webhook's stub, or its snapshot refresh (`ShouldRequest` decides whether there is anything to do).
- `Ensure(ctx, client, ref, policy, observed, Lifetimes)` — the governing Policy's per-cycle claim on a live identity: create or adopt, replace the snapshot when it differs, clear `departed`. It returns the object as written.
- `Record(ctx, client, known, Decision, now, Lifetimes)` — the one status write per identity per cycle: the outcome, the Recommendation and trace for `Computed`, the `departed` fact and the [`servableUntil`](#servable-until) stamp, into the object `Ensure` returned (or, for a departed or Conflicted identity, the one the inventory read). `now` comes from `PolicyReconciler.Clock`, the clock that dates the pass's snapshot, so a test clock drives `observedAt` and `servableUntil`. An unchanged decision costs no write, except the refresh of a `Computed` one's `observedAt` every `RefreshInterval`. A missing object is an error, never a silent no-op.
- `Read(known, policy, now)` — the [read verdict](#read-verdict).
- `Expired(known, policy, Standing, at, Lifetimes)` — the sweep verdict: keep or delete, given where the identity stands in the reconcile's snapshot (governed, ungoverned, departed, Conflicted) taken at `at`.
- `Sweep(ctx, client, policy, Standings, at, Lifetimes)`, `Release(ctx, client, policy)` and `Reap(ctx, client)` — the only three deletes: a Policy's objects judged by `Expired`, a deleted Policy's objects, and objects naming a Policy that no longer exists. The controller supplies the standings (`standingIn`, a function over its snapshot: `wlrcache` does not import the inventory) and the snapshot's instant.
- `Key(id)` and `WorkloadRef(id)` — the object key of an identity's `WorkloadRecommendation` and the `spec.workloadRef` that names it; the webhook reads by the one, and the controller's claim and the webhook's request name the identity by the other.

Every writer runs against a cache-backed client, so nothing re-reads an object it just wrote: each write patches off the object the previous one returned, and a create that loses the race to another writer claims that writer's object with a patch, whose response carries it. The lifecycle suite (`internal/wlrcache/lifecycle_test.go`) runs every writer against a client whose cache lags behind every create.

#### Servable until

Every status write of `Ensure` and `Record` stamps `status.servableUntil` from one formula over the status it writes and the controller's `wlrcache.Lifetimes` (`--reconcile-interval`, `--recommendation-retention`): `observedAt` plus `StalenessBudget()`, the larger of `MinStalenessBudget` (30m) and three intervals, for a live identity; the last computation plus the retention window for a departed or Conflicted one. Only `Computed` moves `observedAt`, so no other outcome extends it, and only a `Computed` record gives an object without a stamp its first (`Lifetimes.stamp`): an object written before the field existed is served nothing until then, since a claim or another outcome stamping it would extend what it is served for. `Request` writes only the snapshot, which the formula does not read. The stamp is truncated to the second the API stores, so restamping a stored status reproduces it and an unchanged decision still costs no write.

The refresh interval (`wlrcache.RefreshInterval`, 10m) must stay well below the minimum budget: a stable Recommendation is up to one refresh interval plus one reconcile interval old when a pass refreshes it, and must outlive a pass that runs one interval late. `TestAStableRecommendationStaysServableThroughALatePass` pins that across intervals; the closest call is an interval just under 10m, where three intervals start to set the budget. The retention default (`wlrcache.DefaultRetention`, 168h) is the flag's default; the configuration rejects a retention or a reconcile interval of zero or less, which would stamp nothing servable.

The stamp is an absolute time, not a bound the webhook applies to the object's age, so whatever the controller last wrote stops being served once it passes, even when the controller stops writing: both the sweep and the record that sets or clears `departed` live inside the reconcile, which returns early when the inventory cannot be read (RBAC revoked on one kind, an unreachable API group). `departed` is only recorded from a snapshot that read successfully, never on a failed read.

#### GC preconditions

Every cleanup path decides on a copy read from the informer cache, so every one deletes with `uid` + `resourceVersion` preconditions (`deleteWhere` in `internal/wlrcache/delete.go`): the `uid` refuses a name reused by a delete-and-recreate, the `resourceVersion` an object rewritten since it was listed — typically re-labelled and re-pointed at another Policy that adopted it. What a conflict means differs:

- **Per-cycle sweep** and **orphan reaper** leave the object alone and re-judge it on their next pass. A conflict is an expected outcome, not an error.
- **Policy-deletion finalizer** returns any failure, conflicts included, so the finalizer stays on and the deletion is retried. The retry re-lists the Policy's objects by label: one another Policy adopted meanwhile (a GitOps rename) is no longer listed and survives.

#### Sweep grace

The sweep judges each of the Policy's `WorkloadRecommendation`s against the reconcile's inventory snapshot (`wlrcache.Expired`): one whose identity is Departed or Conflicted is kept for the retention window, counted from its last computation; one whose identity is governed by a Policy, this one or another that will adopt it, is kept; one whose identity is live but governed by no Policy, or is outside the snapshot (namespace or kind no longer in the Policy's scope), is deleted.

Independently of retention, a `WorkloadRecommendation` created after the snapshot, or within the 10 minutes before it, is never swept: the snapshot cannot account for an identity first written after it, nor for one whose members its cache had not seen yet. The grace is counted from the snapshot's instant (`inventory.Snapshot.TakenAt`), not the sweep's own clock: a pass that waits on evictions can sweep long after its snapshot, and a webhook stub created in between would otherwise be judged ungoverned and deleted. It keys off the creation timestamp, not `status.observedAt`: the computation phase refreshes `observedAt` for every identity it computes each cycle, departed identities included, so an `observedAt`-based guard would be self-satisfying and an opted-out workload would keep its recommendation (and its share of Prometheus load) indefinitely.
