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

- The webhook intercepts every `Pod CREATE` request for pods opted into the policy — through the pod template, the workload or its Namespace (see [resolution order](../reference/annotation.md#resolution-order))
- The latest recommendation is always injected — the webhook overrides whatever the pod template currently specifies
- Existing running pods are **not** affected — only newly created pods receive the recommendation
- If the webhook is unavailable, the pod is admitted without resource injection (`failurePolicy: Ignore`)
- The controller still computes a recommendation for `OnCreate` workloads on its regular reconcile cycle and caches it in a `WorkloadRecommendation` object — this keeps the workload visible on the dashboard and is what the webhook injects at admission (the webhook has no other source of recommendations), but the controller **never** recycles, resizes, or otherwise mutates the workload in this mode
- The dashboard reports pods still waiting for a rollout to pick up the recommendation as drift (stale pods)

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

- As in OnCreate mode, the webhook injects the latest recommendation over whatever the template specifies, so new pods never start with stale resources and never wait for the controller's first resize

`cronJob`, `job` and `pod` never take the eviction path: their running pods are resized in place or not at all — see [Kinds that are never evicted](in-place-updates.md#kinds-that-are-never-evicted).

**Ongoing reconciliation (controller) on clusters without in-place update support (k8s < 1.33):**

1. Each non-terminal pod (Running or Pending) with stale resources is evicted via the Eviction API, one at a time (see [Eviction safeguards](#eviction-safeguards)). Staleness is detected on both requests and limits. Evicting Pending pods unblocks workloads stuck unschedulable because their original request was too large.
2. The workload controller (Deployment/StatefulSet/DaemonSet) creates replacement pods
3. The webhook injects the latest recommendations into the new pods at creation time
4. PodDisruptionBudgets are respected — pods blocked by a PDB are skipped and retried on the next reconcile cycle

**Ongoing reconciliation (controller) on clusters with in-place update support (k8s ≥ 1.33):**

1. Controller resizes each running, non-terminating pod through the `pods/resize` subresource
2. The kubelet applies the new resources, without restarting the container unless its `resizePolicy` requires it
3. If the kubelet reports `Infeasible` or `Error`, or the API server rejects the resize as invalid, the pod is evicted as a fallback; `Deferred` resizes are left to the kubelet

See [In-Place Updates](in-place-updates.md) for details.

**Best for:**

- Long-running workloads that accumulate meaningful usage history
- Situations where you want resources to track actual usage over time
- Clusters with in-place update support (zero-disruption updates, k8s ≥ 1.33)

The controller never patches workload templates (Deployment, StatefulSet, CronJob, …), so GitOps tools see no drift — see [Verifying applied resources](#verifying-applied-resources).

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
| Long-running bare pods (k8s ≥ 1.35, see [Version matrix](in-place-updates.md#version-matrix)) | `Ongoing` — resizes the running pod in place; never evicted |
| Short-lived bare pods, or tasks that cannot tolerate a container restart | `OnCreate` — inject at admission only |
| StatefulSets with persistent state | `Ongoing` + k8s ≥ 1.33, or `OnCreate` |
| DaemonSets | `Ongoing` (rolling update is DaemonSet's normal behaviour) |
| Argo Rollouts | `Ongoing` or `OnCreate` — works like Deployments with canary/blue-green strategies |

---

## Downsize threshold

To avoid churning pods over noise, the controller ignores small **decreases**. A decrease is acted on only when it is at least `max(percent% of the current value, minDecrease)`, set per resource with `resourcesConfigs.<cpu|memory>.downsizeThreshold` (defaults: 5%, with `minDecrease` 10m for CPU and 15Mi for memory). The check is per container and per value (request and limit).

- **Increases always apply**, however small.
- Setting both `percent` and `minDecrease` to `0` acts on every decrease.
- The threshold only gates resizing and eviction of running pods; new pods still receive the exact recommendation from the webhook.

See the [Policy reference](../reference/policy.md#cpudownsizethreshold-memorydownsizethreshold).

---

## Eviction safeguards

Whenever the controller evicts a pod — on k8s < 1.33, or as the fallback for a failed in-place resize — it applies these guards:

- **Ownership check.** Pods are listed by the workload's selector and then kept only when their controller ownerRef chain resolves to the target workload's UID (directly for StatefulSet/DaemonSet/Job, via the ReplicaSet for Deployment/Argo Rollout). A bystander pod that merely shares the labels is never touched. The same check gates in-place resizes.
- **PodDisruptionBudgets.** Evictions go through the Eviction API; a PDB-blocked pod is skipped and retried next reconcile.
- **`safe-to-evict` annotation.** Pods annotated `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` are never evicted. Set `spec.rightSizing.update.eviction.ignoreAutoscalerSafeToEvictAnnotations: true` to evict them anyway. In-place resizes are not gated by it.
- **One pod at a time.** After each eviction the controller waits for the workload to become quiescent (evicted pod gone, no peer Pending or not Ready) before evicting the next, up to `--recycle-replacement-timeout` (default 5m). On timeout it stops for this reconcile.
- **Crash-loop halt.** If any pod of the workload enters `CrashLoopBackOff` during that wait, the loop stops. Later reconciles evict nothing while a pod already running the recommendation is still in `CrashLoopBackOff`, so a bad recommendation cannot cascade one pod per reconcile either: the workload is Blocked (see `k8s_sustain_workload_retry_state` in [Metrics](../reference/metrics.md#drift-retry-autoscaler)) until the pod recovers or the recommendation changes. A crash-looping pod still on its old resources halts nothing — the new values may be its fix.
- **StatefulSet ordering.** StatefulSet pods are evicted in descending ordinal order (`web-2 → web-1 → web-0`); other kinds in name order.

CronJob, Job and bare-pod pods are never evicted — see [Kinds that are never evicted](in-place-updates.md#kinds-that-are-never-evicted). More detail on the wait is in [Eviction fallback](in-place-updates.md#eviction-fallback).

---

## Verifying applied resources

The workload's pod template never changes, so `kubectl get deployment -o yaml` keeps showing the original resources. Check the recommendation and the pods instead:

```bash
# cached recommendation (what the webhook injects and the controller applies)
kubectl get wlrec -n <namespace>
kubectl get wlrec <kind>-<name> -n <namespace> -o yaml

# resources on the running pods
kubectl get pods -n <namespace> -l <workload-selector> \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].resources}{"\n"}{end}'
```

The workload also gets a `ResourcesUpdated` event whenever the controller resizes or evicts at least one of its pods (`kubectl describe`).

---

## Recommend-only mode

Recommend-only is a dry-run that works independently of `OnCreate` or `Ongoing`, at two scopes:

- **Globally**, with `--recommend-only` (Helm `recommendOnly: true`, which sets it on the controller and the webhook). This is a master switch: every policy is dry-run regardless of its own field.
- **Per policy**, with `spec.rightSizing.recommendOnly: true` — useful to onboard one policy while others actively apply.

In this mode:

- The controller still reconciles, computes and caches recommendations in `WorkloadRecommendation` objects, but **never resizes or evicts pods**
- The webhook still resolves pods and reads the cache, but **never injects resources** (only the owner-name label mirror is still applied)
- The controller logs each recommendation at `info` level — the chart's default `controller.logLevel: error` hides these, so read `kubectl get wlrec` or the dashboard instead, or raise the log level

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
