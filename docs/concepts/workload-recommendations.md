# Workload Recommendations (cache CRD)

`WorkloadRecommendation` is a namespaced custom resource that caches the most recent recommendation the controller computed for a single workload. The admission webhook never queries Prometheus itself, so this cache is the webhook's **only** source of recommendations at pod admission — not a fallback for outages, but the primary (and sole) path.

## Why it exists

The three Prometheus queries needed to build a recommendation (per-pod CPU percentile, per-pod memory percentile, OOM signal) only ever run on the controller's reconcile cadence (default every `--reconcile-interval`, 5m). The webhook has no Prometheus client and no independent way to compute a recommendation, so without this cache it would have nothing to inject on any pod CREATE and would always admit pods with whatever requests the workload's pod template specifies.

`WorkloadRecommendation` provides that shared, last-known-good value living in the cluster API, so:

- All webhook replicas see the same value (no per-replica drift, and no per-replica Prometheus load — the whole reason the webhook stopped querying Prometheus directly).
- Webhook restarts don't lose the last computed recommendation.
- Operators can inspect what the webhook would inject with `kubectl get wlrec`.

## How it works

1. **Write path (controller).** Each reconcile runs two phases before anything is applied. **Discovery** walks the policy's matched workloads and guarantees a `WorkloadRecommendation` exists for every matched identity — creating it when missing, and keeping its policy label and `status.observedResources` snapshot current — without issuing a single Prometheus query. **Computation** then works through the policy's `WorkloadRecommendation` objects and writes each one's recommendation back.

   Because discovery creates an object for *every* matched target rather than only for the ones that currently have enough history, the object count tracks **matched workloads**, not computable ones. On a fresh install expect one `WorkloadRecommendation` per matched workload immediately, most of them briefly empty.

   The object's name is `<lowercase-kind>-<workload-name>` in the workload's namespace (names exceeding the 253-char object-name limit are truncated with a short stable hash suffix; controller and webhook compute the name identically). The `status.containers` map carries the same per-container CPU/memory request and limit values that the recycle path applied. Writes are skipped when the new recommendation is byte-identical to the previous one **and** `observedAt` is less than 10 minutes old, so etcd write amplification scales with *change*, not workload count. The 10-minute refresh bumps `observedAt` on stable workloads so the cache never looks older than the 30-minute staleness window to the webhook.

   **The work-list is the `WorkloadRecommendation` list, not the target listing.** That is what lets an identity with no live workload object be refreshed at all: a completed Job, or a bare-pod group between two runs, appears in no listing, but it still has a cache object — so it is recomputed on the reconcile interval like everything else. Such an identity is computed and cached but never applied; there are no running pods to align, and the recommendation exists for the identity's *next* pod.

   The listing is not discarded, though: the work-list is the union of the cache objects and the identities discovery just ensured, because a `WorkloadRecommendation` created moments earlier in the *same* reconcile is often not yet visible to a list against the same cache-backed client — see [Computation](architecture.md#computation) for why, and for what that costs when discovery's own write failed.

   **Stub write path (webhook).** The webhook writes exactly one kind of object: an empty-status *stub*. It never computes or writes a recommendation — it has no Prometheus client — but when a pod is admitted for a workload identity that has no `WorkloadRecommendation` at all, it creates one with an empty `status` and records the admitted pod's container set on it. See [Cold start: stub recommendations](#cold-start-stub-recommendations) below.

   The [workload-age gate](recommendation-pipeline.md#stages) keys on the earliest of two signals: the workload object's `CreationTimestamp` and this cache object's own, which records when k8s-sustain first saw the identity. The second matters for the two ephemeral kinds — bare pods (`ownerKind: Pod`) and standalone Jobs (`ownerKind: Job`): every Job run re-creates the Job (always seconds old at reconcile), and a bare pod's identity has no workload object at all. A `WorkloadRecommendation` that has existed for ≥10 minutes therefore clears the gate for its identity even when the object behind it does not. Nothing ages the cache object out on a timer any more, so it keeps accumulating that age across runs — which is precisely what lets a recurring ephemeral identity clear the gate on a later run. Bare pods are gated exactly like every other kind; see [Cold start](#cold-start-stub-recommendations).

2. **Read path (webhook).** The webhook calls `fetchRecommendations` (`internal/webhook/recommendations.go`) on every admission that reaches this point — it never queries Prometheus and has no other source. If a `WorkloadRecommendation` exists and its `status.observedAt` is within the staleness window (default **30 minutes**, `DefaultCacheStaleness`), the webhook injects from it; otherwise it admits the pod with its original template resources unchanged. This staleness check now gates *every* injection, not just an emergency fallback.

3. **GC path (controller).** Cleanup runs through three independent strategies so a `WorkloadRecommendation` cannot outlive its reason for existing:

   - **Per-cycle sweep.** At the end of every reconcile, the controller lists `WorkloadRecommendation`s carrying the `k8s.sustain.io/policy: <policy>` label and deletes any whose target workload is no longer in the matched set (workload deleted, namespace excluded, annotation removed, kind disabled). Cluster-wide list with no server-side filter would scale O(WLR-count) with cluster size — the label scopes the list server-side and keeps the sweep cheap.
   - **Policy-deletion finalizer.** The controller adds the `k8s.sustain.io/cleanup` finalizer to every `Policy`. On `kubectl delete policy`, the finalizer blocks Policy removal until every owned `WorkloadRecommendation` is deleted. This is the only path that GUARANTEES cleanup because it runs synchronously with the user's delete operation.
   - **Orphan reaper.** A background goroutine scans every `WorkloadRecommendation` in the cluster on a tick (default 10 min) and deletes any whose `spec.policy` references a Policy that no longer exists. Catches WLRs orphaned by `kubectl delete policy --grace-period=0 --force` (which skips finalizers entirely), controller crashes mid-delete, and Policies renamed before the per-cycle sweep ran.

   Foreign-policy entries — those carrying a different policy label — are never touched by any of these strategies.

## Cold start: stub recommendations

Discovery creates a `WorkloadRecommendation` for every workload it **lists**. Some identities are never listed: a Job that runs for ninety seconds and is TTL-cleaned, or a bare-pod group that is up only between two reconciles, may simply not exist at the moment a reconcile fires. Stubs are how those identities get into the cache in the first place — and once an object exists, the ordinary computation phase takes over.

1. **The webhook creates one.** On an admission where no `WorkloadRecommendation` exists for the identity, the webhook creates one with `spec.workloadRef`, `spec.policy`, the `k8s.sustain.io/policy` label, the `k8s.sustain.io/stub: "true"` marker, and an **empty `status`** — then admits the pod unchanged. It also records the admitted pod's per-container requests and limits in `status.observedResources`. The whole write runs on a detached context in a goroutine: an `AdmissionResponse` never blocks on an apiserver write.

   That container snapshot is what makes the object self-sufficient. Computation reads an identity's container list from `status.observedResources`, because a departed identity has no workload object left to read a pod template from — and for an identity that never appears in a listing, the webhook is the only component that ever sees its containers. The snapshot is written on the `AlreadyExists` path too, so an object that somehow lacks one can still be filled in by a later admission; where discovery has already written a snapshot from the workload's pod template, that one wins and the webhook leaves it alone.

   The `k8s.sustain.io/stub` marker is **provenance, not control flow** — nothing branches on it. Both writers produce the same shape: the controller's own write path must `Create` before it can patch status (a status subresource discards status supplied at create), so every controller-written recommendation is *transiently* empty-status too, and "empty status" alone cannot tell the two apart. The label survives because knowing which component first saw an identity is worth a `kubectl get wlrec -l k8s.sustain.io/stub` when an ephemeral workload is not being sized.

   The create is idempotent — an `AlreadyExists` is absorbed silently — and it is a `Create` and never an `Update`, so a stub can never clobber a populated recommendation.

   Idempotency alone is not enough to bound the write volume, though. A stub only becomes visible to the webhook's informer after the create *and* watch propagation, so inside that window every pod of a scaling workload still reads `missing` and fires its own create: a 500-replica scale-out issues 500 concurrent creates of one object name, 499 rejected. That is apiserver traffic driven by pod churn — the same coupling removing Prometheus from admission was meant to eliminate — and it is worst during an outage, when the stub's status stays empty and every admission keeps classifying as `missing`. So requests are **deduplicated per identity** for 30s and **bounded to 16 concurrent** writes. Over-capacity requests queue rather than being dropped: a run-once Job has no later admission to retry on, so discarding its request would leave it permanently unsized.

   Only the *missing* case creates a stub. When a `WorkloadRecommendation` exists but is stale, the object is already there and a create would be a guaranteed no-op — a wasted apiserver write on every admission, at exactly the moment the cluster is least able to absorb it.

2. **The computation phase fills it in.** From the moment the object exists, the identity is in the controller's work-list. Every reconcile recomputes it alongside every other `WorkloadRecommendation` the policy owns, through the same shared pipeline — so a workload's numbers never depend on which component created the object. There is no separate cold-start reconciler; a stub is simply a `WorkloadRecommendation` that has not been computed yet.

3. **`nodata` means "nothing computed yet", and is retried.** If the identity produces nothing — too young for the [workload-age gate](recommendation-pipeline.md#stages), or no usable samples yet — computation sets `status.source: nodata` and stamps `status.observedAt`.

   This is **not** a terminal state. The next reconcile recomputes the identity like any other, so a new identity converges within **one reconcile interval** of Prometheus having enough history for it. Nothing has to be deleted, reaped or recreated to force a fresh attempt, and no admission is needed to re-trigger one.

   `nodata` is never written over a populated recommendation. An identity that already has `status.containers` keeps them even when a later recompute finds nothing — which is exactly what preserves a departed identity's last-known-good once its samples age out of the query window. `observedAt` is deliberately left alone in that case: it is the webhook's freshness signal, and bumping it would claim data that is no longer there.

**What this means per kind.** Because every cache object is recomputed on the interval whether or not its workload is alive, convergence no longer depends on a reconcile catching the workload in the act. A standalone Job that always finishes between two reconciles converges anyway: its cache object persists between runs, ages past the workload-age gate, and Prometheus history accumulates against the one identity. A recurring bare-pod group behaves the same way. Both depend on the object surviving the gap between runs, which is what [`--recommendation-retention`](#retention-for-ephemeral-workloads) governs.

The identity that still does not converge is one whose **name changes on every run** — a timestamp or hash suffix makes each run a brand-new identity with no history to accumulate against. Give it a stable name, run it under a CronJob, or collapse the runs onto one identity with the [`k8s.sustain.io/owner-name` annotation](../guides/standalone-pods-and-grouping.md).

No recommendation is ever aged out by its *status*. The orphan reaper deletes only objects whose owning Policy is gone, and the per-policy sweep deletes only objects whose workload has left the target set — subject to the retention window when the workload object itself is gone. Staleness of a recommendation is the webhook's concern (it stops injecting past the 30-minute window), not the reaper's.

**What this means in practice:** the first pod of a never-before-seen workload identity still starts on template resources. That is unavoidable — admission cannot wait for a Prometheus query it no longer makes. The stub is for the *next* pod. To confirm cold start is converging, watch `k8s_sustain_webhook_recommendation_source_total` turn from `missing` to `hit` for that identity — that works for every kind. `k8s_sustain_wlr_refresh_total` additionally shows the controller-side outcome, but only for cycles in which the identity is *departed*: an ephemeral pod that is alive when the reconcile fires is a live target and increments nothing. See [Metrics](../reference/metrics.md#k8s_sustain_wlr_refresh_total).

## Schema

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: WorkloadRecommendation
metadata:
  name: deployment-web              # <kind>-<name>
  namespace: example
spec:
  policy: production-rightsizing    # owning policy
  workloadRef:
    kind: Deployment
    namespace: example
    name: web
status:
  observedAt: 2026-05-01T12:34:56Z  # webhook trusts ≤30m old, unless departed
  source: prometheus                # "prometheus" (computed), "nodata" (a departed identity
                                    # produced nothing; retried), or unset — a live identity
                                    # that has not produced anything yet is left untouched
  departed: false                   # true = retained for a workload confirmed gone;
                                    # exempt from the staleness gate, see below
  containers:
    app:
      cpuRequest: 250m
      memoryRequest: 256Mi
      cpuLimit: 500m
      memoryLimit: 512Mi
      removeCpuLimit: false      # true when the policy says NoLimit
      removeMemoryLimit: false
```

The `removeCpuLimit` / `removeMemoryLimit` flags carry the explicit "strip the limit" intent (Policy `NoLimit`). They are needed because nil `cpuLimit`/`memoryLimit` alone cannot distinguish "leave alone" (`KeepLimit` / no strategy) from "remove". The webhook reads these from the cache on every admission — it has no other source — so `NoLimit` policies keep stripping limits consistently.

The resource is namespaced and inherits namespace deletion semantics: removing the namespace removes every recommendation in it.

## Who writes recommendations

The controller is the only writer of *recommendations*. On every reconcile cadence it recomputes each `WorkloadRecommendation` its policy owns — for `Ongoing` and `OnCreate` targets alike, and for departed identities with no workload object left — including the cold-start ones the webhook created.

The webhook writes only empty-status **stubs**, plus the admitted pod's container snapshot on them — a request for a recommendation, never a recommendation itself. It has no Prometheus client and computes nothing. See [Cold start: stub recommendations](#cold-start-stub-recommendations).

## Retention for ephemeral workloads

By default a `WorkloadRecommendation` is garbage-collected as soon as its workload leaves the policy's target set — that's the per-cycle sweep described above. Ephemeral workloads need a different rule: a standalone pod that completed, a Job removed by `ttlSecondsAfterFinished` or an Argo CD hook deletion policy, or a Job that reached `Complete`/`Failed` all leave the target set the moment they finish, but their **object** may still exist (or may already be gone) with no further reconcile ever touching them again.

The sweep tells the two cases apart:

- **The workload object itself is gone** (deleted standalone pod, TTL/hook-deleted Job) **or is a Job in a terminal state** — the controller keeps the recommendation for the retention window (`--recommendation-retention` / `controller.recommendationRetention`, default `168h`) instead of deleting it immediately. The dashboard shows these as *inactive* workloads with a last-seen timestamp derived from `status.observedAt`. Set the retention to `0` to restore immediate cleanup.
- **The workload object still exists but opted out** (annotation removed, policy no longer matching, namespace excluded) — it is swept on the next reconcile regardless of the retention setting; the retention window only protects workloads whose object is actually gone. The one exception is a workload object younger than the freshness grace below, which is absent from the target list because it postdates it rather than because it opted out.

**Retention is not only about the dashboard.** Since the webhook reads recommendations exclusively from these objects and never queries Prometheus, this window also decides whether a *recurring* ephemeral identity is rightsized **at admission** on its next run. Reap the object between two runs and every run cold starts: the pod is admitted on its template resources, and the recommendation is only [computed](#cold-start-stub-recommendations) afterwards — too late for a task that finishes in seconds. So the rule is: **retention must exceed the longest expected gap between runs of the same identity.** The `168h` default clears a weekly batch cycle; a monthly job needs more.

What retention does **not** control is how often anything is recomputed. A retained identity is recomputed on the reconcile interval like every other cache object; the window governs only how long its last-known-good keeps being served once the workload object is gone.

**The clock starts later than you might expect.** Retention is measured from `status.observedAt`, and the computation phase keeps refreshing that for as long as the identity's samples are still inside the query window — a departed identity is not silent, it is simply computing from history. Only once its samples age out does `observedAt` freeze and the retention window begin to run. The effective lifetime of a departed recommendation is therefore roughly **`window + retention`**: about 14 days at the `168h` CPU window and `168h` retention defaults, not 7. Size the *gap between runs* against retention as described above — that guidance is unaffected — but budget object count and webhook informer memory against the longer figure.

**How a retained recommendation survives the freshness gate.** The webhook normally refuses to inject a recommendation whose `observedAt` is older than 30 minutes, because that means the controller has fallen behind. A departed identity *is* still recomputed every reconcile interval, but once its samples age out of the query window the recompute finds nothing and deliberately writes nothing — keeping the last-known-good rather than overwriting it with an empty status, and leaving `observedAt` frozen at the last successful write. From then on it would always fail that test. Read naively, a nightly Job would be admitted on template resources on every run but its first.

So the sweep records what it has already determined. When it confirms a workload is gone and keeps its recommendation, it sets `status.departed: true`, and the webhook serves a departed recommendation without applying the staleness budget — counting it as [`retained`](../reference/metrics.md#k8s_sustain_webhook_recommendation_source_total) rather than `hit`, since the data is last-known-good rather than fresh.

The waiver is bounded by the retention window itself, checked by the webhook against its own `--recommendation-retention` (the chart renders it from the same `controller.recommendationRetention` value, so the two cannot drift). Relying on the sweep to have deleted a lapsed object instead would leave the waiver unbounded whenever the controller stops sweeping: both the sweep and the clearing of `departed` live inside the reconcile, which returns early before either when the workload listing fails — RBAC revoked on one kind, an unreachable API group, a removed CRD. The flag set then freezes, and an unbounded waiver would keep injecting that identity's last-known-good at any age, including after the workload came back. Past the window the read reports [`stale`](../reference/metrics.md#k8s_sustain_webhook_recommendation_source_total) instead, which is exactly what the situation is.

This does not weaken the staleness signal. The flag is set only on a *positively confirmed* absence — never when the existence check merely errors — and a workload the controller is failing to refresh stays in the target set, so it is never marked and still trips the gate. The flag is cleared the moment the identity is seen again. In other words, `departed` distinguishes "this identity has no fresh data left to refresh from" from "something should have refreshed this and didn't" — the same distinction the `nodata` state makes.

Long-lived kinds are unaffected either way — a Deployment's object never disappears, so its recommendation is never a retention candidate.

**What a longer window costs depends on whether the identity's name is stable.** Retained objects are cached in full by the webhook's informer, so their count is webhook memory:

| Identity | Objects retained | Effect of raising retention |
| --- | --- | --- |
| CronJob-owned Jobs | One per CronJob (the Job itself is skipped in favour of its CronJob) | None — bounded |
| Bare pods, or standalone Jobs, carrying `k8s.sustain.io/owner-name` | One per annotation value | None — bounded |
| Standalone Jobs **without** that annotation | One per *run* (`IdentityName` is the Job's own generated name) | Grows as runs-per-day × retention-days |

That last row is the only one that scales, and it is the case the [`k8s.sustain.io/owner-name` annotation](../guides/standalone-pods-and-grouping.md) exists to collapse. Annotating recurring Jobs makes retention free *and* gives the identity continuous history across runs — worth doing before raising the window much past the default.

Independently of the retention setting, `WorkloadRecommendation`s **created** within the last 10 minutes are never swept, and neither is one whose workload **object** was created in that window. This freshness grace protects an identity first written just after a reconcile pass built its target list — without it, that pass's own sweep would delete the record seconds later, because the workload legitimately isn't in the list yet.

The grace deliberately keys off creation timestamps rather than `status.observedAt`. Under [WLR-driven computation](#how-it-works) the controller recomputes every object in its own list each cycle, departed identities included, so `observedAt` means "this controller recently ran a query" — not "something recently observed this workload alive". Keyed off `observedAt`, the guard became self-satisfying: the computation phase refreshed the timestamp and the sweep at the end of the same pass read it back as proof of freshness, so a workload that opted out kept its recommendation, and its share of the Prometheus query load, indefinitely. A creation timestamp cannot be refreshed by the writer it is meant to be independent of.

`status.observedResources` complements retention: it's a per-container snapshot of the requests/limits the workload actually ran with, captured at write time, so current-vs-recommended stays visible on the dashboard after the pod spec itself is gone.

Bare-pod identities (`ownerKind: Pod`) can't be existence-checked — the workload reference's name is the `k8s.sustain.io/owner-name` value (or the pod name), not a real Kubernetes object name — so they always resolve as "gone" and ride out the full retention window rather than being swept as an opt-out.

## Admission behaviour

The webhook makes no Prometheus calls at all, so there is no "outage" case distinct from the normal path — every admission follows the same rule:

- **Cache fresh (`observedAt` within 30m).** Webhook injects the cached recommendation. Counted as `hit`.
- **Cache stale (`observedAt` older than 30m).** Webhook admits the pod with its original template resources unchanged. Counted as `stale` — the controller has fallen behind (stuck reconcile loop, backlog, or a Prometheus outage on the controller side). No stub is created; the object already exists.
- **Cache missing entirely.** Webhook admits the pod unchanged *and* creates a stub, which puts the identity into the controller's work-list so a recommendation is computed for the next pod of this workload. Counted as `missing`. Expected transiently for any brand-new workload; a *sustained* rate for the same identity means the controller is never computing it. For an ephemeral identity, cross-check `k8s_sustain_wlr_refresh_total` for that namespace and owner kind.
- **`nodata`.** A recommendation object exists and was evaluated, but the identity produced nothing recommendable yet. Webhook admits the pod unchanged and counts it as `nodata`. No stub is created — the object is already there, and it is already being recomputed every reconcile interval, so a create could only return `AlreadyExists`.
- **Read failed.** Webhook admits the pod unchanged. Counted as `error` — an apiserver/RBAC/cache problem rather than an unreconciled workload.

Each outcome increments `k8s_sustain_webhook_recommendation_source_total{source}`. Nothing in a pod's own spec reveals that it started on template resources because the pipeline was unhealthy, so this counter is the only place that fact is visible.

Because the controller is the only thing that ever talks to Prometheus, a Prometheus outage now shows up as the cache aging past 30 minutes rather than as an immediate webhook-side failure — the practical effect on new pods is the same (template resources until the cache catches up), but it happens on the controller's clock, not the webhook's.

## Observability

- `kubectl get wlrec -A` lists every cached recommendation.
- `kubectl describe wlrec deployment-web -n example` shows the full status for one workload.
- The status `observedAt` field tells operators the freshness of the data the webhook would serve right now.

## Tuning

The staleness window is set at `webhook.Handler.CacheStaleness` (default `DefaultCacheStaleness = 30m`). Future versions may expose this via Helm; today it requires a binary build.

## RBAC

The controller's ClusterRole grants `get;list;watch;create;update;patch;delete` on `workloadrecommendations` (and its `/status` subresource). The webhook reuses the controller ServiceAccount and needs a subset in practice: `list`/`watch`, because it serves admission reads from an informer cache rather than a per-pod apiserver `Get`; `create`, for stub objects; and `get` plus a `/status` patch, to record the admitted pod's container set on a stub that has no snapshot yet. It never writes a recommendation and never deletes an object. The dashboard ClusterRole (when enabled) has read-only access on the resource so operators can browse it from the UI.

### Why the grant is cluster-wide

`WorkloadRecommendation` is a namespaced resource and a Policy can target every namespace in the cluster (`spec.selector.namespaces: []` matches all). The controller therefore needs cluster-wide write access to create/update/delete WLRs anywhere a matched workload lives. The webhook needs cluster-wide reads to look up a cached entry for any pod that lands at admission, regardless of namespace.

Kubernetes RBAC has no native concept of "label-scoped reads" — a `Role` is bound to a single namespace, and a `ClusterRole` cannot be filtered by labels at evaluation time. So even though every WLR carries the `k8s.sustain.io/policy: <name>` label (used to scope the controller's *list* calls server-side), the **RBAC grant itself** must remain unconstrained.

### Hardening options

The grant is unavoidable, but you can layer additional controls:

- **Restrict the namespaces a Policy can target.** Operators provision the Policy CRDs; users don't. Treat Policy authorship as an admin operation in your manifests pipeline rather than letting any namespace owner author one.
- **NetworkPolicy on the webhook.** Limit ingress to the apiserver Service IP only — prevents anyone in the cluster from invoking `/mutate` directly.
- **Audit policy.** Log every k8s API access from the controller / webhook ServiceAccount for an audit trail of WLR mutations. Useful when investigating whether a stale recommendation came from a legitimate reconcile or out-of-band tampering.
- **Put an authenticating proxy in front of the dashboard.** The dashboard is enabled by default (`dashboard.enabled: true`) and ships with **no built-in authentication** — it serves read-only WLR data to anyone who can reach it. It listens on a `ClusterIP` Service, so it is not exposed outside the cluster until you add an Ingress/Gateway. When you do expose it, never expose it directly: front it with an identity-aware proxy such as **Cloudflare Access**, `oauth2-proxy`, or an authenticating Ingress (OIDC/SSO, mTLS). The dashboard runs under its own narrower read-only ClusterRole, but the proxy is what gates *who* can read it. If you have no need to browse recommendations, disabling it (`dashboard.enabled: false`) removes the attack surface entirely.

If your environment requires cell-level isolation between namespaces, the cleanest answer today is **one Helm release per cell** with `selector.namespaces` set so each release manages only its own slice, and `installCRDs=false` on all but one. The CRD is shared cluster-wide, but each controller's WLR mutations stay within its declared namespaces.
