# Architecture

k8s-sustain is split into three independent components that run as separate processes (different container args in the same image):

```
┌─────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                       │
│                                                                 │
│  ┌──────────────────┐        ┌──────────────────────────────┐   │
│  │  k8s-sustain     │        │  k8s-sustain-webhook         │   │
│  │  (controller)    │        │  (admission server)          │   │
│  │                  │        │                              │   │
│  │  Watches Policy  │        │  Intercepts Pod CREATE        │   │
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
│  │  k8s_sustain:container_cpu_usage_by_workload:rate5m      │   │
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
2. For each workload kind enabled in the policy (`deployment`, `statefulSet`, `daemonSet`, `argoRollout`):
   - List all objects of that kind — scoped to the namespaces in `selector.namespaces` when specified, or cluster-wide otherwise
   - Filter by the `k8s.sustain.io/policy` annotation in the pod template
   - Skip workloads with `OnCreate` mode (handled by the webhook)
   - Skip workloads in retry backoff from a previous transient failure
3. Process matching workloads in parallel (bounded by `--concurrency-limit`, default 5):
   - Query Prometheus for workload-level CPU and memory signals (sum across all replicas) at the configured percentile and window
   - Detect autoscalers (HPA / KEDA ScaledObject) targeting the workload — read-only, no patches
   - Divide the workload-total by the median replica count to derive a per-pod recommendation; KEDA scale-to-zero falls back to `max(1, ScaledObject.minReplicaCount)`
   - Apply a per-pod p95 floor to protect against load imbalance across replicas
   - Apply headroom and `min/maxAllowed` clamps; derive limits from the computed request
   - If `--recommend-only` is set, log the recommendation and skip patching
   - Recycle stale running pods: on k8s >= 1.31 via in-place resource patching (using the `/resize` subresource on k8s >= 1.33); on k8s < 1.31 via the Eviction API (PDB-respecting). The webhook injects the latest resources into replacement pods at creation time
   - Emit a `ResourcesUpdated` event on the workload object on success
   - On transient failure (Prometheus timeout, API 5xx), schedule retry with exponential backoff (30s base, 5min cap) and emit a `ReconciliationRetryScheduled` warning event on the workload

The controller requeues after `--reconcile-interval` (default `10m`).

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

## Dashboard (`k8s-sustain dashboard`)

The dashboard is an optional web UI that provides:

- **Policy overview** — list all policies with status, namespaces, workload types
- **Workload metrics** — interactive CPU and memory time-series charts
- **Policy simulator** — test "what-if" scenarios with different percentiles, headroom, and min/max values

It is read-only: it queries the Kubernetes API and Prometheus but never modifies any resources. See the [Dashboard guide](../guides/dashboard.md) for details.

## Recommend-only mode

When `--recommend-only` is passed (or `recommendOnly: true` in the Helm values), all three components continue to operate normally — querying Prometheus, computing recommendations, resolving workloads — but **no mutations are applied**. Recommendations are emitted as structured JSON log lines at `info` level.

This is useful for:

- Validating that the operator produces sensible recommendations before enabling active mode
- Auditing what changes would be made without risk
- Running the operator in a staging environment alongside existing resource settings

## Recommendation pipeline

1. **Workload-level signal.** Recording rules sum container CPU/memory across all replicas of a workload, grouped by `(namespace, owner_kind, owner_name, container)`. A separate rule counts replicas per workload.
2. **Per-pod conversion.** The recommender divides the workload-total at percentile *p* by the median replica count over the recommendation window. KEDA scale-to-zero falls back to `max(1, ScaledObject.minReplicaCount)`.
3. **Per-pod floor.** A per-pod p95 query is used as a `max()` floor on the workload-derived value. This protects against load imbalance: if one replica runs hotter than the average, its p95 sets the floor.
4. **Headroom + clamping.** Standard request-headroom percentage and `min/maxAllowed` clamps are applied as before.
5. **Limits.** Derived from the computed request via the existing limit configuration (`equalsToRequest`, `requestsLimitsRatio`, etc.).

The signal is replica-invariant by construction. HPA scaling does not perturb the recommendation, so no autoscaler object is ever modified.

### Autoscaler coordination

When `spec.rightSizing.autoscalerCoordination.enabled` is `true` and the
workload is targeted by an HPA or KEDA `ScaledObject` on `averageUtilization`,
the recommender shapes the per-pod request so the autoscaler's signal stays
meaningful — multiplying CPU/memory by `(100 / hpa_target_pct) × 1.10` and,
optionally, applying a CPU-only replica-budget correction. The applied
multiplier is exposed via `k8s_sustain_coordination_factor`. See
[Autoscaler Coordination](autoscaler-coordination.md) for the formulas and
detection rules.

## Prometheus recording rules

k8s-sustain relies on pre-computed recording rules to avoid expensive fan-out queries at reconcile time. The chart installs three rule groups:

| Group | Records |
|-------|---------|
| `k8s_sustain.workload_mapping` | Maps pods to their workload owner (resolves RS→Deployment) |
| `k8s_sustain.cpu_rates` | Per-container CPU rate, with and without workload labels |
| `k8s_sustain.memory_rates` | Per-container memory working set, with and without workload labels |

Percentile computation is **not** pre-recorded. At reconcile time the controller and webhook query `k8s_sustain:container_cpu_usage_by_workload:rate5m` and `k8s_sustain:container_memory_by_workload:bytes` using `quantile_over_time` with the exact quantile and window from the policy. This avoids maintaining fixed-window pre-aggregations that would not match per-policy configuration.

## Policy selection

Each workload explicitly declares its policy via the `k8s.sustain.io/policy` annotation on its pod template. This is a deliberate design choice:

- **No implicit matching** — a workload is never accidentally governed by a policy
- **No ambiguity** — one annotation, one policy, deterministic behavior
- **Same annotation, two consumers** — the webhook reads it from the pod (inherited from the template); the controller reads it from the workload's pod template directly
