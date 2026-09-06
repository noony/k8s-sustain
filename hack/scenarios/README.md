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
| `stepped`            | Two-phase load (5 min low / 5 min high) — exercises a second recycle on drift. Requires `WINDOW <= 2m`: with the default 10m window the percentile always spans the whole high phase and the recommendation stays pinned high. |
| `hpa`                | Uncoordinated baseline: recommender shrinks requests, HPA reacts by scaling replicas up. Metrics-server is auto-installed by `make test-kind-up`. |
| `hpa-coordinated`    | Same workload as `hpa` with `autoscalerCoordination.enabled: true` — overhead formula keeps utilization below the HPA target so replicas stay at 1. |
| `hpa-replica-anchor` | Pre-scaled to 6 replicas with `replicaBudgetAnchor: 0.10` — replica-budget correction adds an extra CPU bump (factor clamped at 2.0) to encourage consolidation. |
| `init-containers`    | Pod with a regular container, a classic init container, and a sidecar (restartable) init container. Validates that the regular container and the sidecar get recommendations and drive recycle on drift, while the classic init container (`migrate`, exits after ~5s) gets none — it is too short-lived for Prometheus to hold a usage series. |
| `oom-kill`           | Container quietly holds 30Mi for 60s, then bursts to 120Mi against a 96Mi limit → repeats `OOMKilled`. Validates the OOM-aware memory floor: recommendation stays ≥ current request even though the percentile alone would suggest shrinking. Inspect `k8s_sustain_oom_floor_applied_total{owner_name="stress"}` — it should increment. |
| `qos-change`         | 3-replica Deployment whose pods start **Guaranteed** (requests == limits on CPU and memory); the policy removes the CPU limit, so the recommendation would flip them to Burstable. Kubernetes forbids a QoS class change via `/resize`, so on k8s ≥ 1.33 each in-place attempt is rejected `Invalid` per-pod and the controller falls back to **eviction**: pods are replaced (new UID) one at a time, the webhook injects the new resources, and the replacements come up `Burstable`. A PDB (`minAvailable: 2`) is honored throughout — at least 2 pods stay available during the recycle. In-place stays enabled for every other workload. |
| `cronjob`            | CronJob runs every 2 minutes for ~90s with steady ~200m / 100Mi load. Validates the `Ongoing` path for short-lived CronJob runs: the CronJob spec is **never modified** (no GitOps drift); each new run's pod is webhook-injected with the latest recommendation. Inspect the latest pod, not the spec: `kubectl get pod -n scenario-cronjob -l app=stress --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].spec.containers[0].resources}'`. |
| `cronjob-long-running` | CronJob with a 12-minute pod runtime (every 15 min, `concurrencyPolicy: Forbid`). Validates the **in-place pod resize** path on a live `restartPolicy: Never` pod (k8s ≥ 1.35): while a run is in flight, the controller patches `pods/resize` and the container's resources change without a restart. Watch a single pod across two reconcile intervals and confirm `restartCount` stays at 0. |
| `cronjob-overprovisioned` | CronJob analogue of the `overprovisioned` Deployment scenario. Heavy downsizing: CPU 1000m → ~60m, memory 512Mi → ~50Mi, observed on each new run's pod. The CronJob spec is intentionally unchanged. |
| `job`                | Standalone `batch/v1` Job (no CronJob owner). Validates the webhook's `Pod → Job` resolution branch in isolation — the controller does not reconcile standalone Jobs, so any resource adjustment on the pod is purely the webhook's doing. Re-apply the Job after WINDOW elapses to see the recommendation kick in. |
| `statefulset`        | 3-replica StatefulSet (`web-0..2`) with oversized requests. Validates (a) eviction in descending ordinal order (`web-2 → web-1 → web-0`), and (b) the UID-keyed recycle wait — StatefulSets recreate evicted pods under the **same name** but a new UID, so the wait must key on UID. The full 3-pod recycle should complete in roughly one reconcile cycle, not 15+ minutes (one replacement-timeout per pod). |
| `custom-name`        | Single Deployment (`stress`) annotated `k8s.sustain.io/owner-name: renamed-app` on its pod template. Validates the identity-rename override: the Prometheus identity and the `WorkloadRecommendation` (`deployment-renamed-app`) use the override, not the Deployment's own name (`deployment-stress` never exists). Otherwise behaves exactly like `steady` (same load profile, same expected downsizing). Not in `status.sh`'s generic table (it queries recommendations by the Deployment's own name); see the verification commands in `custom-name.yaml`'s header comment. |
| `bare-pod`           | Standalone `Pod` (no controller owner — simulating Airflow's `KubernetesPodOperator`) carrying `k8s.sustain.io/policy` and `k8s.sustain.io/owner-name: etl-daily` directly on itself, with `pod: Ongoing` configured on the Policy. Validates that Ongoing **resizes a Pod-kind target in place and never evicts it**: the pod's deliberately oversized `500m/256Mi` requests come down toward its actual ~`200m`/`~100Mi` usage through the `pods/resize` subresource, under an unchanging UID, and a `WorkloadRecommendation` (`pod-etl-daily`) is cached alongside. On a cluster below k8s 1.33 there is no in-place support, so the pod stays at `500m/256Mi` and only the recommendation appears. Not in `status.sh`'s generic table; see the verification commands in `bare-pod.yaml`'s header comment. |
| `recommend-only`     | Clone of `steady` whose Policy sets `spec.rightSizing.recommendOnly: true` (with `deployment: Ongoing`, proving the field — not the update mode — suppresses the apply). Validates per-policy dry-run end-to-end: a `WorkloadRecommendation` (`deployment-stress`, ~220m/~110Mi) is computed and cached, but the pod is **never recycled** (requests stay `500m/256Mi`) and the webhook **never injects** — delete the pod and the replacement keeps the template resources. Shows up in `status.sh`'s table as a recommendation that never converges with current and `RECYCLED` staying `no`. |
| `recurring`          | A namespace-scoped CronJob (RBAC: `create`/`get`/`delete` on pods only, no `ClusterRole`) launches a genuinely bare Pod (no `ownerReferences`) every 2 minutes under a stable `k8s.sustain.io/owner-name: etl-recurring`, waits for it, then **deletes** it — simulating a recurring Airflow task. The delete is load-bearing: `workload.GroupBarePods` ignores pod phase, so leftover `Succeeded` pods would keep the identity in every target listing and the departed path would never run. Each cycle leaves ~80s with **zero** pods, comfortably more than the harness's 30s `--reconcile-interval`. Validates the WLR-driven-refresh headline claim: `status.observedAt` on `pod-etl-recurring` advances on reconciles where the pod list is confirmed empty, and `k8s_sustain_wlr_refresh_total` (emitted only from the departed path) climbs. Measured behaviour is **window-dependent** — at the default `WINDOW=10m` the 10-minute `MinWorkloadAge` gate holds for 6 clean template-resource runs and then CPU sticks at the `1m` floor with memory staying realistic and no pods killed; at `WINDOW=2m` the same gate clears into `cpu=1m, mem=4Mi/6Mi` and every subsequent pod is OOM-killed at `t=0` in a self-sustaining loop. Both are the same currently-expected design gap (the gate is time-based, not data-volume-based), not a scenario defect. See `recurring.yaml`'s header for both chronologies and for how to observe `status.departed: true` (suspend the CronJob, ~15 min). Not in `status.sh`'s generic table. |

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
