# Local end-to-end testing

This guide walks through the `Makefile.scenarios` harness — a one-command
way to bring up a kind cluster, install k8s-sustain, and run synthetic
workload scenarios that exercise `Ongoing`-mode pod recycling end-to-end.

## Prerequisites

- Docker
- [`kind`](https://kind.sigs.k8s.io/)
- `kubectl`
- `helm` >= 3.10

`make test-kind-up` installs the in-cluster dependencies for you:

- **cert-manager** — required by the k8s-sustain admission webhook to issue
  its serving cert.
- **metrics-server** (patched with `--kubelet-insecure-tls` so it works
  against kind's self-signed kubelet certs) — required by the `hpa`
  scenario.

## Quick start

```bash
make test-kind-up                       # ~3-5 min the first time
make test-scenario-steady               # apply the scenario
sleep 11m                               # wait for WINDOW + reconcile slack
make test-scenario-status               # see the table
make test-kind-down                     # tear it all down
```

## Context safety

Every `test-scenario-*` target refuses to run unless the current kubectl
context is `kind-k8s-sustain` (i.e. the cluster `make test-kind-up` just
created). This is a guardrail against accidentally applying scenario
manifests to a real cluster.

To override (e.g. when you renamed the cluster, or you really know what
you're doing):

```bash
SKIP_CONTEXT_CHECK=1 make test-scenario-steady
```

## Tunable variables

| Variable               | Default                  | Notes                                                       |
| ---------------------- | ------------------------ | ----------------------------------------------------------- |
| `WINDOW`               | `10m`                    | Policy `window` value, templated into each scenario YAML.  |
| `RECONCILE`            | `30s`                    | Controller `--reconcile-interval`, set via helm `--set`.   |
| `TEST_IMG`             | `k8s-sustain:dev`        | Image built and loaded into kind.                          |
| `WAIT`                 | `0`                      | Optional pause between scenarios in `test-scenario-all`. Scenarios are isolated by namespace and don't interfere; only set this if you want staggered apply timing. |
| `CLUSTER_NAME`         | `k8s-sustain`            | Kind cluster name (context becomes `kind-<name>`).         |
| `CERT_MANAGER_VERSION` | `v1.16.2`                | cert-manager chart version installed by `test-kind-up`.    |
| `SKIP_CONTEXT_CHECK`   | unset                    | Set to `1` to bypass the kubectl-context guard.             |

## Workload generator

Each scenario runs a small Python load generator (`python:3.12-alpine`)
shipped as a ConfigMap. The generator allocates a fixed amount of memory
and busy-loops a tunable fraction of one core, controlled by env vars:

- `LOAD_DUTY` — fraction of one core to consume (e.g. `0.20` ≈ 200 mCPU).
- `LOAD_MEM_MB` — MiB to allocate and keep resident.
- `LOAD_PHASES` — `"duty:secs,duty:secs,..."` schedule, used by `stepped`
  to alternate between low and high load.

This replaces `polinux/stress` because Kubernetes requires
`requests.cpu <= limits.cpu`, so we cannot cgroup-throttle `stress`'s
full-core workers below the request — the partial-load Python loop lets
us pick any fractional CPU usage we want without violating that.

## Scenario catalog

### `steady`

Single Deployment producing ~200m CPU and ~100MiB memory. Initial requests
are deliberately oversized at `500m / 256Mi`.

**Expected:** CPU request drops to ~`220m`, memory request drops to
~`110Mi`, within `WINDOW + reconcile_interval`.

### `overprovisioned`

Same shape as `steady` but with extreme oversizing: `1000m / 512Mi`
requests for ~`50m / 40Mi` of usage.

**Expected:** Aggressive downsizing — CPU to ~`60m`, memory to ~`50Mi`.

### `underprovisioned`

Initial requests `50m / 32Mi`, *no limits*, actual usage ~`300m / 200Mi`.

**Expected:** CPU request grows to ~`330m`, memory to ~`230Mi`.

### `stepped`

Single Deployment whose load alternates between ~`100m` (5 min) and
~`400m` (5 min), driven by `LOAD_PHASES=0.10:300,0.40:300`.

**Expected:** The recommender lands at ~`110m` during the low phase, then
upsizes to ~`440m` once the high phase dominates the percentile window.
The controller recycles the pod each time, exercising the second-recycle
path that uniform-load scenarios cannot reach.

### `hpa`

Single Deployment with `requests.cpu: 500m`, actual usage ~`150m`, plus
an `autoscaling/v2` HPA targeting 60% CPU utilization (min 1 / max 5).

**Expected:** Recommender shrinks requests to ~`165m`. Once shrunk, HPA's
effective utilization jumps above 60% and replicas scale up. Validates
the interaction between right-sizing and the HPA.

### `init-containers`

Single Deployment whose pod template includes:

- a regular container `app` (CPU `500m`, ~`200m` actual usage),
- a classic init container `migrate` that exits in ~5 seconds,
- a sidecar init container `log-shipper` (`restartPolicy: Always`,
  ~`50m` actual usage).

**Expected:** All three containers receive recommendations
(`kubectl get wlrec -n scenario-init-containers -o yaml`). Drift in `app` or
`log-shipper` triggers a pod recycle (in-place on k8s ≥ 1.32, eviction
otherwise). Drift in `migrate` does **not** trigger recycle — it has already
exited; the new requests land via webhook injection on the next pod creation.

Inspect the `container_kind` label on emitted gauges:

```bash
kubectl --raw \
  /api/v1/namespaces/k8s-sustain/services/k8s-sustain-controller:8080/proxy/metrics \
  | grep 'k8s_sustain_recommended_cpu_cores{.*container_kind="init"'
```

## Observability

`make test-scenario-status` prints a table:

```text
NAMESPACE                  POD             CPU req  CPU rec  MEM req  MEM rec  RECYCLED
scenario-overprovisioned   stress-xxxxx    1000m    62m      512Mi    48Mi     yes
scenario-steady            stress-yyyyy    500m     230m     256Mi    115Mi    yes
```

The dashboard remains the richer source of truth — start a port-forward
and open `http://localhost:8090`:

```bash
kubectl port-forward -n k8s-sustain svc/k8s-sustain-dashboard 8090:8090
```

## Adding a new scenario

See `hack/scenarios/README.md`.
