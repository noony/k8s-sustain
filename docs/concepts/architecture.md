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

**Reconcile loop:**

1. A `Policy` event is received (create / update / periodic requeue)
2. For each workload kind enabled in the policy (`deployment`, `statefulSet`, `daemonSet`, `argoRollout`, `cronJob`):
   - List all objects of that kind — scoped to the namespaces in `selector.namespaces` when specified, or cluster-wide otherwise
   - Filter by the `k8s.sustain.io/policy` annotation in the pod template
   - Skip workloads in retry backoff from a previous transient failure
3. Process matching workloads in parallel (bounded by `--concurrency-limit`, default 5):
   - Detect autoscalers (HPA / KEDA `ScaledObject`) targeting the workload — read-only, no patches
   - Compute a per-container recommendation (see [Recommendation Pipeline](recommendation-pipeline.md)) and cache it in a `WorkloadRecommendation` object, regardless of update mode — this keeps `OnCreate` workloads visible on the dashboard and gives the webhook a Prometheus-outage fallback
   - `OnCreate`-mode workloads stop here: the recommendation is computed and cached, but never applied by the controller — resource injection at pod creation is the webhook's job
   - If `--recommend-only` is set, log the recommendation and skip patching
   - Recycle stale running pods: on k8s >= 1.33 via in-place resource patching through the `/resize` subresource; on k8s < 1.33 via the Eviction API (PDB-respecting). The webhook injects the latest resources into replacement pods at creation time. Pods are listed by the workload's label selector and then filtered by **controller ownership**: a pod is only recycled when its ownerRef chain resolves to the target workload (directly for StatefulSet/DaemonSet, via the owning ReplicaSet for Deployment/Argo Rollout). Bare pods or pods of another workload with an overlapping selector are skipped and logged — the opt-in contract is per-workload
   - **CronJob exception:** the controller never mutates the CronJob spec and never evicts a job pod. On clusters that support in-place resize, currently-running job pods are resized via `pods/resize`; otherwise they finish on their existing resources and the next scheduled run picks up the new values from the webhook
   - Emit a `ResourcesUpdated` event on the workload object on success
   - On transient failure (Prometheus timeout, API 5xx), schedule retry with exponential backoff (30s base, 5min cap) and emit a `ReconciliationRetryScheduled` warning event on the workload

The controller requeues after `--reconcile-interval` (default `5m`).

### Pod OOM watcher

A second controller-runtime reconciler runs alongside the Policy reconciler and watches `Pod` objects cluster-wide, filtered to those carrying the `k8s.sustain.io/policy` annotation. It exists to close the multi-minute latency window between an OOM kill and the next recording-rule-driven reconcile (kube-state-metrics scrape → Prometheus scrape → 1m rule evaluation → 5–10m reconcile interval).

When a container's `LastTerminationState.Terminated.Reason == "OOMKilled"` is observed, the watcher:

- Resolves the pod's top-level workload owner and upserts an entry in an in-memory cache keyed by `(namespace, ownerKind, ownerName, container)`. Restart-count and termination timestamp are used to dedup repeated observations of the same kill.
- Captures the container's memory limit from the pod spec at that moment, so the recommender has a bump anchor even when Prometheus hasn't yet surfaced the OOM-time limit recording rule.
- Enqueues the owning Policy for immediate reconcile via a `source.Channel` wired into the Policy reconciler's work queue.

