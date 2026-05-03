# Scenarios

Synthetic workloads that exercise k8s-sustain's `Ongoing` recycling and
recommendation behaviour on a local kind cluster. Each scenario is a single
self-contained YAML applied via `make test-scenario-<name>`.

See `Makefile.scenarios` for the targets and `docs/guides/local-testing.md`
for the operator-facing walkthrough.

## Scenarios

| Name                 | What it validates                                                              |
| -------------------- | ------------------------------------------------------------------------------ |
| `steady`             | Basic downsizing + first recycle on a constant load.                           |
| `overprovisioned`    | Aggressive downsizing (CPU 1000m → ~60m, memory 512Mi → ~50Mi).                |
| `underprovisioned`   | Upsizing when actual usage exceeds requests (no limits set).                   |
| `stepped`            | Two-phase load (5 min low / 5 min high) — exercises a second recycle on drift.  |
| `hpa`                | Uncoordinated baseline: recommender shrinks requests, HPA reacts by scaling replicas up. Metrics-server is auto-installed by `make test-kind-up`. |
| `hpa-coordinated`    | Same workload as `hpa` with `autoscalerCoordination.enabled: true` — overhead formula keeps utilization below the HPA target so replicas stay at 1. |
| `hpa-replica-anchor` | Pre-scaled to 6 replicas with `replicaBudgetAnchor: 0.10` — replica-budget correction adds an extra CPU bump (factor clamped at 2.0) to encourage consolidation. |
| `init-containers`    | Pod with a regular container, a classic init container, and a sidecar (restartable) init container. Validates that all three get recommendations, the sidecar drives recycle on drift, and the classic init container does not. |
| `oom-kill`           | Container quietly holds 30Mi for 60s, then bursts to 120Mi against a 96Mi limit → repeats `OOMKilled`. Validates the OOM-aware memory floor: recommendation stays ≥ current request even though the percentile alone would suggest shrinking. Inspect `k8s_sustain_oom_floor_applied_total{owner_name="stress"}` — it should increment. |

## Running a scenario

```bash
make test-kind-up                       # one-time cluster setup
make test-scenario-steady               # apply the scenario
make test-scenario-status               # current vs. recommended
make test-scenario-clean                # tear all scenarios down
make test-kind-down                     # delete the kind cluster
```

Tunable variables (passed on the command line):

```bash
make test-scenario-steady WINDOW=2m     # use a 2-minute policy window
make test-scenario-all WAIT=3m          # add a 3-minute stagger between scenarios (default: 0, scenarios are isolated)
SKIP_CONTEXT_CHECK=1 make test-scenario-steady   # bypass the kubectl-context guard
```

`make test-kind-up` installs cert-manager (for the webhook serving cert)
and metrics-server (for the HPA scenario) in addition to k8s-sustain
itself.

## Adding a new scenario

1. Copy an existing YAML to `hack/scenarios/<name>.yaml`.
2. Replace every `<old-name>` with `<name>`.
3. Add `<name>` to `SCENARIOS := ...` in `Makefile.scenarios`.
4. Document it in this README and in `docs/guides/local-testing.md`.

## What is `__WINDOW__`?

A literal sentinel string that `make` substitutes with `$(WINDOW)` at apply
time using `sed`. This avoids pulling in helm or envsubst for what is a
single substitution.
