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
- If a container already has a non-zero CPU request, it is left unchanged (idempotent)
- Existing running pods are **not** affected — only newly created pods receive the recommendation
- If the webhook is unavailable, the pod is admitted without resource injection (`failurePolicy: Ignore`)

**Best for:**

- Workloads where you want a clean initial resource profile without disrupting running pods
- CronJob pods that are ephemeral and recreated on every run
- Environments where you cannot tolerate rolling restarts

**Limitation:** Existing pods retain their current (possibly over-provisioned) resources until they are naturally restarted (deployment update, node drain, etc.).

---

## Ongoing

Resources are updated by the **controller** on a recurring interval.

```yaml
spec:
  update:
    types:
      deployment: Ongoing
```

**Behaviour on clusters without in-place update support (k8s < 1.27):**

1. Controller patches the workload's pod template with updated resources
2. A `kubectl.kubernetes.io/restartedAt` annotation is added to the template, triggering a rolling restart
3. New pods are scheduled with the recommended resources

**Behaviour on clusters with in-place update support (k8s ≥ 1.27):**

1. Controller patches the workload's pod template (no restart annotation)
2. Controller also patches each running, non-terminating pod's `spec.containers[*].resources` directly
3. The kubelet applies the new resources without restarting the container

See [In-Place Updates](in-place-updates.md) for details.

**Best for:**

- Long-running workloads that accumulate meaningful usage history
- Situations where you want resources to track actual usage over time
- Clusters with in-place update support (zero-disruption updates)

**Limitation:** Causes rolling restarts on clusters without in-place update support (k8s < 1.27).

---

## Choosing a mode

| Scenario | Recommended mode |
|----------|-----------------|
| New cluster, no baseline yet | `OnCreate` — sets a sensible default at creation |
| Existing workloads, must avoid downtime | `OnCreate` — only affects future pods |
| Existing workloads, k8s ≥ 1.27 | `Ongoing` — in-place updates, zero restarts |
| CronJob pods (ephemeral per-run) | `OnCreate` — each run gets fresh recommendations |
| StatefulSets with persistent state | `Ongoing` + k8s ≥ 1.27, or `OnCreate` |
| DaemonSets | `Ongoing` (rolling update is DaemonSet's normal behaviour) |

---

## Mixing modes

A single policy can use different modes for different workload kinds:

```yaml
spec:
  update:
    types:
      deployment: Ongoing      # controller updates templates + in-place pods
      statefulSet: OnCreate    # only inject at pod creation, no disruption
      cronJob: OnCreate        # inject at each job pod creation
      daemonSet: Ongoing       # rolling DaemonSet update
```
