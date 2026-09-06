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
  /api/v1/namespaces/k8s-sustain/services/k8s-sustain-controller:8080/proxy/metrics \
  | grep 'k8s_sustain_recommended_cpu_cores{.*container_kind="init"'
```

### `cronjob`

Single CronJob (`schedule: "* * * * *"`) running for ~30s per invocation
with steady ~200m CPU / ~100MiB memory load. Initial requests are
deliberately oversized at `500m / 256Mi`. `Ongoing` mode is enabled for
CronJobs.

**Expected:** the CronJob spec is **never modified** (no GitOps drift).
Currently-running job pods are resized in place via the `pods/resize`
subresource when the cluster supports it; otherwise they finish on their
existing resources. New runs (next minute boundary) spawn with updated
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
scenario that exercises the new in-place resize path on a `restartPolicy:
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
itself is intentionally unchanged. Confirms the new pod-level path
handles large recommendation deltas just as well as the old spec-patching
path did.

### `job`

A standalone `batch/v1` Job (no CronJob owner) with one ~5-minute run.
The controller does not reconcile standalone Jobs, so this scenario
exercises only the webhook's `Pod → Job` resolution branch.

**Expected:** First run starts with the original `500m / 256Mi` requests
(no history yet). Re-apply the Job after `WINDOW` elapses — the second
run's pod is webhook-injected with the percentile-based recommendation
(~`60m / 35Mi`), and the webhook upserts a `WorkloadRecommendation`
(`job-oneshot`) for the identity.

Note the re-applied Job object is only seconds old at admission — the
10-minute workload-age gate passes because, for `Job`-kind workloads, it
keys on the age of the identity's accumulated Prometheus history (from
the first run) rather than the Job object's `CreationTimestamp`. The
first run has no history, so it is skipped by the gate and leaves no
`WorkloadRecommendation` behind.

