# In-Place Updates

Kubernetes 1.27 introduced the `InPlacePodVerticalScaling` feature gate (alpha), which became beta (on by default) in Kubernetes 1.33 together with the `pods/resize` subresource as the only supported way to mutate pod resources. It allows changing a pod's resource requests and limits **without restarting the container**.

k8s-sustain auto-detects whether the cluster supports the feature and chooses the appropriate code path. There is **no minimum k8s version for k8s-sustain itself** — clusters below 1.33 fall back transparently to PDB-respecting eviction.

## Version matrix

| k8s version | Feature state                                          | k8s-sustain behaviour                                                                                                                           |
|-------------|--------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|
| **≤ 1.32**  | Feature absent, or alpha gate off by default; no stable `/resize` subresource | Detected at startup as `inPlace=false` → eviction path. Stale pods evicted via the Eviction API; webhook re-injects on replacement. |
| **1.33+**   | On by default; `/resize` subresource (`PATCH /api/v1/.../pods/<name>/resize`) | `inPlace=true`. All resizes go through `/resize`; sidecar (restartable init) resize attempted as a separate `/resize` call.          |

## How the runtime path is chosen

At controller startup the discovery API is queried and `major.minor` is compared against `1.33`. The result lives on `Patcher.inPlace`:

```text
INFO  InPlacePodVerticalScaling support  enabled=true   server=v1.33.2
INFO  InPlacePodVerticalScaling support  enabled=false  server=v1.30.5
```

In both modes the patcher lists pods by the workload's label selector and then drops any pod whose controller ownerRef chain does not resolve to the target workload (directly for StatefulSet/DaemonSet pods, via the owning ReplicaSet for Deployment/Argo Rollout pods). A bare debug pod carrying the same labels, or a pod belonging to another workload with an overlapping selector, is never resized or evicted — the skip is logged so overlapping selectors are easy to diagnose.

When `Ongoing` mode is active and `inPlace=true`, the patcher walks each running pod and:

1. Compares the pod spec against the current recommendation.
   - **Spec differs** — a resize is submitted with the new values, even if a previous resize is still pending. The kubelet re-evaluates pending resizes against the new desired state, so a recommendation that has since been lowered can succeed where the old one was infeasible.
   - **Spec already at target** — the kubelet's verdict on the staged resize decides, read from the `PodResizePending` and `PodResizeInProgress` pod conditions:
     - **`Infeasible`** — the node cannot satisfy the request: the spec carries the target resources but they never landed. The pod is evicted (unconditionally — the spec matching the recommendation must not be mistaken for "already resized") so the scheduler can place the replacement elsewhere; the webhook injects the new resources into the replacement.
     - **`Error`** (reported on the `PodResizeInProgress` condition, kubelet ≥ 1.34) — the kubelet accepted the resize but failed while actuating it, and does not retry on its own. Same eviction fallback as `Infeasible`, so the pod doesn't run on its old allocation forever while the spec claims the target.
     - **`Deferred`** — the kubelet accepted the request but is waiting on conditions (e.g. room freed by another pod terminating). Skipped; the kubelet will apply it without further intervention.
     - **No pending resize** — the pod is at target; nothing to do. An unrecognized future verdict is logged and left to the kubelet.
