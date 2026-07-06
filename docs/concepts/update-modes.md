# Update Modes

Each workload kind in a `Policy` is configured with one of two update modes. You can mix modes across workload kinds within the same policy.

## OnCreate

Resources are injected by the **admission webhook** at pod creation time, before the pod is scheduled.

```yaml
spec:
  rightSizing:
    update:
      types:
        deployment: OnCreate
```

**Behaviour:**

- The webhook intercepts every `Pod CREATE` request for pods that carry the policy annotation
- The latest recommendation is always injected — the webhook overrides whatever the pod template currently specifies
- Existing running pods are **not** affected — only newly created pods receive the recommendation
- If the webhook is unavailable, the pod is admitted without resource injection (`failurePolicy: Ignore`)
- The controller still computes a recommendation for `OnCreate` workloads on its regular reconcile cycle and caches it in a `WorkloadRecommendation` object — this keeps the workload visible on the dashboard and gives the webhook a fallback value during a Prometheus outage, but the controller **never** recycles, resizes, or otherwise mutates the workload in this mode

**Best for:**

- Workloads where you want a clean initial resource profile without disrupting running pods
- CronJob pods that are ephemeral and recreated on every run
- Environments where you cannot tolerate rolling restarts

**Limitation:** Existing pods retain their current (possibly over-provisioned) resources until they are naturally restarted (deployment update, node drain, etc.).

---

## Ongoing

Resources are updated by the **controller** on a recurring interval. Additionally, the **admission webhook** injects the latest recommendation at pod creation time so that new pods start with correct resources immediately, without waiting for the controller to reconcile.

```yaml
spec:
  rightSizing:
    update:
      types:
        deployment: Ongoing
```

**At pod creation (webhook):**

- The webhook intercepts `Pod CREATE` requests for pods that carry the policy annotation
- Unlike OnCreate mode, the webhook always injects the latest recommendation — even if the container already has a CPU request — ensuring new pods never start with stale resources

**Ongoing reconciliation (controller) on clusters without in-place update support (k8s < 1.33):**

1. Each non-terminal pod (Running or Pending) with stale resources is evicted via the Eviction API. Staleness is detected on both requests and limits — a recommendation that only changes a limit still triggers a recycle. Evicting Pending pods unblocks workloads stuck unschedulable because their original request was too large; the webhook re-injects the smaller recommendation on the replacement.
2. The workload controller (Deployment/StatefulSet/DaemonSet) creates replacement pods
3. The webhook injects the latest recommendations into the new pods at creation time
4. PodDisruptionBudgets are respected — pods blocked by a PDB are skipped and retried on the next reconcile cycle

**Ongoing reconciliation (controller) on clusters with in-place update support (k8s ≥ 1.33):**

1. Controller patches each running, non-terminating pod's `spec.containers[*].resources` directly
2. The kubelet applies the new resources without restarting the container
3. If the kubelet reports `Infeasible` (node cannot satisfy the request) or `Error` (actuating the accepted resize failed), the pod is evicted as a fallback
4. If the kubelet reports `Deferred`, the resize is pending kubelet-side conditions and no action is taken

See [In-Place Updates](in-place-updates.md) for details.

**Best for:**

- Long-running workloads that accumulate meaningful usage history
- Situations where you want resources to track actual usage over time
- Clusters with in-place update support (zero-disruption updates, k8s ≥ 1.33)

**Note:** The controller never patches workload templates (Deployment, StatefulSet, CronJob, etc.) — the webhook handles resource injection at pod creation. On clusters without in-place update support (k8s < 1.33), pods are replaced via PDB-respecting eviction, which causes pod restarts.

**CronJob exception:** for `cronJob: Ongoing`, eviction is *never* used — evicting a Job pod would kill the run. Currently-running job pods are resized in place when the cluster supports it (k8s ≥ 1.33, with full coverage of `restartPolicy: Never`/`OnFailure` on k8s ≥ 1.35); otherwise they are left to finish on their original resources and the next scheduled run picks up the new values from the webhook. The CronJob spec itself is never modified, so GitOps tools see no drift.

**Standalone Job exception:** `job: Ongoing` behaves the same way — the controller resizes a standalone Job's currently-running pods in place and never evicts them (which would discard in-flight work) or mutates the Job spec. Because a standalone Job has no next run, in-place resize is the only post-creation correction, so `Ongoing` is worthwhile only for **long-running** Jobs and requires k8s ≥ 1.35 (standalone Jobs always run with `restartPolicy: Never`/`OnFailure`). On clusters without in-place support the running pod is left untouched. Jobs owned by a CronJob are handled by the CronJob path above, not this one.

---

## Choosing a mode

| Scenario | Recommended mode |
|----------|-----------------|
| New cluster, no baseline yet | `OnCreate` — sets a sensible default at creation |
| Existing workloads, must avoid downtime | `OnCreate` — only affects future pods |
| Existing workloads, k8s ≥ 1.33 | `Ongoing` — in-place updates, zero restarts |
| CronJob pods (ephemeral per-run) | `OnCreate` — each run gets fresh recommendations |
| Long-running standalone Jobs (k8s ≥ 1.35) | `Ongoing` — resizes the running pod in place mid-run |
| Short-lived standalone Jobs | `OnCreate` — pods finish before a reconcile would touch them |
| StatefulSets with persistent state | `Ongoing` + k8s ≥ 1.33, or `OnCreate` |
| DaemonSets | `Ongoing` (rolling update is DaemonSet's normal behaviour) |
| Argo Rollouts | `Ongoing` or `OnCreate` — works like Deployments with canary/blue-green strategies |

---

## Recommend-only mode

Independently of `OnCreate` or `Ongoing`, you can run in **recommend-only** mode at two scopes: globally, by passing `--recommend-only` (or `recommendOnly: true` in the Helm values), or per policy, by setting `spec.rightSizing.recommendOnly: true` on an individual `Policy`. The global flag is a master switch — when set, every policy is dry-run regardless of its own field. In this mode:

- The controller still reconciles and computes recommendations, but **never recycles pods**
- The webhook still intercepts pod creation and computes recommendations, but **never injects resources** (only the owner-name metadata label mirror is still applied)
- Computed recommendations are logged as structured JSON at `info` level

This is useful for validating recommendations before switching to active mode. See the [CLI reference](../reference/cli.md) for details.

---

## Mixing modes

A single policy can use different modes for different workload kinds:

```yaml
spec:
  rightSizing:
    update:
      types:
        deployment: Ongoing      # controller recycles stale pods; webhook injects resources
        statefulSet: OnCreate    # only inject at pod creation, no disruption
        cronJob: OnCreate        # inject at each job pod creation
        daemonSet: Ongoing       # controller recycles stale pods; webhook injects resources
```