```bash
kubectl delete -f hack/scenarios/job.yaml
make test-scenario-job
kubectl get pod -n scenario-job -l app=stress \
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

- The full 3-pod recycle finishes in roughly one reconcile cycle. If the
  recycle wait ever regressed back to keying on pod name (which the
  StatefulSet controller reuses across replacements), each pod would
  block for the full `--replacement-timeout` (5 min default) and the run
  would stretch to 15+ minutes before failing. Watch the controller log:

  ```bash
  kubectl logs -n k8s-sustain -l app.kubernetes.io/name=k8s-sustain \
    -c controller --since=5m | grep -E 'evict|recycle|replacement'
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
  curl -s localhost:8090/api/workloads/scenario-custom-name/Deployment/renamed-app/recommendations
  ```

- `status.sh`'s generic table queries recommendations by the Deployment's own
  name (`stress`), which the override deliberately bypasses, so `custom-name`
  is not included in it; use the commands above instead.

### `bare-pod`

A standalone `Pod` — no Deployment, no `ownerReferences` at all — simulating
Airflow's `KubernetesPodOperator`, which by default launches a Pod directly
with no parent Job. The policy and `k8s.sustain.io/owner-name: etl-daily`
annotations live directly on the Pod (there's no pod template for a bare pod
to carry them on). The Policy sets `rightSizing.update.types.pod: Ongoing`.
Initial requests are deliberately oversized at `500m / 256Mi`; actual usage
is the same ~`200m / ~100Mi` profile as `steady`.

**Expected:**

- The pod is admitted with the webhook-mirrored label. Confirm:

  ```bash
  kubectl get pod -n scenario-bare-pod etl-daily-run-1 --show-labels | grep k8s.sustain.io/owner-name
  ```

- **The pod is resized in place, never evicted.** Eviction is permanently off
  for `Pod`-kind targets — nothing would recreate the pod — but an in-place
  resize needs no controller behind it, so under `Ongoing` the running pod is
  corrected directly through the `pods/resize` subresource. The UID never
  changes; only the requests do:

  ```bash
  kubectl get pod -n scenario-bare-pod etl-daily-run-1 \
    -o jsonpath='{.metadata.uid}{" "}{.spec.containers[0].resources.requests}{"\n"}'
  # same UID throughout; CPU comes down from 500m toward ~220m
  ```

  A memory decrease can be reported `Infeasible`/`Deferred` by the kubelet;
  the patcher logs it and skips, and never falls back to eviction here. On a
  cluster below k8s 1.33 there is no in-place support at all, so the pod
  stays at `500m / 256Mi` and only the recommendation below appears.

  The scenario pod leaves `restartPolicy` unset, so it defaults to `Always`
  and resizes on any k8s ≥ 1.33. Do not generalise that to real Airflow
  pods: `KubernetesPodOperator` uses `restartPolicy: Never`, which is only
  fully covered from k8s ≥ 1.35 — on 1.33/1.34 such a pod's resize is
  rejected and it keeps its admitted resources.

- The controller computes and caches a `WorkloadRecommendation`
  (`pod-etl-daily`) from the pod's actual usage — this is also the webhook's
  *only* source of recommendations (it never queries Prometheus itself), so
  a later pod sharing this `owner-name` (e.g. the next Airflow DAG run) gets
  injected from it as long as the cache is still fresh when that pod is
  admitted. Like every other kind, this waits out the 10-minute
  `MinWorkloadAge` gate — bare pods are no longer exempt from it, since an
  `Ongoing` bare pod can now be resized in place, so a near-zero percentile
  from a too-young identity is no longer harmless. Expect the first cached
  recommendation once the `etl-daily` identity has been known for 10+
  minutes, not within the first couple of reconcile cycles:

  (The identity's history-age gate is applied by the controller when it
  builds the recommendation to cache, not by the webhook at admission — the
  webhook no longer computes anything of its own, so a pod re-created under
  the same `owner-name` within ~10 minutes of the identity's first samples
  simply has no cache entry yet to inject from and keeps its template
  resources.)

  ```bash
  kubectl get workloadrecommendation -n scenario-bare-pod
  kubectl port-forward -n k8s-sustain svc/k8s-sustain-dashboard 8090:8090 &
  curl -s localhost:8090/api/workloads/scenario-bare-pod/Pod/etl-daily/recommendations
  ```

- Not in `status.sh`'s generic table (it assumes a Deployment per scenario);
  use the commands above instead.

### `oom-kill`

Single-container Deployment that quietly holds ~30Mi for 60 s, then attempts
to allocate 120Mi against a 96Mi cgroup memory limit — the kernel kills the
container, Kubernetes restarts it, and the cycle repeats.

**Expected:**

- `kubectl get pods -n scenario-oom-kill` shows `OOMKilled` in the last
  termination reason and a growing restart count.
- `k8s_sustain:workload_oom_24h{owner_name="stress"}` becomes positive.
- Memory recommendation **does not shrink** despite most samples being
  quiet (~30Mi). The OOM-aware floor pulls the reco to
  `max(peak_working_set_24h, current_request)` plus headroom — i.e. ≥ 96Mi.
- `k8s_sustain_oom_floor_applied_total{owner_name="stress"}` increments on
  each reconcile while the OOM is within the 24 h window.

```bash
kubectl get wlrec -n scenario-oom-kill stress -o yaml
kubectl --raw \
  /api/v1/namespaces/k8s-sustain/services/k8s-sustain-controller:8080/proxy/metrics \
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
kubectl logs -n k8s-sustain deploy/k8s-sustain-controller \
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

A namespace-scoped `CronJob` ("launcher", every 2 minutes) plays the role of
an external scheduler like Airflow. Its RBAC is `create`/`get`/`delete` on
`pods` in this namespace only (`ServiceAccount`/`Role`/`RoleBinding`, no
`ClusterRole`); each run computes a unique pod name
(`etl-recurring-$(date +%s)`, so plain `create` never collides), creates a
genuinely bare Pod — **no `ownerReferences` at all** — carrying a stable
`k8s.sustain.io/owner-name: etl-recurring` annotation, waits for the ~30s
load to finish, and then **deletes the pod**.