2. Issues `PATCH /api/v1/.../pods/<name>/resize`.
3. An `Invalid` rejection is a *per-pod* validation failure — the subresource being served means the feature is available (the resize would change the pod's QoS class, decrease a memory limit with a `NotRequired` resize policy, …). Only that pod falls back to eviction (the webhook re-injects on the replacement); in-place stays enabled for every other pod.
4. A `NotFound` response means the pod disappeared between listing and patching; it is skipped without error.

Sidecar (restartable init) containers are resized in a **separate** `/resize` call so a sidecar rejection (their resize needs feature gates beyond `InPlacePodVerticalScaling`) cannot block the regular-container resize. Failures on the sidecar call are logged and ignored — new requests will land at next pod creation via webhook injection.

## Eviction fallback

On any cluster where `inPlace=false` (auto-detected as < 1.33):

- Stale pods are evicted **one at a time** via the Eviction API. After each eviction the patcher waits for the workload's selector to become **quiescent** — the evicted pod is gone and no peer is `Pending` or `Running`-but-`NotReady` — before evicting the next pod. This caps the disruption to (at most) one pod per workload at a time and avoids stampeding a workload with many stale pods.
- The quiescence check uses pod state, not a frozen Ready-count baseline, so it handles HPA scale-down naturally: if the autoscaler decides not to provision a replacement for an evicted pod, the remaining peers stay Ready and the wait returns immediately.
- The wait has a fixed timeout (default 5 minutes, tunable via `controller.recycleReplacementTimeout` / `--recycle-replacement-timeout`). The default is sized to cover node-autoscaling latency: when Karpenter or cluster-autoscaler has to provision a fresh node for the replacement pod, the cold start (node boot + image pull + init containers) regularly takes 2–3 minutes and can hit 5+ on slow registries. Setting the timeout too low produces false-positive aborts during normal scale-up; raise it on clusters with slow provisioning, lower it on tight clusters where you want faster surfacing of stuck rollouts. When the budget elapses the loop aborts for this reconcile so a genuinely stuck workload does not get more pods taken down before the next reconcile can re-evaluate.
- **Crash-loop circuit breaker.** During the post-eviction wait, if any pod in the workload's selector enters `CrashLoopBackOff`, the loop aborts immediately. This guards against a bad recommendation cascading through every pod in the workload — the next reconcile picks up the recommendation drift and re-computes.
- **StatefulSet ordering.** Pods owned by a StatefulSet are evicted in descending ordinal order (e.g. `web-2 → web-1 → web-0`), matching the StatefulSet controller's update semantics. Pods owned by other workload kinds (Deployment, DaemonSet, …) are evicted in alphabetical name order so reconciles stay deterministic.
- 429 responses (PodDisruptionBudget blocking eviction) are logged and skipped — the next reconcile cycle will retry.
- Pods annotated `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` are never evicted (only the literal `"false"` blocks, matching cluster-autoscaler's convention). The skip is logged and the loop moves to the next pod. Set `spec.rightSizing.update.eviction.ignoreAutoscalerSafeToEvictAnnotations: true` to evict them anyway. In-place resizes are not gated by the annotation — a resize does not disrupt the pod. This gate also covers the eviction fallback for `Infeasible`/`Error` in-place resizes described above.
- The workload controller (Deployment / StatefulSet / etc.) replaces the evicted pod from the updated template; the webhook injects the latest recommendation into the replacement at admission time.

CronJobs are special-cased: the controller never mutates the CronJob spec (which would cause GitOps drift) and never evicts a job pod (which would kill the run). Job pods are enumerated via the `batch.kubernetes.io/job-name` label and confirmed by controller ownerRef back to the Job (which is itself ownerRef-checked against the CronJob), so a bystander pod carrying the label is never touched. On clusters that support in-place resize, currently-running job pods are resized via the `pods/resize` subresource using the same machinery as Deployments — including for `restartPolicy: Never`/`OnFailure` pods on k8s ≥ 1.35. If the cluster does not support in-place resize, the running pod is left untouched and the next scheduled run picks up the new resources from the webhook. Standalone Jobs (not owned by a CronJob) get the same treatment when `job: Ongoing` is set: their running pods are resized in place and never evicted. Unlike a CronJob there is no next run, so on clusters without in-place support the pod simply keeps its original resources for the rest of its lifetime.

## Caveats

- **Memory shrink may force restart.** When a memory request is **lowered**, some kubelet versions return `Deferred` until the next container start because the cgroup cannot shrink while the workload is using more than the new limit. k8s-sustain accepts this — the new value lands on the next pod restart, no recycling needed.
- **Memory grow is always live.** Increasing memory requests/limits is applied without restart on supported kernels.
- **CPU is always resizable in-place.** No restart, no kubelet deferral.
- **VPA conflicts.** Running Vertical Pod Autoscaler alongside k8s-sustain on the same pods produces conflicting patches. Use the `k8s.sustain.io/policy` annotation to opt workloads in selectively, and exclude those workloads from VPA targets.
- **Resize status inspection:**

  ```bash
  # pending-resize verdict lives in pod conditions
  kubectl get pod my-pod -o jsonpath='{.status.conditions[?(@.type=="PodResizePending")]}'
  # actual resources currently allocated to the containers
  kubectl get pod my-pod -o jsonpath='{.status.containerStatuses[*].resources}'
  ```

## Disabling in-place updates

There is currently no toggle to force eviction-based behaviour on a supported cluster: the mode follows the detected server version. File an issue if you need a clean toggle.