The recommender reads the cache during recommendation build: a hit sets the live OOM signal for that container only — equivalent to the per-container Prometheus `workload_oom_24h` recency signal — and feeds the OOM-floor stage of the [Recommendation Pipeline](recommendation-pipeline.md#stages). The math and bump factor are unchanged — the watcher only makes the trigger and the signal source fresher.

**Operational notes.** The cache is in-memory and lives only for the lifetime of the controller process; on restart, the 24h Prometheus history (`k8s_sustain:workload_oom_24h`, `k8s_sustain:container_oom_limit_24h:bytes`) repopulates the floor on the next reconcile. Only pods carrying the `k8s.sustain.io/policy` annotation are watched, so cardinality is bounded by opted-in workloads. The watcher is leader-elected like other controllers — non-leaders idle.

## Admission Webhook (`k8s-sustain webhook`)

The webhook is a [mutating admission webhook](https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/#mutatingadmissionwebhook) that intercepts `pods/CREATE` requests.

**Admission flow:**

1. Pod creation request arrives at the API server
2. API server calls `POST /mutate` on the webhook service
3. Webhook reads `k8s.sustain.io/policy` from the pod's annotations
4. Resolves the pod's owner chain to determine the workload kind:
   - `Pod → ReplicaSet → Deployment`
   - `Pod → Job → CronJob`
   - `Pod → StatefulSet / DaemonSet`
5. Fetches the named Policy from the API server
6. Checks that the policy has `OnCreate` mode for that workload kind
7. Queries Prometheus for current recommendations
8. Skips containers that already have a CPU request set
9. If `--recommend-only` is set, logs the recommendation and allows the pod through unchanged
10. Returns an RFC 6902 JSON Patch with the recommended resources
11. The API server applies the patch before persisting the pod

The webhook **fails open** (`failurePolicy: Ignore` by default) — if it is unreachable or returns an error, the pod is admitted unchanged. The controller will handle ongoing reconciliation regardless.

**Latency budget.** The handler is bounded by a hard 4s deadline on the admission context (under the apiserver's 5s `MutatingWebhookConfiguration` timeout). Each Prometheus query has its own short 2s per-query timeout so a slow upstream cannot exhaust the budget for the cache-fallback path. If Prometheus is unavailable, the webhook serves the cached `WorkloadRecommendation` written by the controller; if the cache is missing or stale beyond `DefaultCacheStaleness` (30 min), the pod is admitted with its original template resources.

**Ephemeral-identity cache writes.** For bare pods and standalone Jobs — workloads that can be created and deleted entirely between two controller reconciles — the webhook itself writes the `WorkloadRecommendation` after computing a fresh recommendation, in a detached goroutine so the write never blocks the `AdmissionResponse`. See [Workload Recommendations](workload-recommendations.md) for details.

## Dashboard (`k8s-sustain dashboard`)

The dashboard is an optional web UI that provides:

- **Policy overview** — list all policies with status, namespaces, workload types
- **Workload metrics** — interactive CPU and memory time-series charts
- **Policy simulator** — test "what-if" scenarios with different percentiles, headroom, and min/max values

It is read-only: it queries the Kubernetes API and Prometheus but never modifies any resources. See the [Dashboard guide](../guides/dashboard.md) for details.

## Recommend-only mode

When `--recommend-only` is passed (or `recommendOnly: true` in the Helm values), all three components continue to operate normally — querying Prometheus, computing recommendations, resolving workloads — but **no mutations are applied**. Recommendations are emitted as structured JSON log lines at `info` level.

This is useful for:

- Validating that k8s-sustain produces sensible recommendations before enabling active mode
- Auditing what changes would be made without risk
- Running k8s-sustain in a staging environment alongside existing resource settings

## Recommendation pipeline

See [Recommendation Pipeline](recommendation-pipeline.md) for how recommendations are computed.

## Prometheus recording rules

k8s-sustain ships pre-computed recording rules that the controller and webhook query at reconcile time. See [Recording Rules](../reference/recording-rules.md) for the full catalogue.

## Policy selection

Each workload explicitly declares its policy via the `k8s.sustain.io/policy` annotation on its pod template. This is a deliberate design choice:

- **No implicit matching** — a workload is never accidentally governed by a policy
- **No ambiguity** — one annotation, one policy, deterministic behavior
- **Same annotation, two consumers** — the webhook reads it from the pod (inherited from the template); the controller reads it from the workload's pod template directly
