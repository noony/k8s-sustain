# Architecture

k8s-sustain is split into two independent components that run as separate processes (different container args in the same image):

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
│    DaemonSets                                                   │
│                                                                 │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                        Prometheus                        │   │
│  │  k8s_sustain:container_cpu_usage_by_workload:rate5m      │   │
│  │  k8s_sustain:container_memory_by_workload:bytes          │   │
│  └──────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

## Controller (`k8s-sustain start`)

The controller is a standard [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) reconciler that watches `Policy` objects.

**Reconcile loop:**

1. A `Policy` event is received (create / update / periodic requeue)
2. For each workload kind enabled in the policy (`deployment`, `statefulSet`, `daemonSet`, `cronJob`):
   - List all objects of that kind cluster-wide
   - Filter by the `k8s.sustain.io/policy` annotation in the pod template
   - Skip workloads with `OnCreate` mode (handled by the webhook)
3. For each matching workload:
   - Query Prometheus for the p`N` of CPU and memory over the configured window
   - Compute per-container recommendations (request + limit)
   - Patch the workload's pod template with updated resources
   - On clusters ≥ 1.27 with in-place updates enabled: also patch all running pods directly

The controller requeues after `--reconcile-interval` (default `1h`).

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
9. Returns an RFC 6902 JSON Patch with the recommended resources
10. The API server applies the patch before persisting the pod

The webhook **fails open** (`failurePolicy: Ignore` by default) — if it is unreachable or returns an error, the pod is admitted unchanged. The controller will handle ongoing reconciliation regardless.

## Prometheus recording rules

k8s-sustain relies on pre-computed recording rules rather than running expensive instant queries at reconcile time. The chart installs six rule groups:

| Group | Records |
|-------|---------|
| `k8s_sustain.workload_mapping` | Maps pods to their workload owner (resolves RS→Deployment) |
| `k8s_sustain.cpu_rates` | Per-container CPU rate, with and without workload labels |
| `k8s_sustain.memory_rates` | Per-container memory working set, with and without workload labels |
| `k8s_sustain.cpu_percentiles` | p50/p70/p80/p90/p95/p99 per pod and per workload |
| `k8s_sustain.memory_percentiles` | p50/p70/p80/p90/p95/p99 per pod and per workload |

The controller and webhook query `k8s_sustain:container_cpu_usage_by_workload:rate5m` and `k8s_sustain:container_memory_by_workload:bytes` using `quantile_over_time` over the policy's configured window.

## Policy selection

Each workload explicitly declares its policy via the `k8s.sustain.io/policy` annotation on its pod template. This is a deliberate design choice:

- **No implicit matching** — a workload is never accidentally governed by a policy
- **No ambiguity** — one annotation, one policy, deterministic behavior
- **Same annotation, two consumers** — the webhook reads it from the pod (inherited from the template); the controller reads it from the workload's pod template directly
