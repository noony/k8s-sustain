# Workload Recommendations (cache CRD)

`WorkloadRecommendation` (`kubectl get wlrec`) is a namespaced custom resource that caches the most recent recommendation the controller computed for one workload identity. The admission webhook never queries Prometheus, so this cache is its **only** source of recommendations at pod admission.

## Why it exists

Only the controller talks to Prometheus, on its reconcile cadence (`--reconcile-interval`, default 5m). The webhook has no Prometheus client, so without a shared cache it would have nothing to inject. Keeping the last-known-good value in the cluster API means:

- All webhook replicas see the same value, and add no Prometheus load.
- Webhook restarts don't lose the last computed recommendation.
- Operators can inspect what the webhook would inject with `kubectl get wlrec`.

## How it works

1. **Write path (controller).** Each reconcile, **discovery** ensures a `WorkloadRecommendation` exists for every matched identity (no Prometheus queries), then **computation** recomputes every object the policy owns and writes its recommendation back — see [Computation](architecture.md#computation). Expect one object per matched workload from the first reconcile, most of them briefly empty.

    The object's name is `<lowercase-kind>-<workload-name>` in the workload's namespace (names over 253 characters are truncated with a stable hash suffix). A write is skipped when the recommendation is unchanged **and** `observedAt` is less than 10 minutes old, so etcd writes scale with change rather than workload count, while `observedAt` on stable workloads never approaches the webhook's 30-minute staleness window.

2. **Stub write path (webhook).** When a pod is admitted for an identity with no `WorkloadRecommendation`, the webhook creates an empty-status stub. See [Cold start](#cold-start-stub-recommendations).

3. **Read path (webhook).** Every admission reads the cache; see [Admission behaviour](#admission-behaviour).

4. **Garbage collection (controller).** Three independent mechanisms:

    - **Per-cycle sweep** — at the end of each reconcile, deletes objects carrying the policy's label whose workload is no longer matched (workload deleted, namespace excluded, annotation removed, kind disabled), subject to [retention](#retention-for-ephemeral-workloads).
    - **Policy-deletion finalizer** — `k8s.sustain.io/cleanup` on every `Policy` blocks its deletion until every owned `WorkloadRecommendation` is deleted.
    - **Orphan reaper** — every 10 minutes, deletes objects whose `spec.policy` names a Policy that no longer exists (e.g. after `kubectl delete policy --force --grace-period=0`, which skips finalizers).

    Objects belonging to another policy are never touched. Deleting a namespace deletes its recommendations.

## Cold start: stub recommendations

Discovery only creates objects for workloads it **lists**. A Job that runs for ninety seconds, or a bare-pod group that is up only between two reconciles, may never be listed. Stubs get those identities into the cache:

1. **The webhook creates a stub.** On an admission with no `WorkloadRecommendation` for the identity, the webhook creates one with `spec.workloadRef`, `spec.policy`, the `k8s.sustain.io/policy` label, the `k8s.sustain.io/stub: "true"` label and an empty `status`, records the pod's per-container requests and limits in `status.observedResources`, and admits the pod unchanged. The write runs asynchronously, never blocks the admission, and is a `Create` (never an `Update`), so it cannot clobber a populated recommendation. The container snapshot lets computation work on an identity that has no workload object left to read a template from.

2. **Computation fills it in.** From then on the identity is in the controller's work-list and is recomputed every reconcile through the same pipeline as every other object.

3. **`nodata` is retried.** If the identity produces nothing — too young for the [workload-age gate](recommendation-pipeline.md#stages), or no usable samples yet — computation sets `status.source: nodata`. That is not terminal: the next reconcile recomputes it, so a new identity converges within one reconcile interval of Prometheus having enough history. `nodata` never overwrites an existing recommendation.

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
  policy: production-rightsizing    # owning policy
  workloadRef:
    kind: Deployment
    namespace: example
    name: web
status:
  observedAt: 2026-05-01T12:34:56Z  # webhook trusts ≤30m old, unless departed
  source: prometheus                # "prometheus" (computed), "nodata" (nothing to
                                    # recommend yet; retried), or unset (not yet computed)
  departed: false                   # true = retained for a workload confirmed gone;
                                    # exempt from the staleness check, see below
  containers:
    app:
      cpuRequest: 250m
      memoryRequest: 256Mi
      cpuLimit: 500m
      memoryLimit: 512Mi
      removeCpuLimit: false         # true when the policy says NoLimit
      removeMemoryLimit: false
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

`observedResources` is written by discovery from the pod template (or by the webhook from the admitted pod). It keeps current-vs-recommended visible on the dashboard after the workload is gone, and supplies the container list for identities with no workload object.

## Retention for ephemeral workloads

By default a `WorkloadRecommendation` is deleted as soon as its workload leaves the policy's target set. Ephemeral workloads need a different rule, so the sweep distinguishes two cases:

- **The workload object is gone** (deleted pod, TTL- or hook-deleted Job) **or is a Job in a terminal state** — the recommendation is kept for the retention window (`--recommendation-retention` / Helm `controller.recommendationRetention`, default `168h`). The dashboard shows it as an *inactive* workload. Set `0` for immediate cleanup.
- **The workload object still exists but opted out** (annotation removed, policy no longer matching, namespace excluded) — it is swept on the next reconcile regardless of retention. Objects, and workloads, created in the last 10 minutes are never swept.

Bare-pod identities (`ownerKind: Pod`) have no object to check, so they always count as gone and ride out the full retention window.

**Retention decides whether recurring ephemeral workloads are sized at admission.** If the object is deleted between two runs, the next run cold-starts on template resources. Retention must exceed the longest gap between runs of the same identity: the `168h` default covers a weekly job; a monthly job needs more.

**The clock starts late.** A retained identity keeps being recomputed, and `observedAt` keeps advancing, while its samples are still inside the query window. Retention only starts counting once they age out, so a departed recommendation lives roughly **`window + retention`** — about 14 days with a `168h` window and the default retention. Budget object count and webhook memory against that figure.

**Retained recommendations bypass the staleness check.** When the sweep confirms a workload is gone and keeps its recommendation, it sets `status.departed: true`. The webhook serves a departed recommendation regardless of `observedAt` age — counted as `retained` rather than `hit` — until the retention window (checked against the webhook's own `--recommendation-retention`, rendered from the same Helm value) runs out; after that it counts as `stale`. The flag is cleared as soon as the workload is seen again, and is only set on a confirmed absence, so a workload the controller is merely failing to refresh still trips the staleness check.

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
- **`retained`** — a departed recommendation within the retention window: injected.
- **`stale`** — `observedAt` older than 30 minutes: the pod is admitted with its template resources. The controller has fallen behind (stuck reconcile, backlog, or Prometheus unreachable from the controller).
- **`missing`** — no object: the pod is admitted unchanged and a stub is created. Expected once per new identity; a sustained rate for one identity means the controller never computes it.
- **`nodata`** — the object exists but nothing is recommendable yet: admitted unchanged.
- **`error`** — the read failed (apiserver, RBAC or cache problem): admitted unchanged.

!!! warning "Keep `--reconcile-interval` well below 30 minutes"
    The 30-minute staleness window is fixed. The controller refreshes `observedAt` on each reconcile, so with `--reconcile-interval` at or above 30m every recommendation goes stale between reconciles and new pods start on template resources.

The webhook never sees pods in the release namespace, `kube-system`, `kube-public`, or any namespace in the Helm value `excludedNamespaces`: the `MutatingWebhookConfiguration`'s `namespaceSelector` excludes them, so no annotation there has any effect at admission.

## Observability

- `kubectl get wlrec -A` lists every cached recommendation.
- `kubectl describe wlrec deployment-web -n example` shows the full status for one workload.
- `status.observedAt` shows how fresh the data the webhook would serve is.

## RBAC

The controller and webhook share a ServiceAccount with cluster-wide access to `workloadrecommendations`; the dashboard has read-only access. See [Runtime security](../security.md#runtime-security).

Every k8s-sustain controller reconciles every `Policy` in the cluster, so install a single release per cluster.
