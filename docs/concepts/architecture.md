# Architecture

k8s-sustain is split into three independent components that run as separate processes (different container args in the same image):

```text
┌─────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                       │
│                                                                 │
│  ┌──────────────────┐        ┌──────────────────────────────┐   │
│  │  k8s-sustain     │        │  k8s-sustain-webhook         │   │
│  │  (controller)    │        │  (admission server)          │   │
│  │                  │        │                              │   │
│  │  Watches Policy  │        │  Intercepts Pod CREATE       │   │
│  │  objects and     │        │  requests, injects           │   │
│  │  reconciles      │        │  resources from OnCreate     │   │
│  │  Ongoing-mode    │        │  policies                    │   │
│  │  workloads       │        │                              │   │
│  └────────┬─────────┘        └──────────────┬───────────────┘   │
│           │                                 │                   │
│           │ list / patch                    │ Get Policy        │
│           │                                 │ Get Job/RS        │
│           │                                 │ Watch WLR (cache) │
│           │                                 │ Create stub WLR   │
│           ▼                                 ▼                   │
│  ┌────────────────────────────────────────────────────────┐     │
│  │                   Kubernetes API Server                │     │
│  └─────────────────────────┬──────────────────────────────┘     │
│                            │                                    │
│           ┌────────────────┼────────────────┐                   │
│           ▼                ▼                ▼                   │
│    Deployments      StatefulSets        CronJobs                │
│    DaemonSets       Argo Rollouts                               │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                        Prometheus                        │   │
│  │  k8s_sustain:container_cpu_usage_by_workload:rate1m      │   │
│  │  k8s_sustain:container_memory_by_workload:bytes          │   │
│  └────────────────────────────┬─────────────────────────────┘   │
│                               │                                 │
│  ┌────────────────────────────┴─────────────────────────────┐   │
│  │  k8s-sustain-dashboard (optional)                        │   │
│  │  Web UI: policy exploration, metrics, simulator          │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

## Controller (`k8s-sustain start`)

The controller is a standard [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) reconciler that watches `Policy` objects.

**Reconcile loop:** a `Policy` event (create / update / periodic requeue) runs three phases in order — **discovery**, **computation**, **application**. Only computation talks to Prometheus. Independent `Policy` objects reconcile in parallel, bounded by `--policy-concurrency-limit` (default 10) — a separate knob from the per-workload fan-out inside the computation phase.

The controller requeues after `--reconcile-interval` (default `5m`).

### Discovery

For each workload kind enabled in the policy (`deployment`, `statefulSet`, `daemonSet`, `argoRollout`, `cronJob`, `job`, `pod`):

- List all objects of that kind — scoped to the namespaces in `selector.namespaces` when specified, or cluster-wide otherwise
- Resolve the `k8s.sustain.io/policy` annotation via `internal/policymatch.ResolvePolicy`, most-specific-first: the pod template (or, for bare pods, the pod's own annotations, since there is no template to read it from), then the workload object's own `metadata.annotations`, then the Namespace's. See the [Annotation reference](../reference/annotation.md) for the full precedence and the opt-out escape hatch.
- Ensure a `WorkloadRecommendation` exists for the matched identity, creating it when missing and keeping its policy label and `status.observedResources` snapshot current

Discovery issues **no Prometheus queries**, and it creates a cache object for *every* matched target rather than only for the ones that currently have enough history to compute. The object count therefore tracks matched workloads, not computable ones — expect a `WorkloadRecommendation` per matched workload from the first reconcile onward, most of them briefly empty.

Several targets collapse onto one identity under [owner-name grouping](../guides/standalone-pods-and-grouping.md) (`app-blue` and `app-green` both reporting as `Deployment/app`). They share a single `WorkloadRecommendation` and a single computation, but every member is carried into the application phase, which must reach each one's own pods.

### Computation

The work-list is **the policy's `WorkloadRecommendation` objects, not the target listing**. That inversion is the point: an identity that appears in no listing — a completed Job, a bare-pod group between runs — is still recomputed on every cycle. When the work-list came from the listing, nothing could refresh those identities at all, and their recommendation stayed frozen until the retention window lapsed and the object was swept.

The listing is not discarded, though: the work-list is the union of the cache objects and the identities discovery just ensured. Both the controller's reads go through the same watch-populated informer cache, so an object created moments earlier in the *same* reconcile is often not visible to a list yet — and nothing watches `WorkloadRecommendation` to re-trigger a reconcile when it becomes visible. Without the union a freshly matched workload would be neither computed nor applied on the cycle it is first seen and would wait a full `--reconcile-interval` for a second chance. Discovery already holds everything such an identity needs (its container set comes from the target), so this costs no extra API calls.

- Prefetch every identity's Prometheus inputs in **one sharded batch call per policy** (see [Recommendation Pipeline](recommendation-pipeline.md#how-the-data-is-fetched)) — a handful of queries for the whole policy instead of several per workload. Every kind batches identically, including `Job` and `Pod`
- Skip identities in retry backoff from a previous transient failure. Under owner-name grouping an identity is withheld only when *every* member is backed off, so one sick member cannot deny its healthy siblings their inputs
- Process the work-list in parallel (bounded by `--workload-concurrency-limit`, default 5): detect autoscalers (HPA / KEDA `ScaledObject`) targeting the workload — read-only, no patches — and compute a per-container recommendation (see [Recommendation Pipeline](recommendation-pipeline.md))
- The unit is the **identity**, not the workload object: a group of workloads sharing an owner-name produces exactly one computation and one write, against the union of the members' containers. Computing per member instead gave a single shared object several competing answers, with the one that survived decided by whichever member's goroutine finished last
- Write the result back to the `WorkloadRecommendation`, regardless of update mode — this keeps `OnCreate` workloads visible on the dashboard and is the webhook's only source of recommendations at admission (it never queries Prometheus itself)
- An identity that produces nothing recommendable this cycle is simply not written. For a **live** target the status is left as it stands — an identity that has never produced anything keeps an empty `status.containers` and an unset `status.source`. Either way it is recomputed on the next cycle, and converges as soon as Prometheus has enough history; nothing here is a terminal state
- A **departed** identity that produces nothing is additionally marked `status.source: nodata`, recording that the controller looked and found nothing. The mark never overwrites an existing recommendation, so a departed identity whose samples have aged out of the query window keeps its last-known-good
- A **departed** identity — one with a cache object but no live workload object behind it — stops here. It is computed and cached so the webhook can inject it into that identity's *next* pod, but there are no running pods to align

Departed refreshes are counted in `k8s_sustain_wlr_refresh_total{namespace, owner_kind, outcome}`, the only signal that reports on a population no listing contains; see [Metrics](../reference/metrics.md#k8s_sustain_wlr_refresh_total).

### Application

Only live targets in `Ongoing` mode are applied. `OnCreate`-mode workloads stop after computation — the recommendation is cached but never applied by the controller, because resource injection at pod creation is the webhook's job — and a departed identity has no pods to apply anything to, so it stopped in the previous phase.

- If `--recommend-only` is set, log the recommendation and skip patching
- Recycle stale running pods: on k8s >= 1.33 via in-place resource patching through the `/resize` subresource; on k8s < 1.33 via the Eviction API (PDB-respecting). The webhook injects the latest resources into replacement pods at creation time. Pods are listed by the workload's label selector and then filtered by **controller ownership**: a pod is only recycled when its ownerRef chain resolves to the target workload (directly for StatefulSet/DaemonSet, via the owning ReplicaSet for Deployment/Argo Rollout). Bare pods or pods of another workload with an overlapping selector are skipped and logged — the opt-in contract is per-workload
- **CronJob and Job exception:** the controller never mutates the CronJob or Job spec and never evicts a job pod. On clusters that support in-place resize, currently-running job pods are resized via `pods/resize`; otherwise they finish on their existing resources and, for a CronJob, the next scheduled run picks up the new values from the webhook
- **Bare-pod exception:** a `Pod`-kind identity is never evicted in any mode — no controller would recreate the pod — but under `Ongoing` its running pods *are* resized in place, since an in-place resize needs no controller behind it. Membership comes from the same grouping rule the controller discovers them with, so a ReplicaSet-owned pod that merely carries the mirrored `owner-name` label is never touched
- Under owner-name grouping, every member is applied independently against the identity's one shared recommendation, narrowed to the containers that member actually declares
- Emit a `ResourcesUpdated` event on the workload object on success
- On transient failure (Prometheus timeout, API 5xx), schedule retry with exponential backoff (30s base, 5min cap) and emit a `ReconciliationRetryScheduled` warning event on the workload

`--policy-concurrency-limit` × `--workload-concurrency-limit` bounds how many workloads are in flight at once (50 by default), each issuing several Prometheus queries — enough to burst well past Prometheus's own `--query.max-concurrency` (default 20, shared with every other consumer of that server: Grafana dashboards, alerting rules). `--prometheus-max-inflight` (default 8) is a separate, global cap on concurrent Prometheus queries across the whole controller process, independent of the two concurrency-limit flags above — it throttles the client itself rather than the workload fan-out, so it protects Prometheus even if the concurrency limits are turned up.

### Pod OOM watcher

A second controller-runtime reconciler runs alongside the Policy reconciler and watches `Pod` objects cluster-wide, filtered by a local event predicate to those carrying the `k8s.sustain.io/policy` annotation OR a fresh `OOMKilled` container status. It exists to close the multi-minute latency window between an OOM kill and the next recording-rule-driven reconcile (kube-state-metrics scrape → Prometheus scrape → 1m rule evaluation → 5–10m reconcile interval).

When a container's `LastTerminationState.Terminated.Reason == "OOMKilled"` is observed, the watcher first checks whether this exact termination (pod UID + container + restart count + terminated-at, all local — no apiserver call) has already been resolved on an earlier pass; if so it stops right there. Otherwise it resolves the [multi-level annotation](../reference/annotation.md) the pod's workload opts in through — pod template, owning workload metadata, or Namespace, same `ResolvePolicy` every other reader uses — and, if the pod is managed by some policy:

- Resolves the pod's top-level workload owner and upserts an entry in an in-memory cache keyed by `(namespace, ownerKind, ownerName, container)`. Restart-count and termination timestamp are used to dedup repeated observations of the same kill. That key names a workload, not a pod, so every pod of a workload writes to the same slot — and pods are reconciled in parallel. The slot therefore keeps the *newest* observation, ordered by `(terminated-at, restart-count)`, rather than whichever write happened to land last: otherwise a slower goroutine carrying an older kill (with a smaller pre-bump memory limit) could overwrite a newer one and under-bump the OOM floor for that cycle. Restart count only breaks ties on an equal timestamp, which `metav1.Time`'s one-second resolution makes possible for two kills of one container. An out-of-order observation still counts as a new kill for the purpose of triggering an immediate reconcile — it is a real kill nothing has seen yet — it just does not displace the newer entry.
- Captures the memory limit the kubelet had actually applied to the container (`ContainerStatus.Resources`, falling back to the pod spec when the status carries none), so the recommender has a bump anchor even when Prometheus hasn't yet surfaced the OOM-time limit recording rule. The applied limit — not the spec — is what the kernel killed at: the spec holds the *desired* limit, which the recommender itself rewrites on every in-place resize, so anchoring there would feed the OOM floor its own previous output and compound it on each kill.
- Enqueues the owning Policy for immediate reconcile via a `source.Channel` wired into the Policy reconciler's work queue.

The recommender reads the cache during recommendation build: a hit sets the live OOM signal for that container only — equivalent to the per-container Prometheus `workload_oom_24h` recency signal — and feeds the OOM-floor stage of the [Recommendation Pipeline](recommendation-pipeline.md#stages). The math and bump factor are unchanged — the watcher only makes the trigger and the signal source fresher.

**Operational notes.** The cache is in-memory and lives only for the lifetime of the controller process; on restart, the 24h Prometheus history (`k8s_sustain:workload_oom_24h`, `k8s_sustain:container_oom_limit_24h:bytes`) repopulates the floor on the next reconcile. The event predicate stays local (no apiserver calls): a pod is queued for reconcile because it carries the annotation itself or has ever had an `OOMKilled` container status. The OOM arm is sticky — `LastTerminationState.Terminated` persists on the pod status for the container's lifetime, so once a container has OOM'd, every later event for that pod (a readiness flip, an IP change) keeps re-queuing a reconcile too, not just the kill itself. Queued-pod cardinality is therefore bounded by the number of pods that have OOM'd at least once (plus annotated pods), not by OOM events; but the repeat wakes are cheap: the pre-resolution check described above (pod UID + container + restart count + terminated-at, same tuple the Sink already dedups managed OOMs by) catches a repeat before it costs anything beyond the initial Pod Get, so a chronically-restarting pod's owner/Namespace resolution runs once per distinct kill, not once per status write — including for a pod that turns out to be managed by no Policy at all. The watcher is leader-elected like other controllers — non-leaders idle.

## Admission Webhook (`k8s-sustain webhook`)

The webhook is a [mutating admission webhook](https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/#mutatingadmissionwebhook) that intercepts `pods/CREATE` requests.

**Admission flow:**

1. Pod creation request arrives at the API server
2. API server calls `POST /mutate` on the webhook service
3. Webhook reads `k8s.sustain.io/policy` from the pod's own annotations (the common case, since pods inherit it from the pod template); if the pod carries neither the policy nor the opt-out annotation, it falls back to resolving the owning workload's `metadata.annotations` and then the Namespace's — see [Policy selection](#policy-selection)
4. Resolves the pod's owner chain to determine the workload kind:
   - `Pod → ReplicaSet → Deployment`
   - `Pod → Job → CronJob`
   - `Pod → StatefulSet / DaemonSet`
5. Fetches the named Policy from the API server
6. Checks that the policy manages that workload kind at all. Both `OnCreate` **and** `Ongoing` inject — otherwise an `Ongoing` pod would start on template resources and wait for a resize it does not need
7. Reads the `WorkloadRecommendation` the controller already cached for that workload (`fetchRecommendations` in `internal/webhook/recommendations.go`) — the webhook itself never queries Prometheus
8. Narrows the cached recommendation to the containers present in this pod, matched by name. A container that already has resources set is *not* skipped — the patch replaces whatever the template specified
9. If the cache is stale beyond `DefaultCacheStaleness` (30 min), or `--recommend-only` is set, allows the pod through with its template resources unchanged. If the cache is **missing entirely**, additionally creates a stub `WorkloadRecommendation` so the controller computes one for the next pod of this workload
10. Otherwise returns an RFC 6902 JSON Patch with the recommended resources
11. The API server applies the patch before persisting the pod

The webhook **fails open** (`failurePolicy: Ignore` by default) — if it is unreachable or returns an error, the pod is admitted unchanged. The controller will handle ongoing reconciliation regardless.

**Latency budget.** The handler is bounded by a hard 4s deadline on the admission context (under the apiserver's 5s `MutatingWebhookConfiguration` timeout). The webhook makes no Prometheus calls — the only outbound work in the admission path is a handful of Kubernetes API `Get`s (Policy lookup, owner resolution, the `WorkloadRecommendation` read), each bounded by its own short 2s per-call timeout, so a slow apiserver round-trip cannot exhaust the budget. The webhook always serves the cached `WorkloadRecommendation` written by the controller; if the cache is missing or stale beyond `DefaultCacheStaleness` (30 min), the pod is admitted with its original template resources.

**Informer-backed reads.** With Prometheus out of the admission path, the apiserver is the only remaining source of latency there. The webhook therefore serves its `Policy`, `WorkloadRecommendation` and `Namespace` reads from an informer cache (`k8s.NewCached`) rather than a per-pod `Get` — at several thousand workloads an uncached read per admission is a round trip the cluster does not need. Budget roughly 20–50 MB resident at 10k workloads; the chart's webhook memory request is sized accordingly.

Only those three kinds are cached. The owner-chain kinds (`ReplicaSet`, `Job`, and — for the multi-level opt-in's workload-level read — `Deployment`, `StatefulSet`, `DaemonSet`, `CronJob`, `Rollout`) are read at most once per pod CREATE, and an informer over every object of one of those kinds in the cluster would cost far more memory than the Get it saves — so they stay uncached. The three cached informers are registered and synced during startup, before the webhook is receiving traffic: registering them lazily on first use would push a blocking cluster-wide LIST into the first admission, inside its 2s per-call timeout, and on a large cluster the first pods after every restart would fail open onto template resources.

The owner-chain reads are not entirely without a cost control of their own: see [Annotation Reference](../reference/annotation.md#the-webhooks-cost-control-does-not-fully-bound-itself) for the small TTL cache that keeps a rolling restart from re-reading the same owner's annotations once per replica.

**Stub writes (the webhook's only write).** The webhook computes nothing, so it never writes a recommendation. It does write one thing: when a pod is admitted for a workload identity that has no `WorkloadRecommendation` at all, it creates an empty-status **stub** — recording the admitted pod's container set in `status.observedResources` — and admits the pod unchanged. Creating the object is what puts the identity into the controller's work-list; the [computation phase](#computation) fills in the recommendation on its next cycle, so the next pod of that workload is injected.

This is how an identity the controller's periodic listing never sees gets discovered at all: bare pods and standalone Jobs can be created and deleted entirely between two reconciles, so no target listing may ever contain them. Recording the container set on the stub is what makes the object self-sufficient afterwards — computation reads the containers from there rather than from a workload object that may already be gone. The create is detached from the `AdmissionResponse` and idempotent, which makes a mass scale-out self-debouncing; on SIGTERM those detached writes are cancelled and joined before the informer cache they read through is stopped. See [Workload Recommendations](workload-recommendations.md#cold-start-stub-recommendations) for the full lifecycle.

## Dashboard (`k8s-sustain dashboard`)

The dashboard is an optional web UI that provides:

- **Policy overview** — list all policies with status, namespaces, workload types
- **Workload metrics** — interactive CPU and memory time-series charts
- **Policy simulator** — test "what-if" scenarios with different percentiles, headroom, and min/max values

It is read-only: it queries the Kubernetes API and Prometheus but never modifies any resources. See the [Dashboard guide](../guides/dashboard.md) for details.

## Recommend-only mode

When `--recommend-only` is passed (or `recommendOnly: true` in the Helm values), all three components continue to operate normally — the controller keeps querying Prometheus, computing recommendations, and caching them; the webhook keeps resolving workloads and reading the cached recommendation; the dashboard keeps querying Prometheus for its own charts — but **no mutations are applied**. Recommendations are emitted as structured JSON log lines at `info` level. The same dry-run can be scoped to a single policy with `spec.rightSizing.recommendOnly: true`; the global flag remains a master switch that overrides every policy.

This is useful for:

- Validating that k8s-sustain produces sensible recommendations before enabling active mode
- Auditing what changes would be made without risk
- Running k8s-sustain in a staging environment alongside existing resource settings
- Onboarding one policy in dry-run (`spec.rightSizing.recommendOnly`) while other policies actively apply

## Recommendation pipeline

See [Recommendation Pipeline](recommendation-pipeline.md) for how recommendations are computed.

## Prometheus recording rules

k8s-sustain ships pre-computed recording rules that the controller and webhook query at reconcile time. See [Recording Rules](../reference/recording-rules.md) for the full catalogue.

## Policy selection

Each workload explicitly declares its policy via the `k8s.sustain.io/policy` annotation, honoured at three levels — pod template, the workload object's own `metadata.annotations`, and its Namespace's — most specific first. See the [Annotation reference](../reference/annotation.md) for the full precedence table and the `k8s.sustain.io/opt-out` escape hatch. This is a deliberate design choice:

- **No implicit matching** — a workload is never accidentally governed by a policy. A Namespace annotation only reaches workloads a Policy's own `spec.selector` already covers, and `--excluded-namespaces` remains a hard deny — namespace owners choose among the policies offered to them, they cannot grant themselves one.
- **No ambiguity** — one annotation, one policy, deterministic behavior: the first level that says anything wins.
- **Same resolution, three consumers** — the controller, the webhook, and the dashboard all resolve the annotation through the one shared `internal/policymatch.ResolvePolicy` function, so they can never disagree about which policy a workload is opted into. The webhook reads the pod's own annotations first (inherited from the template, so this is usually all it needs) and only falls back to reading the owning workload and Namespace objects when the pod itself is silent.
