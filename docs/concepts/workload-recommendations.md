# Workload Recommendations (cache CRD)

`WorkloadRecommendation` (`kubectl get wlrec`) is a namespaced custom resource that caches the most recent recommendation the controller computed for one workload identity. The admission webhook never queries Prometheus, so this cache is its **only** source of recommendations at pod admission.

## Why it exists

Only the controller talks to Prometheus, on its reconcile cadence (`--reconcile-interval`, default 5m). The webhook has no Prometheus client, so without a shared cache it would have nothing to inject. Keeping the last-known-good value in the cluster API means:

- All webhook replicas see the same value, and add no Prometheus load.
- Webhook restarts don't lose the last computed recommendation.
- Operators can inspect what the webhook would inject with `kubectl get wlrec`.

## How it works

1. **Write path (controller).** Each reconcile, **discovery** ensures a `WorkloadRecommendation` exists for every live identity the policy governs (no Prometheus queries), then **computation** recomputes every identity the policy governs, Departed ones included, and writes its recommendation and outcome back — see [Computation](architecture.md#computation). Expect one object per governed identity from the first reconcile, most of them briefly empty.

    The object's name is `<lowercase-kind>-<workload-name>` in the workload's namespace (names over 253 characters are truncated with a stable hash suffix). A write is skipped when the recommendation is unchanged **and** `observedAt` is less than 10 minutes old, so etcd writes scale with change rather than workload count, while `observedAt` on stable workloads never approaches the webhook's 30-minute staleness window.

2. **Stub write path (webhook).** When a pod is admitted for an identity with no `WorkloadRecommendation`, the webhook creates an empty-status stub. See [Cold start](#cold-start-stub-recommendations).

3. **Read path (webhook).** Every admission reads the cache; see [Admission behaviour](#admission-behaviour).

4. **Garbage collection (controller).** Three independent mechanisms:

    - **Per-cycle sweep** — at the end of each reconcile, deletes objects carrying the policy's label whose identity no Policy governs any more (annotation removed, namespace excluded, kind disabled, selector narrowed), subject to [retention](#retention-for-ephemeral-workloads). A Conflicted identity's object is kept frozen, and one now governed by another Policy is left for that Policy to adopt.
    - **Policy-deletion finalizer** — `k8s.sustain.io/cleanup` on every `Policy` blocks its deletion until every owned `WorkloadRecommendation` is deleted.
    - **Orphan reaper** — every 10 minutes, deletes objects whose `spec.policy` names a Policy that no longer exists (e.g. after `kubectl delete policy --force --grace-period=0`, which skips finalizers).

    Objects belonging to another policy are never touched. Deleting a namespace deletes its recommendations.

## Cold start: stub recommendations

Discovery only creates objects for identities it sees alive. A Job that runs for ninety seconds, or a bare-pod group that is up only between two reconciles, may never be seen. Stubs get those identities into the cache:

1. **The webhook creates a stub.** On an admission with no `WorkloadRecommendation` for the identity, the webhook creates one with `spec.workloadRef`, `spec.policy`, the `k8s.sustain.io/policy` label, the `k8s.sustain.io/stub: "true"` label and an empty `status`, records the pod's per-container requests and limits in `status.observedResources`, and admits the pod unchanged. The write runs asynchronously, never blocks the admission, and is a `Create` (never an `Update`), so it cannot clobber a populated recommendation. The container snapshot lets computation work on an identity that has no workload object left to read a template from.

2. **Computation fills it in.** From then on the identity is in the controller's work-list and is recomputed every reconcile through the same pipeline as every other object.

3. **`NoData` and `TooYoung` are retried.** If the identity produces nothing — too young for the [workload-age gate](recommendation-pipeline.md#stages), or no usable samples yet — computation sets `status.outcome` to `TooYoung` or `NoData`. That is not terminal: the next reconcile recomputes it, so a new identity converges within one reconcile interval of Prometheus having enough history. Neither outcome overwrites an existing recommendation.

Because retained objects are recomputed whether or not their workload is alive, a standalone Job or recurring bare-pod group converges even if no reconcile ever catches it running — as long as the object survives the gap between runs (see [retention](#retention-for-ephemeral-workloads)). An identity whose **name changes on every run** (timestamp or hash suffix) never accumulates history; give it a stable name, run it under a CronJob, or group runs with the [`k8s.sustain.io/owner-name` annotation](../guides/standalone-pods-and-grouping.md).

The first pod of a never-before-seen identity always starts on its template resources; the stub is for the *next* pod. To confirm cold start is converging, watch `k8s_sustain_webhook_recommendation_source_total` move from `missing` to `hit`. `kubectl get wlrec -l k8s.sustain.io/stub` lists identities first seen by the webhook.

## Schema

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: WorkloadRecommendation
metadata:
  name: deployment-web              # <kind>-<name>
  namespace: example
  labels:
    k8s.sustain.io/policy: production-rightsizing
    # k8s.sustain.io/stub: "true"   # present when the webhook created the object
spec:
  policy: production-rightsizing    # the Policy that last governed the identity
  workloadRef:
    kind: Deployment
    namespace: example
    name: web
status:
  observedAt: 2026-05-01T12:34:56Z  # when containers was last computed; the webhook
                                    # trusts ≤30m old, unless departed or Conflicted
  outcome: Computed                 # the last pass: Computed, NoData, TooYoung,
                                    # FetchFailed or Conflicted; unset = not decided yet.
                                    # Every outcome but Computed keeps containers
  departed: false                   # true = retained for an identity with no live
                                    # member; exempt from the staleness check, see below
  containers:
    app:
      cpuRequest: 250m
      memoryRequest: 256Mi
      cpuLimit: 500m
      memoryLimit: 512Mi
      removeCpuLimit: false         # true when the policy says NoLimit
      removeMemoryLimit: false
  trace:                            # how each container's values were derived
    app:
      cpu:
        percentile: 227500u         # usage percentile of the busiest pod
        withHeadroom: 228m          # + headroom, rounded up
        clamped: 228m               # after minAllowed/maxAllowed
        coordination:               # only when an autoscaler shapes it
          overheadFactor: 1.375     # 110 / HPA target (80%)
          scaled: 314m
          value: 314m               # re-clamped: the final request
        limit: 628m
      memory:
        percentile: "183500800"
        oomFloor:                   # only after a recent OOM kill
          value: 240Mi
          determined: true          # the floor set the final request
        withHeadroom: 264Mi
        clamped: 264Mi
        coordination:
          overheadFactor: 1         # the HPA has no memory target
          scaled: 264Mi
          value: 264Mi
  observedResources:                # what the workload actually ran with
    app:
      cpuRequest: "1"
      memoryRequest: 1Gi
      cpuLimit: "2"
      memoryLimit: 2Gi
    istio-proxy:
      init: true                    # init or sidecar container
      cpuRequest: 100m
      memoryRequest: 128Mi
```

`removeCpuLimit` / `removeMemoryLimit` carry the Policy's `NoLimit` intent, since a nil `cpuLimit`/`memoryLimit` alone cannot distinguish "leave alone" from "remove".

`trace` is the record of how each container's values were derived: per resource, the request after every [stage](recommendation-pipeline.md#stages) that ran, in order, then the limit derived from the final request (`limit`, or `removeLimit`; neither means the container keeps its own). A stage that did not run is absent: `oomFloor` without a recent OOM kill, `coordination` without autoscaler coordination, `percentile` for memory anchored on an OOM kill alone; a resource whose request is kept has no trace. `percentile` is stored at full precision (nanocores, bytes). `coordination.replicaFactor` appears for CPU when the Policy sets a `replicaBudgetAnchor`, and `coordination.value` differs from `scaled` when a min/max clamp capped the coordinated request. `oomFloor.determined` says the floor set the final request: it beat the percentile and no clamp, before or after coordination, replaced it. The trace is written together with `containers`; a pass whose values are unchanged but whose trace moved (a percentile that rounds to the same request) writes nothing, so the trace can lag the latest samples by up to the 10-minute refresh.

`observedResources` is written by discovery from the union of the identity's governed members' containers (or by the webhook from the admitted pod). It keeps current-vs-recommended visible on the dashboard after the workload is gone, and supplies the container list for identities with no workload object.

`kubectl get wlrec` shows `outcome` as a column next to the policy.

| `status.outcome` | Meaning | `containers` |
| --- | --- | --- |
| `Computed` | A Recommendation was computed on the last pass | Fresh, with its `trace` |
| `NoData` | Prometheus answered with nothing recommendable | Last Recommendation kept, if any |
| `TooYoung` | The identity is younger than the [minimum age](recommendation-pipeline.md#stages) | Last Recommendation kept, if any |
| `FetchFailed` | The identity's inputs could not be read; retried with backoff | Last Recommendation kept, if any |
| `Conflicted` | Its members opt into different Policies; nothing is recomputed | Frozen |

## Conflicted identities

An identity is **Conflicted** when its members are governed by different Policies — `api-blue` opting into `p` and `api-green` into `q`, both grouped as `Deployment/api`, or two bare pods sharing an owner-name. A member that opts out, or whose Policy does not accept it (missing, kind not managed, selector not matching), is no party to a conflict.

No Policy governs a Conflicted identity until its members agree again ([ADR 0002](../adr/0002-conflicted-identity-freezes-its-recommendation.md)):

- The controller does not rewrite its `WorkloadRecommendation`'s `spec.policy` or label, does not recompute it and applies nothing to its pods. It only records `status.outcome: Conflicted`.
- The stored Recommendation stays frozen, exempt from the staleness check like a departed one and bounded by the same retention window. The webhook injects it only into pods that opt into the Policy named in `spec.policy` (counted as `retained`); pods of the other Policy are admitted unchanged (counted as `other-policy`).
- It counts towards neither Policy's `k8s_sustain_policy_workload_count`, and loses the controller's health series (pods, stale pods, retry state). The dashboard lists it once, in the Conflicted state, under no Policy.
- The controller logs every reconcile of a party Policy until the conflict is resolved. Fix it by aligning the members' policy annotations or giving them distinct `k8s.sustain.io/owner-name` values.

## Retention for ephemeral workloads

By default a `WorkloadRecommendation` is deleted as soon as no Policy governs its identity. Ephemeral workloads need a different rule, so the sweep distinguishes two cases:

- **The identity is Departed** — no live member left: the objects were deleted (bare pods, TTL- or hook-deleted Job) or the only one is a Job in a terminal state. The recommendation is kept for the retention window (`--recommendation-retention` / Helm `controller.recommendationRetention`, default `168h`). The dashboard shows it as a *Departed* identity. Set `0` for immediate cleanup.
- **The identity still has live members but opted out** (annotation removed, policy no longer matching, namespace excluded) — it is swept on the next reconcile regardless of retention. Objects created in the last 10 minutes are never swept.

**Retention decides whether recurring ephemeral workloads are sized at admission.** If the object is deleted between two runs, the next run cold-starts on template resources. Retention must exceed the longest gap between runs of the same identity: the `168h` default covers a weekly job; a monthly job needs more.

**The clock starts late.** A retained identity keeps being recomputed, and `observedAt` keeps advancing, while its samples are still inside the query window. Retention only starts counting once they age out, so a departed recommendation lives roughly **`window + retention`** — about 14 days with a `168h` window and the default retention. Budget object count and webhook memory against that figure.

**Retained recommendations bypass the staleness check.** When the sweep finds an identity Departed and keeps its recommendation, it sets `status.departed: true`. The webhook serves a departed recommendation regardless of `observedAt` age — counted as `retained` rather than `hit` — until the retention window (checked against the webhook's own `--recommendation-retention`, rendered from the same Helm value) runs out; after that it counts as `stale`. The flag is cleared as soon as the identity has a live member again, and is only set when the controller's inventory read succeeded, so a workload the controller is merely failing to refresh still trips the staleness check.

**Cost depends on whether the identity's name is stable.** The webhook's informer caches every retained object:

| Identity | Objects retained | Effect of raising retention |
| --- | --- | --- |
| CronJob-owned Jobs | One per CronJob | None — bounded |
| Bare pods, or standalone Jobs, carrying `k8s.sustain.io/owner-name` | One per annotation value | None — bounded |
| Standalone Jobs **without** that annotation | One per *run* (the Job's generated name) | Grows as runs-per-day × retention-days |

Annotate recurring Jobs with [`k8s.sustain.io/owner-name`](../guides/standalone-pods-and-grouping.md) before raising retention much past the default.

## Admission behaviour

The webhook makes no Prometheus calls, so every admission follows the same rule. Each outcome increments `k8s_sustain_webhook_recommendation_source_total{source}`, the only place it is visible that a pod started on template resources:

- **`hit`** — `observedAt` within 30 minutes: the cached recommendation is injected.
- **`retained`** — a departed or Conflicted recommendation within the retention window: injected.
- **`other-policy`** — the object's `spec.policy` is not the Policy the pod opts into (the other side of a [Conflicted identity](#conflicted-identities), or an identity moving between Policies before the new one adopts it): admitted unchanged, no stub.
- **`stale`** — `observedAt` older than 30 minutes: the pod is admitted with its template resources. The controller has fallen behind (stuck reconcile, backlog, or Prometheus unreachable from the controller).
- **`missing`** — no object: the pod is admitted unchanged and a stub is created. Expected once per new identity; a sustained rate for one identity means the controller never computes it.
- **`nodata`** — the object exists and has an outcome but holds no Recommendation yet: admitted unchanged.
- **`error`** — the read failed (apiserver, RBAC or cache problem): admitted unchanged.

!!! warning "Keep `--reconcile-interval` well below 30 minutes"
    The 30-minute staleness window is fixed. The controller refreshes `observedAt` on each reconcile, so with `--reconcile-interval` at or above 30m every recommendation goes stale between reconciles and new pods start on template resources.

The webhook never sees pods in the release namespace, `kube-system`, `kube-public`, or any namespace in the Helm value `excludedNamespaces`: the `MutatingWebhookConfiguration`'s `namespaceSelector` excludes them, so no annotation there has any effect at admission.

## Observability

- `kubectl get wlrec -A` lists every cached recommendation.
- `kubectl describe wlrec deployment-web -n example` shows the full status for one workload.
- `status.observedAt` shows how fresh the data the webhook would serve is.
- `status.trace` shows why a container has the values it has; the dashboard's workload detail page renders it as a table.

## RBAC

The controller and webhook share a ServiceAccount with cluster-wide access to `workloadrecommendations`; the dashboard has read-only access. See [Runtime security](../security.md#runtime-security).

Every k8s-sustain controller reconciles every `Policy` in the cluster, so install a single release per cluster.
