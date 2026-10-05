# Workload Recommendations (cache CRD)

`WorkloadRecommendation` (`kubectl get wlrec`) is a namespaced custom resource that caches the most recent recommendation the controller computed for one workload identity. The admission webhook never queries Prometheus, so this cache is its **only** source of recommendations at pod admission.

## Why it exists

Only the controller talks to Prometheus, on its reconcile cadence (`--reconcile-interval`, default 5m). The webhook has no Prometheus client, so without a shared cache it would have nothing to inject. Keeping the last-known-good value in the cluster API means:

- All webhook replicas see the same value, and add no Prometheus load.
- Webhook restarts don't lose the last computed recommendation.
- Operators can inspect what the webhook would inject with `kubectl get wlrec`.

## Lifecycle

The object's name is `<lowercase-kind>-<workload-name>` in the workload's namespace (names over 253 characters are truncated with a stable hash suffix). Every write to it, and every judgement on it, goes through one module (`internal/wlrcache`), so which component owns which field is decided in one place.

### Writers

| Writer | When | What it writes |
| --- | --- | --- |
| **Request** (webhook) | A pod is admitted for an identity with no object, or one the controller keeps no live view of | Creates an empty-status [stub](#cold-start-stub-recommendations) carrying the pod's containers in `status.observedResources`; refreshes that snapshot for a departed identity. Never touches a Recommendation |
| **Ensure** (controller, discovery) | Every reconcile, for each live identity the Policy governs | Creates the object or adopts it (`spec.policy` and the `k8s.sustain.io/policy` label become the governing Policy's), replaces `status.observedResources` with the members' containers when it differs, clears `status.departed` |
| **Record** (controller, computation) | Every reconcile, once per identity on the work-list, and for each Conflicted identity the Policy is party to | One status write with the pass's decision: `status.outcome`, `status.departed`, and for `Computed` the Recommendation, its `trace`, `observedAt` and `computedBy`. An unchanged decision costs no write |

Record is the only writer of the Recommendation and of `departed`, so a departed identity whose samples are still in its query window costs at most one status write per 10 minutes, the interval after which an unchanged Recommendation is rewritten only to refresh `observedAt`. That keeps `observedAt` on a stable workload well inside the webhook's 30-minute staleness budget while etcd writes scale with change rather than workload count.

The controller applies an identity's Recommendation only once its Record has landed: an identity whose object could not be ensured or written is left alone that cycle and counted as failed, because the replacement pods of an eviction would read the old value at admission.

### States

| State | How it gets there | `status` |
| --- | --- | --- |
| Undecided | Request or Ensure created it; no pass has recorded anything yet | No `outcome`, no `containers` |
| No Recommendation yet | A pass recorded `NoData`, `TooYoung`, `FetchFailed` or `Conflicted` and none was ever computed | `outcome` set, no `containers` |
| Computed | A pass of the governing Policy computed a Recommendation | `outcome: Computed`, `containers`, `computedBy`, fresh `observedAt` |
| Kept | A later pass recorded another outcome | That outcome; the last `containers`, `computedBy` and `observedAt` kept |
| Departed | The identity has no live member left | `departed: true`, with whatever the last pass recorded |
| Conflicted | Its members are governed by different Policies | `outcome: Conflicted`; everything else frozen ([below](#conflicted-identities)) |
| Adopted | Another Policy now governs the identity | `spec.policy` names the new Policy; `computedBy` still names the old one until the new one computes |

### A Recommendation is served only to its Policy

The webhook injects a Recommendation only into pods whose resolved Policy both governs the object (`spec.policy`) and computed the values (`status.computedBy`) — [ADR 0003](../adr/0003-a-recommendation-is-served-only-to-its-policy.md). When another Policy adopts an identity, the previous Policy's values stay in the object but are withheld from every pod until the new Policy records a Recommendation of its own; until then pods start on their template resources, whatever the new Policy's first passes record (too young under its minimum age, no data under its window, a failed fetch).

An object written before `status.computedBy` existed carries none, and is withheld until its Policy records a Recommendation: a live identity with samples gets one on its next reconcile.

### Garbage collection

Three independent mechanisms delete objects, each only at the revision it judged (its UID and `resourceVersion`), so an object rewritten since it was listed — adopted by another Policy, say — is judged again rather than deleted:

- **Per-cycle sweep** — at the end of each reconcile, deletes the objects naming the Policy whose identity no Policy governs any more (annotation removed, namespace excluded, kind disabled, selector narrowed), and the objects of Departed and Conflicted identities past the [retention window](#retention-for-ephemeral-workloads). An object created in the last 10 minutes is never swept, and one now governed by another Policy is left for that Policy to adopt.
- **Policy-deletion finalizer** — `k8s.sustain.io/cleanup` on every `Policy` blocks its deletion until every object still naming it is deleted. An object another Policy adopts while the cleanup runs (a GitOps rename creates the new Policy before deleting the old) survives: the delete conflicts, and the retry no longer lists it.
- **Orphan reaper** — every 10 minutes, deletes objects whose `spec.policy` names a Policy that no longer exists (e.g. after `kubectl delete policy --force --grace-period=0`, which skips finalizers).

Deleting a namespace deletes its recommendations.

## Cold start: stub recommendations

Discovery only creates objects for identities it sees alive. A Job that runs for ninety seconds, or a bare-pod group that is up only between two reconciles, may never be seen. Stubs get those identities into the cache:

1. **The webhook creates a stub.** On an admission with no `WorkloadRecommendation` for the identity, the webhook creates one with `spec.workloadRef`, `spec.policy`, the `k8s.sustain.io/policy` label, the `k8s.sustain.io/stub: "true"` label and an empty `status`, records the pod's per-container requests and limits in `status.observedResources`, and admits the pod unchanged. The write runs asynchronously, never blocks the admission, and is a `Create` (never an `Update`), so it cannot clobber a populated recommendation. The container snapshot lets computation work on an identity that has no workload object left to read a template from.

2. **Computation fills it in.** From then on the identity is in the controller's work-list and is recomputed every reconcile through the same pipeline as every other object. Admissions before that reconcile read the object as *undecided* and create nothing more.

3. **`NoData` and `TooYoung` are retried.** If the identity produces nothing — too young for the [workload-age gate](recommendation-pipeline.md#stages), or no usable samples yet — computation sets `status.outcome` to `TooYoung` or `NoData`. That is not terminal: the next reconcile recomputes it, so a new identity converges within one reconcile interval of Prometheus having enough history. Neither outcome overwrites an existing recommendation.

4. **Later runs keep the snapshot current.** While the identity is departed (or has no snapshot), an admitted pod whose containers differ from `status.observedResources` replaces it, so an identity the controller never catches alive is computed against its latest run, not its first.

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
  computedBy: production-rightsizing # the Policy that computed containers; they are
                                    # injected only into that Policy's pods
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

`computedBy` is written with every `Computed` outcome and kept by every other one, like `containers`. It differs from `spec.policy` only after another Policy adopted the identity and before that Policy computed a Recommendation of its own; the webhook then withholds `containers` from every pod (see [above](#a-recommendation-is-served-only-to-its-policy)).

`trace` is the record of how each container's values were derived: per resource, the request after every [stage](recommendation-pipeline.md#stages) that ran, in order, then the limit derived from the final request (`limit`, or `removeLimit`; neither means the container keeps its own). A stage that did not run is absent: `oomFloor` without a recent OOM kill, `coordination` without autoscaler coordination, `percentile` for memory anchored on an OOM kill alone; a resource whose request is kept has no trace. `percentile` is stored at full precision (nanocores, bytes). `coordination.replicaFactor` appears for CPU when the Policy sets a `replicaBudgetAnchor`, and `coordination.value` differs from `scaled` when a min/max clamp capped the coordinated request. `oomFloor.determined` says the floor set the final request: it beat the percentile and no clamp, before or after coordination, replaced it. The trace is written together with `containers`; a pass whose values are unchanged but whose trace moved (a percentile that rounds to the same request) writes nothing, so the trace can lag the latest samples by up to the 10-minute refresh.

`observedResources` is written by discovery from the union of the identity's governed members' containers (or by the webhook from the admitted pod, for an identity the controller sees no live member of). It keeps current-vs-recommended visible on the dashboard after the workload is gone, and supplies the container list for identities with no workload object.

`kubectl get wlrec` shows `outcome` as a column next to the policy.

| `status.outcome` | Meaning | `containers` |
| --- | --- | --- |
| `Computed` | A Recommendation was computed on the last pass | Fresh, with its `trace` and `computedBy` |
| `NoData` | Prometheus answered with nothing recommendable | Last Recommendation kept, if any |
| `TooYoung` | The identity is younger than the [minimum age](recommendation-pipeline.md#stages) | Last Recommendation kept, if any |
| `FetchFailed` | The identity's inputs could not be read; retried with backoff | Last Recommendation kept, if any |
| `Conflicted` | Its members opt into different Policies; nothing is recomputed | Frozen |

## Conflicted identities

An identity is **Conflicted** when its members are governed by different Policies — `api-blue` opting into `p` and `api-green` into `q`, both grouped as `Deployment/api`, or two bare pods sharing an owner-name. A member that opts out, or whose Policy does not accept it (missing, kind not managed, selector not matching), is no party to a conflict.

No Policy governs a Conflicted identity until its members agree again ([ADR 0002](../adr/0002-conflicted-identity-freezes-its-recommendation.md)):

- The controller does not rewrite its `WorkloadRecommendation`'s `spec.policy` or label, does not recompute it and applies nothing to its pods. It only records `status.outcome: Conflicted` (and, since its members are live, clears `status.departed`).
- The stored Recommendation stays frozen, exempt from the staleness check like a departed one, and kept by the sweep for the same retention window. The webhook injects it only into pods of the Policy that computed it (counted as `retained`); pods of the other Policy are admitted unchanged (counted as `other-policy`).
- When the conflict resolves, the Policy that governs again adopts the object. If that is the other Policy, the frozen values were never computed for its pods and stay withheld until it computes its own ([ADR 0003](../adr/0003-a-recommendation-is-served-only-to-its-policy.md)).
- It counts towards neither Policy's `k8s_sustain_policy_workload_count`, and loses the controller's health series (pods, stale pods, retry state). The dashboard lists it once, in the Conflicted state, under no Policy.
- The controller logs every reconcile of a party Policy until the conflict is resolved. Fix it by aligning the members' policy annotations or giving them distinct `k8s.sustain.io/owner-name` values.

## Retention for ephemeral workloads

By default a `WorkloadRecommendation` is deleted as soon as no Policy governs its identity. Ephemeral workloads need a different rule, so the sweep distinguishes two cases:

- **The identity is Departed** — no live member left: the objects were deleted (bare pods, TTL- or hook-deleted Job) or the only one is a Job in a terminal state. The recommendation is kept for the retention window (`--recommendation-retention` / Helm `controller.recommendationRetention`, default `168h`). The dashboard shows it as a *Departed* identity. Set `0` for immediate cleanup. A Conflicted identity's frozen recommendation is kept for the same window.
- **The identity still has live members but opted out** (annotation removed, policy no longer matching, namespace excluded) — it is swept on the next reconcile regardless of retention. Objects created in the last 10 minutes are never swept.

**Retention decides whether recurring ephemeral workloads are sized at admission.** If the object is deleted between two runs, the next run cold-starts on template resources. Retention must exceed the longest gap between runs of the same identity: the `168h` default covers a weekly job; a monthly job needs more.

**The clock starts late.** A retained identity keeps being recomputed, and `observedAt` keeps advancing, while its samples are still inside the query window. Retention only starts counting once they age out, so a departed recommendation lives roughly **`window + retention`** — about 14 days with a `168h` window and the default retention. Budget object count and webhook memory against that figure.

**Retained recommendations bypass the staleness check.** Each reconcile records a departed identity's decision with `status.departed: true`. The webhook serves a departed recommendation regardless of `observedAt` age — counted as `retained` rather than `hit` — until the retention window (checked against the webhook's own `--recommendation-retention`, rendered from the same Helm value) runs out; after that it counts as `stale`. The flag is cleared as soon as the identity has a live member again, and is only set by a reconcile whose inventory read succeeded, so a workload the controller is merely failing to refresh still trips the staleness check.

**Cost depends on whether the identity's name is stable.** The webhook's informer caches every retained object:

| Identity | Objects retained | Effect of raising retention |
| --- | --- | --- |
| CronJob-owned Jobs | One per CronJob | None — bounded |
| Bare pods, or standalone Jobs, carrying `k8s.sustain.io/owner-name` | One per annotation value | None — bounded |
| Standalone Jobs **without** that annotation | One per *run* (the Job's generated name) | Grows as runs-per-day × retention-days |

Annotate recurring Jobs with [`k8s.sustain.io/owner-name`](../guides/standalone-pods-and-grouping.md) before raising retention much past the default.

## Admission behaviour

The webhook makes no Prometheus calls, so every admission follows the same rule: it reads the identity's object and takes one **verdict** on it, from the object, the pod's resolved Policy and the current time alone. Each verdict increments `k8s_sustain_webhook_recommendation_source_total{source}`, the only place it is visible that a pod started on template resources:

| Verdict | `source` | When | Effect |
| --- | --- | --- | --- |
| Fresh | `hit` | Computed by the pod's Policy, `observedAt` within 30 minutes | Injected |
| Retained | `retained` | Computed by the pod's Policy for a departed or Conflicted identity, within the retention window | Injected |
| Withheld | `other-policy` | The object (`spec.policy`) or its values (`status.computedBy`) belong to another Policy: the other side of a [Conflicted identity](#conflicted-identities), or an identity [moving between Policies](#a-recommendation-is-served-only-to-its-policy) until the new one computes | Admitted unchanged |
| Stale | `stale` | `observedAt` older than 30 minutes, or than the retention window when retained. The controller has fallen behind (stuck reconcile, backlog, or Prometheus unreachable from the controller) | Admitted unchanged |
| Absent | `missing` | No object | Admitted unchanged; a stub is created. Expected once per new identity; a sustained rate for one identity means the controller never computes it |
| Undecided | `undecided` | The object exists but no pass has recorded anything yet | Admitted unchanged; no stub |
| No data | `nodata` | A pass recorded an outcome, but there is no Recommendation yet. Checked before staleness | Admitted unchanged |

A read that fails (apiserver, RBAC or cache problem) counts as `error` and admits the pod unchanged.

!!! warning "Keep `--reconcile-interval` well below 30 minutes"
    The 30-minute staleness window is fixed. The controller refreshes `observedAt` on each reconcile, so with `--reconcile-interval` at or above 30m every recommendation goes stale between reconciles and new pods start on template resources.

The webhook never sees pods in the release namespace, `kube-system`, `kube-public`, or any namespace in the Helm value `excludedNamespaces`: the `MutatingWebhookConfiguration`'s `namespaceSelector` excludes them, so no annotation there has any effect at admission.

## Observability

- `kubectl get wlrec -A` lists every cached recommendation.
- `kubectl describe wlrec deployment-web -n example` shows the full status for one workload.
- `status.observedAt` shows how fresh the data the webhook would serve is, and `status.computedBy` which Policy's pods it is served to.
- `status.trace` shows why a container has the values it has; the dashboard's workload detail page renders it as a table.

## RBAC

The controller and webhook share a ServiceAccount with cluster-wide access to `workloadrecommendations`; the dashboard has read-only access. See [Runtime security](../security.md#runtime-security).

Every k8s-sustain controller reconciles every `Policy` in the cluster, so install a single release per cluster.