The delete is what makes the scenario work. `workload.GroupBarePods` does
not filter on pod phase, so a `Succeeded` pod left behind still reports the
identity in every target listing — the identity would never depart and the
departed-refresh path would never run. With the delete, each cycle leaves
roughly 80s of every 120s in which **no pod of the identity exists at all**,
comfortably longer than the harness's deployed `--reconcile-interval=30s`.

**Expected — measured on a real kind cluster (k8s 1.35); see
`recurring.yaml`'s header comment for the full chronologies and reasoning:**

- The `WorkloadRecommendation` (`pod-etl-recurring`) survives every gap, and
  `status.observedAt` advances on reconciles taken while the pod list is
  confirmed empty — this is the regression the branch fixes. The direct
  counter is `k8s_sustain_wlr_refresh_total`, emitted only from
  `refreshDepartedRecommendation`:

  ```bash
  kubectl get pods -n scenario-recurring -l app=etl-recurring   # empty
  kubectl get wlrec -n scenario-recurring pod-etl-recurring \
    -o jsonpath='{.status.observedAt}{"\n"}'                    # advances
  ```

- The 10-minute `MinWorkloadAge` gate holds correctly for the first 6 runs
  — all admitted at the template's `500m / 256Mi`,
  `recommendation_skipped_total{reason="workload_too_young"}` climbing.

- **The moment the gate clears is not a clean convergence, and what happens
  next depends on `WINDOW`.** The gate is time-based, not data-volume-based:
  a pod that runs ~30s per 120s has contributed only ~2.5 minutes of real
  container runtime by the time the 10-minute gate opens, so the percentile
  is computed over a mostly-empty series.

  - At the harness default **`WINDOW=10m`** (plain
    `make test-scenario-recurring`): CPU floors to `1m` and stays there;
    memory stays realistic (`53–62Mi` request, `81–93Mi` limit); every pod
    completes and **nothing is killed**. Undramatic and long-lived — it did
    not self-heal within 14 minutes of observation.
  - At **`WINDOW=2m`**: the gate clears straight into `cpu=1m,
    mem=4Mi/6Mi`, and every subsequent pod is OOM-killed the instant it
    starts (`exitCode: 137`, `reason: OOMKilled`, `startedAt ==
    finishedAt`). This is self-sustaining — a pod that dies at `t=0`
    contributes no samples, so the floor re-derives itself. Observed
    unbroken for 18 minutes with no recovery.

  Either way it is a real, currently-expected design gap — recorded, not
  fixed by this scenario. It is **not** rescued by the OOM-aware memory
  floor, which structurally cannot fire here: both detection paths key on
  `LastTerminationState`, which Kubernetes only populates after a container
  restart, and a `restartPolicy: Never` pod that dies once never gets one
  (the captured kill showed `lastState: {}`):

  ```bash
  kubectl get pod -n scenario-recurring -l app=etl-recurring \
    --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{.items[-1].spec.containers[0].resources}{"\n"}'
  ```

- `status.departed: true` is normally not visible while the launcher keeps
  ticking at the default window, because the recommendation keeps changing and
  each refresh therefore rewrites `observedAt`, so the sweep's 10-minute grace
  period never lapses. A refresh does *not* rewrite `observedAt`
  unconditionally: `wlrcache.Upsert` skips the write when the new status is
  equivalent to the stored one and `observedAt` is younger than
  `wlrcache.RefreshInterval` (10 minutes) — the same 10 minutes as the grace
  period, by coincidence of two independently tuned constants rather than by
  design. To see `departed` deliberately, suspend the launcher and budget
  ~15 minutes; measured at 10m01s after the last `observedAt` write, with the
  recommendation still retained. Exact commands are in `recurring.yaml`'s
  header.

- Not in `status.sh`'s generic table (it assumes a Deployment per
  scenario); use the commands above instead.

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
