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

## Make targets

`make test-help` lists every harness target.

| Target | What it does |
|---|---|
| `test-kind-up` | Create (or reuse) the kind cluster, install cert-manager and metrics-server, build and load the image, `helm upgrade --install` k8s-sustain with debug logging |
| `test-kind-down` | Delete the kind cluster |
| `test-reboot` | Clean every scenario and Policy, uninstall the release, then run `test-kind-up` again |
| `test-scenario-<name>` | Apply one scenario from the [catalog](#scenario-catalog) |
| `test-scenario-all` | Apply every scenario back-to-back (`WAIT` pauses between them) |
| `test-scenario-status` | Current vs. recommended table (`hack/scenarios/status.sh`) |
| `test-scenario-watch` | List pods in every scenario namespace |
| `test-scenario-logs` | Tail controller, webhook and dashboard logs |
| `test-scenario-clean` | Delete every `scenario-*` namespace and its Policy |

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
| `METRICS_SERVER_URL`   | upstream `latest` `components.yaml` | metrics-server manifest applied by `test-kind-up`.  |
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

Run this one with `WINDOW=2m`. The percentile window must be shorter than a
5-minute phase, otherwise it always contains the whole high phase and the
recommendation correctly stays pinned near `440m` for the entire run:

```bash
make test-scenario-stepped WINDOW=2m
```

### `hpa`

Single Deployment with `requests.cpu: 500m`, actual usage ~`150m`, plus
an `autoscaling/v2` HPA targeting 60% CPU utilization (min 1 / max 5).

**Expected:** Recommender shrinks requests to ~`165m`. Once shrunk, HPA's
effective utilization jumps above 60% and replicas scale up. Validates
the interaction between right-sizing and the HPA.

### `hpa-coordinated`

Same Deployment and HPA as `hpa` (`500m` request, ~`150m` usage, 60% target)
with `autoscalerCoordination.enabled: true`.

**Expected:** the CPU request is sized with the overhead formula,
`base × (110 / 60)` ≈ 1.83× — ~`300–350m` instead of ~`165m`. Utilization stays
near 50%, below the HPA target, so replicas stay at 1. The Workloads page shows
the **Coordinated** badge. See
[Autoscaler coordination](../concepts/autoscaler-coordination.md).

### `hpa-replica-anchor`

`hpa-coordinated` plus `replicaBudgetAnchor: 0.10`, pre-scaled to 6 replicas
(HPA min 1 / max 5). The anchor's target is `round(1 + 0.1 × 4) = 1` replica,
so while the Deployment runs more replicas the CPU request gets up to a 2×
bump on top of the overhead formula.

**Expected:** usually indistinguishable from `hpa-coordinated`. The HPA
scales 6 → 1 within ~9 minutes, before the 10-minute workload-age gate lets
the first recommendation compute, so the replica factor is `1.0` by then. To
observe the bump, keep the HPA scaled out past the gate (more load, or
`behavior.scaleDown.stabilizationWindowSeconds` above 600).

### `init-containers`

Single Deployment whose pod template includes:

- a regular container `app` (CPU `500m`, ~`200m` actual usage),
- a classic init container `migrate` that exits in ~5 seconds,
- a sidecar init container `log-shipper` (`restartPolicy: Always`,
  ~`50m` actual usage).

**Expected:** `app` and `log-shipper` receive recommendations
(`kubectl get wlrec -n scenario-init-containers -o yaml`). Drift in either
triggers a pod recycle (in-place on k8s ≥ 1.33, eviction otherwise).

`migrate` receives **no** recommendation: a ~5-second lifetime leaves no
usage series in Prometheus for the recommender to read. Even if it had one,
drift there could not trigger a recycle — the container has already exited
while the pod is Running.

Inspect the `container_kind` label on emitted gauges:

```bash
kubectl --raw \
  /api/v1/namespaces/k8s-sustain/services/k8s-sustain-metrics:8080/proxy/metrics \
  | grep 'k8s_sustain_recommended_cpu_cores{.*container_kind="init"'
```

### `cronjob`

Single CronJob (`schedule: "*/2 * * * *"`) running for ~90s per invocation
with steady ~200m CPU / ~100MiB memory load. Initial requests are
deliberately oversized at `500m / 256Mi`. `Ongoing` mode is enabled for
CronJobs.

**Expected:** the CronJob spec is **never modified** (no GitOps drift).
Currently-running job pods are resized in place via the `pods/resize`
subresource when the cluster supports it; otherwise they finish on their
existing resources. New runs (every two minutes) spawn with updated
requests injected by the webhook at admission. After
`WINDOW + reconcile_interval` the CPU request drops to ~`220m` and
memory to ~`110Mi`.

```bash
# CronJob spec is unchanged across reconciles (no controller patches)
kubectl get cronjob -n scenario-cronjob job \
  -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].resources}'
# The current recommendation is exposed via WorkloadRecommendation
kubectl get wlrec -n scenario-cronjob cronjob-job -o yaml
# Inspect a running job pod — its container resources reflect the latest reco
kubectl get pod -n scenario-cronjob -l batch.kubernetes.io/job-name -o yaml \
  | grep -A4 'resources:'
```

This scenario also exercises the Pod → Job → CronJob recording-rule
chain — without it, `owner_kind="CronJob"` would have no metrics and the
recommendation would never compute.

### `cronjob-long-running`

CronJob with a 12-minute pod runtime (every 15 minutes,
`concurrencyPolicy: Forbid`). Initial requests are oversized at
`500m / 256Mi`. Run with `WINDOW=2m RECONCILE=30s` so the controller has
time to land at least one recommendation while a single run is still
alive.

**Expected:** the **same pod** is resized in place via the `pods/resize`
subresource as the controller reconciles. The container's
`spec.containers[0].resources` change without a restart — verify
`status.containerStatuses[0].restartCount` stays at `0`. This is the
scenario that exercises the in-place resize path on a `restartPolicy:
Never` Job pod (k8s ≥ 1.35).

```bash
POD=$(kubectl get pod -n scenario-cronjob-long-running -l app=stress \
  --field-selector status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}')
kubectl get pod -n scenario-cronjob-long-running $POD \
  -o jsonpath='{.spec.containers[0].resources}{"\n"}'
sleep 60
kubectl get pod -n scenario-cronjob-long-running $POD \
  -o jsonpath='{.spec.containers[0].resources}{"\n"}'
kubectl get pod -n scenario-cronjob-long-running $POD \
  -o jsonpath='{.status.containerStatuses[0].restartCount}{"\n"}'
```

### `cronjob-overprovisioned`

CronJob analogue of the `overprovisioned` Deployment scenario. Initial
requests `1000m / 512Mi`, actual per-run usage ~`50m / 40Mi` for ~90s.

**Expected:** Aggressive downsizing — CPU to ~`60m`, memory to ~`50Mi`,
observed on each new run's pod (webhook-injected). The CronJob spec
itself is unchanged.

### `job`

A standalone `batch/v1` Job (no CronJob owner) with one ~5-minute run,
under `job: OnCreate`. Exercises the webhook's `Pod → Job` resolution: the
controller computes and caches the recommendation, and the webhook injects
it into the next run's pod. Under `OnCreate` the running pod is never touched
(`job: Ongoing` would resize it in place, never evict it).

**Expected:** First run starts with the original `500m / 256Mi` requests
(no history yet); the webhook creates a cold-start stub
`WorkloadRecommendation` (`job-oneshot`). Re-apply the Job after `WINDOW`
elapses — once the controller has filled the stub, the new run's pod is
injected with the percentile-based recommendation (~`60m / 35Mi`).

The re-applied Job object is only seconds old, but the 10-minute
workload-age gate keys on the earlier of the Job's creation time and its
`WorkloadRecommendation`'s, so the cached object carries the identity's age
across runs.

```bash
kubectl delete -f hack/scenarios/job.yaml
make test-scenario-job
kubectl get pod -n scenario-job -l app=stress \
  -o jsonpath='{.items[0].spec.containers[0].resources}{"\n"}'
```

### `coldstart`

A standalone Job with a fixed name (`etl`, `job: OnCreate`) that runs ~90s
and is TTL-deleted 30s later, so its whole lifetime fits between two
reconciles at the default 5m interval. Exercises
[cold-start stub recommendations](../concepts/workload-recommendations.md#cold-start-stub-recommendations):
the webhook creates the `WorkloadRecommendation` at the first admission, and
the controller keeps recomputing it from its work-list even though it never
sees the Job alive.

The harness installs with `RECONCILE=30s`, which does catch the Job alive.
To see the stub as the only writer, reinstall with
`make test-kind-up RECONCILE=5m`.

**Expected:**

1. First run: admitted at `500m / 256Mi`; `job-etl` appears with no
   `status.containers` (no history yet).
2. Re-run the Job a few times, spaced out, until the identity has history:

    ```bash
    kubectl delete job etl -n scenario-coldstart --ignore-not-found
    make test-scenario-coldstart
    ```

3. `status.containers` fills with a real recommendation, and the next run's pod
   is admitted with it (~`55m / ~35Mi`).

```bash
kubectl get wlrec -n scenario-coldstart job-etl \
  -o jsonpath='{.status.containers}{"\n"}'
kubectl get pod -n scenario-coldstart -l app=etl \
  -o jsonpath='{.items[0].spec.containers[0].resources}{"\n"}'
```

### `statefulset`

Three-replica StatefulSet (`web-0`, `web-1`, `web-2`) with deliberately
oversized requests (`500m / 256Mi`) and ~`200m / ~100Mi` of actual load.

**Expected:**

- After `WINDOW + reconcile_interval` the controller recycles all three
  pods. On Kubernetes ≥ 1.33 this is in-place via `/resize`; on older
  versions it is eviction.
- In the eviction path, pods are evicted in **descending ordinal order**:
  `web-2 → web-1 → web-0`, matching the StatefulSet controller's update
  semantics. Inspect with:

  ```bash
  kubectl get events -n scenario-statefulset \
    --sort-by=.lastTimestamp | grep -E 'Evicted|Killing|Recycled'
  ```

- The full 3-pod recycle finishes in roughly one reconcile cycle. The
  recycle wait keys on pod UID, not name (the StatefulSet controller reuses
  names across replacements); if each pod instead blocks for the full
  `--recycle-replacement-timeout` (5 min default), that wait is broken.
  Watch the controller log:

  ```bash
  kubectl logs -n k8s-sustain deploy/k8s-sustain --since=5m \
    | grep -E 'evict|recycle|replacement'
  ```

- The CPU request drops to ~`220m` and memory to ~`110Mi` on every
  replacement pod. Confirm uniformly across ordinals:

  ```bash
  for i in 0 1 2; do
    kubectl get pod -n scenario-statefulset web-$i \
      -o jsonpath='{.spec.containers[0].resources}{"\n"}'
  done
  ```

### `custom-name`

Single Deployment named `stress`, same shape as `steady` (`500m / 256Mi`
requests, ~`200m / ~100Mi` actual usage), but its pod template carries
`k8s.sustain.io/owner-name: renamed-app` — overriding its
Prometheus/recommendation identity to `Deployment/renamed-app`. Validates the
identity-rename override (see
[Standalone Pods & Identity Grouping](standalone-pods-and-grouping.md)): the
Deployment's own name is never used for Prometheus queries or the
`WorkloadRecommendation`.

**Expected:**

- The pod is admitted with the webhook-mirrored label. Confirm:

  ```bash
  kubectl get pods -n scenario-custom-name --show-labels | grep k8s.sustain.io/owner-name
  ```

- The `WorkloadRecommendation` is named by the override identity
  (`deployment-renamed-app`), not by the Deployment's own name
  (`deployment-stress` never exists):

  ```bash
  kubectl get workloadrecommendation -n scenario-custom-name
  ```

- After `WINDOW + reconcile_interval`, CPU request drops to ~`220m` and
  memory to ~`110Mi` (identical target to `steady`, since the load profile
  and `resourcesConfigs` match) — and the recommendation is only queryable
  under the renamed identity:

  ```bash
  kubectl port-forward -n k8s-sustain svc/k8s-sustain-dashboard 8090:8090 &
  curl -s localhost:8090/api/workloads/scenario-custom-name/Deployment/renamed-app
  ```

### `bare-pod`

A standalone `Pod` with no `ownerReferences`, simulating Airflow's
`KubernetesPodOperator`. The policy and `k8s.sustain.io/owner-name: etl-daily`
annotations sit on the Pod itself, and the Policy sets
`update.types.pod: Ongoing`. Requests `500m / 256Mi`, usage ~`200m / ~100Mi`.

**Expected:**

- The pod carries the webhook-mirrored `k8s.sustain.io/owner-name` label.
- After the 10-minute workload-age gate, the controller caches a
  `WorkloadRecommendation` (`pod-etl-daily`); a later pod with the same
  `owner-name` is injected from it at admission.
- On k8s ≥ 1.33 the running pod is **resized in place, never evicted**: same
  UID, CPU request comes down toward ~`220m`. A memory decrease the kubelet
  reports `Infeasible`/`Deferred` is skipped, never turned into an eviction.
  Below 1.33 the pod keeps `500m / 256Mi`. (The scenario pod defaults to
  `restartPolicy: Always`; real `KubernetesPodOperator` pods use `Never`,
  which resizes only from k8s 1.35.)

```bash
kubectl get pod -n scenario-bare-pod etl-daily-run-1 --show-labels
kubectl get pod -n scenario-bare-pod etl-daily-run-1 \
  -o jsonpath='{.metadata.uid}{" "}{.spec.containers[0].resources.requests}{"\n"}'
kubectl get wlrec -n scenario-bare-pod pod-etl-daily -o yaml
```

### `oom-kill`

Single-container Deployment that quietly holds ~30Mi for 60 s, then attempts
to allocate 120Mi against a 96Mi cgroup memory limit — the kernel kills the
container, Kubernetes restarts it, and the cycle repeats.

**Expected:**

- `kubectl get pods -n scenario-oom-kill` shows `OOMKilled` in the last
  termination reason and a growing restart count.
- `k8s_sustain:workload_oom_24h{owner_name="stress"}` becomes positive.
- Memory recommendation **does not shrink** despite most samples being
  quiet (~30Mi). The OOM-aware floor pulls it to
  `max(kernel high-water peak, OOM-time cgroup limit × 1.20)` plus headroom —
  i.e. ≥ ~115Mi before headroom.
- `k8s_sustain_oom_floor_applied_total{owner_name="stress"}` increments on
  each reconcile while the OOM is within the 24 h window, and the
  `WorkloadRecommendation`'s `status.trace.stress.memory.oomFloor` shows the
  floor with `determined: true`.

```bash
kubectl get wlrec -n scenario-oom-kill deployment-stress -o yaml
kubectl --raw \
  /api/v1/namespaces/k8s-sustain/services/k8s-sustain-metrics:8080/proxy/metrics \
  | grep 'k8s_sustain_oom_floor_applied_total'
```

### `qos-change`

3-replica Deployment whose containers start as **Guaranteed QoS** —
`requests == limits` for both CPU (`500m`) and memory (`256Mi`) — with
~`200m / 100Mi` of actual load per pod, protected by a PodDisruptionBudget
with `minAvailable: 2`. The policy removes the CPU limit (`noLimit: true`)
and sets a 1.5× memory limit ratio, so the recommendation breaks the
`requests == limits` equality: applying it would flip the pods to
Burstable, which Kubernetes forbids through the `/resize` subresource.

**Expected (k8s ≥ 1.33):**

- Each in-place resize is rejected as `Invalid` by the API server (QoS
  class change). This is a *per-pod* rejection — in-place mode stays
  enabled for every other workload.
- The controller falls back to **eviction**: pods are replaced (new
  name/UID) rather than resized, one at a time — after each eviction the
  recycle loop waits for the replacement to become Ready before evicting
  the next pod, so the PDB's `minAvailable: 2` holds throughout. An
  eviction blocked by the PDB (429) is skipped and retried on the next
  reconcile.
- The replacement pods run `Burstable` with CPU request ~`220m` (no limit)
  and memory request ~`110Mi` (limit ~`165Mi`).

```bash
# Guaranteed before the recycle, Burstable after
kubectl get pod -n scenario-qos-change -l app=stress \
  -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.status.qosClass}{"\n"}{end}'
# the per-pod Invalid rejection and the eviction fallback
kubectl logs -n k8s-sustain deploy/k8s-sustain \
  | grep -E 'rejected as invalid|falling back to eviction'
# PDB status during the recycle: disruptionsAllowed flips between 1 and 0,
# at least 2 pods stay Ready
kubectl get pdb -n scenario-qos-change stress -o wide
```

On clusters without in-place support (< 1.33) the scenario still converges —
stale pods are evicted directly — but the interesting branch (the `Invalid`
rejection on `/resize`) is never exercised.

### `recommend-only`

Clone of `steady` — same `500m / 256Mi` requests, same ~`200m / ~100Mi`
load — but the Policy sets `spec.rightSizing.recommendOnly: true` (see the
[Policy reference](../reference/policy.md#specrightsizingrecommendonly)).
The update mode stays `deployment: Ongoing` on purpose: it is the
`recommendOnly` field, not the mode, that suppresses the apply. Run
`scenario-steady` alongside to watch a dry-run policy and an active one
coexist.

**Expected:**

- A `WorkloadRecommendation` is computed and cached with the same
  convergence target as `steady` (~`220m` / ~`110Mi`):

  ```bash
  kubectl get wlrec -n scenario-recommend-only deployment-stress -o yaml
  ```

- **The pod is never recycled**: same UID over time, requests stay
  `500m/256Mi` indefinitely — in `make test-scenario-status` the
  recommendation never converges with current and `RECYCLED` stays `no`:

  ```bash
  kubectl get pod -n scenario-recommend-only -l app=stress \
    -o jsonpath='{.items[0].metadata.uid}{" "}{.items[0].spec.containers[0].resources.requests}{"\n"}'
  ```

- **The webhook does not inject either**: delete the pod (only after the
  `WorkloadRecommendation` above exists — before that, the young-workload
  gate would also leave the replacement unmutated and the check proves
  nothing) and the replacement comes back with the template's `500m/256Mi`:

  ```bash
  kubectl delete pod -n scenario-recommend-only -l app=stress
  # wait for the replacement, then re-run the jsonpath above: still 500m/256Mi
  ```

- The operator says why nothing happens — the controller logs
  `recommend-only: computed recommendations` with `source=policy`, and the
  webhook logs `recommend-only: would inject resources` on pod creation:

  ```bash
  kubectl logs -n k8s-sustain -l app.kubernetes.io/name=k8s-sustain --tail=500 \
    | grep 'recommend-only'
  ```

### `recurring`

A namespace-scoped CronJob (`launcher`, every 2 minutes) plays an external
scheduler like Airflow: each run creates a bare Pod
`etl-recurring-<timestamp>` (no `ownerReferences`) with the stable
`k8s.sustain.io/owner-name: etl-recurring` annotation, waits ~30s for it to
finish, then **deletes it**. That leaves ~80s of every 120s with no pod of the
identity at all — longer than the harness's 30s reconcile interval — so the
controller must keep the identity alive from its `WorkloadRecommendation`
alone. `recurring.yaml`'s header holds the measured chronologies.

**Expected:**

- `pod-etl-recurring` survives every gap and `status.observedAt` advances
  while no pod exists; `k8s_sustain_wlr_refresh_total` counts those refreshes.
- The first ~6 runs are admitted at the template's `500m / 256Mi` while the
  10-minute workload-age gate holds
  (`recommendation_skipped_total{reason="workload_too_young"}` climbs).
- After the gate the percentile is computed over a mostly-empty series (a
  ~30s pod every 120s): at `WINDOW=10m` CPU floors to `1m` with realistic
  memory; at `WINDOW=2m` memory also collapses and every run is OOM-killed at
  start. This is a known limitation of duty-cycled identities, not rescued by
  the OOM floor (a `restartPolicy: Never` pod never gets a
  `LastTerminationState`); use a window that spans several runs. See
  [Standalone Pods & Identity Grouping](standalone-pods-and-grouping.md).

```bash
kubectl get pods -n scenario-recurring -l app=etl-recurring
kubectl get wlrec -n scenario-recurring pod-etl-recurring \
  -o jsonpath='{.status.observedAt}{"\n"}'
kubectl get pod -n scenario-recurring -l app=etl-recurring \
  --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{.items[-1].spec.containers[0].resources}{"\n"}'
```

### `namespace-optin`

Two Deployments in one namespace, same shape as `steady`, but neither
carries a `k8s.sustain.io/policy` annotation of its own anywhere: `web` opts
in purely through the Namespace's `k8s.sustain.io/policy` annotation, and
`opted-out` sets `k8s.sustain.io/opt-out: "true"` on its own
`metadata.annotations` to override that inherited opt-in. Validates the
delegated, most-specific-first annotation resolution (see the [Annotation
reference](../reference/annotation.md)).

**Expected:**

- `web`: CPU request drops from `500m` to ~`220m`, memory from `256Mi` to
  ~`110Mi`, within `WINDOW + reconcile_interval` — exactly like `steady`,
  despite carrying no annotation of its own anywhere.
- `opted-out`: requests stay pinned at `500m / 256Mi` forever; it is never
  reconciled.

  ```bash
  kubectl get deploy -n scenario-namespace-optin \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[0].resources.requests}{"\n"}{end}'
  ```

## Observability

`make test-scenario-status` prints a table:

```text
NAMESPACE                    POD                    CPUreq    CPUrec    MEMreq    MEMrec    RECYCLED
scenario-overprovisioned     stress-xxxxx           1000m     62m       512Mi     48Mi      yes
scenario-steady              stress-yyyyy           500m      230m      256Mi     115Mi     yes
```

The dashboard remains the richer source of truth — start a port-forward
and open `http://localhost:8090`:

```bash
kubectl port-forward -n k8s-sustain svc/k8s-sustain-dashboard 8090:8090
```

Every scenario appears in the table, including CronJob/Job runs (newest pod),
bare pods, and the `custom-name` override identity.

## Adding a new scenario

See [`hack/scenarios/README.md`](https://github.com/noony/k8s-sustain/blob/main/hack/scenarios/README.md).
