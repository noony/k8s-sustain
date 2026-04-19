# Update Modes

Each workload kind in a `Policy` is configured with one of two update modes. You can mix modes across workload kinds within the same policy.

## OnCreate

Resources are injected by the **admission webhook** at pod creation time, before the pod is scheduled.

```yaml
spec:
  update:
    types:
      deployment: OnCreate
```

**Behaviour:**

- The webhook intercepts every `Pod CREATE` request for pods that carry the policy annotation
- The latest recommendation is always injected — the webhook overrides whatever the pod template currently specifies
- Existing running pods are **not** affected — only newly created pods receive the recommendation
- If the webhook is unavailable, the pod is admitted without resource injection (`failurePolicy: Ignore`)

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
  update:
    types:
      deployment: Ongoing
```

**At pod creation (webhook):**

- The webhook intercepts `Pod CREATE` requests for pods that carry the policy annotation
- Unlike OnCreate mode, the webhook always injects the latest recommendation — even if the container already has a CPU request — ensuring new pods never start with stale resources

**Ongoing reconciliation (controller) on clusters without in-place update support (k8s < 1.31):**

1. Each running pod that has stale resources is evicted via the Eviction API
2. The workload controller (Deployment/StatefulSet/DaemonSet) creates replacement pods
3. The webhook injects the latest recommendations into the new pods at creation time
4. PodDisruptionBudgets are respected — pods blocked by a PDB are skipped and retried on the next reconcile cycle

**Ongoing reconciliation (controller) on clusters with in-place update support (k8s ≥ 1.31):**

1. Controller patches each running, non-terminating pod's `spec.containers[*].resources` directly
2. The kubelet applies the new resources without restarting the container
3. If the kubelet reports `Infeasible` (node cannot satisfy the request), the pod is evicted as a fallback
4. If the kubelet reports `Deferred`, the resize is pending kubelet-side conditions and no action is taken

See [In-Place Updates](in-place-updates.md) for details.

**Best for:**

- Long-running workloads that accumulate meaningful usage history
- Situations where you want resources to track actual usage over time
- Clusters with in-place update support (zero-disruption updates, k8s ≥ 1.31)

**Note:** The controller never patches workload templates (Deployment, StatefulSet, etc.) — the webhook handles resource injection at pod creation. On clusters without in-place update support (k8s < 1.31), pods are replaced via eviction, which causes pod restarts.

---

## Choosing a mode

| Scenario | Recommended mode |
|----------|-----------------|
| New cluster, no baseline yet | `OnCreate` — sets a sensible default at creation |
| Existing workloads, must avoid downtime | `OnCreate` — only affects future pods |
| Existing workloads, k8s ≥ 1.31 | `Ongoing` — in-place updates, zero restarts |
| CronJob pods (ephemeral per-run) | `OnCreate` — each run gets fresh recommendations |
| StatefulSets with persistent state | `Ongoing` + k8s ≥ 1.31, or `OnCreate` |
| DaemonSets | `Ongoing` (rolling update is DaemonSet's normal behaviour) |

---

## Recommend-only mode

Independently of `OnCreate` or `Ongoing`, you can run the entire operator in **recommend-only** mode by passing `--recommend-only` or setting `recommendOnly: true` in the Helm values. In this mode:

- The controller still reconciles and computes recommendations, but **never recycles pods**
- The webhook still intercepts pod creation and computes recommendations, but **never mutates the pod**
- Computed recommendations are logged as structured JSON at `info` level

This is useful for validating recommendations before switching to active mode. See the [CLI reference](../reference/cli.md) for details.

---

## Mixing modes

A single policy can use different modes for different workload kinds:

```yaml
spec:
  update:
    types:
      deployment: Ongoing      # controller recycles stale pods; webhook injects resources
      statefulSet: OnCreate    # only inject at pod creation, no disruption
      cronJob: OnCreate        # inject at each job pod creation
      daemonSet: Ongoing       # controller recycles stale pods; webhook injects resources
```
