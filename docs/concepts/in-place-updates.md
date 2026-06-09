# In-Place Updates

Kubernetes 1.27 introduced the `InPlacePodVerticalScaling` feature gate (alpha), which became beta (on by default) in Kubernetes 1.31 and GA in Kubernetes 1.33. It allows changing a pod's resource requests and limits **without restarting the container**.

k8s-sustain auto-detects whether the cluster supports the feature and chooses the appropriate code path. There is **no minimum k8s version for k8s-sustain itself** — clusters too old for in-place resize fall back transparently to PDB-respecting eviction.

## Version matrix

| k8s version            | Feature gate                              | Resize API used                              | k8s-sustain behaviour                                                                                              |
|------------------------|-------------------------------------------|----------------------------------------------|--------------------------------------------------------------------------------------------------------------------|
| **≤ 1.26**             | _none_ (feature didn't exist)             | _n/a_                                        | Eviction-only. Stale pods evicted via the Eviction API; webhook re-injects on replacement.                         |
| **1.27 – 1.30 (alpha)** | `--feature-gates=InPlacePodVerticalScaling=true` required on apiserver + kubelet | Direct pod patch (`PATCH /api/v1/.../pods/<name>`) | Detected at startup as `inPlace=false` (the controller treats < 1.31 as not-supported) → eviction path.            |
| **1.31 – 1.32 (beta)** | On by default                             | Direct pod patch (no `/resize` subresource yet) | `inPlace=true`. `/resize` subresource returns 404 → patcher falls back to direct pod patch within the same call.   |
| **1.33+ (GA)**         | Always on                                 | `/resize` subresource (`PATCH /api/v1/.../pods/<name>/resize`) | `inPlace=true`. `/resize` is the primary path; sidecar (restartable init) resize attempted as a separate /resize call. |

## How the runtime path is chosen

At controller startup the discovery API is queried and `major.minor` is compared against `1.31`. The result lives on `Patcher.inPlace`:

```text
INFO  InPlacePodVerticalScaling support  enabled=true   server=v1.33.2
INFO  InPlacePodVerticalScaling support  enabled=false  server=v1.30.5
```

In both modes the patcher lists pods by the workload's label selector and then drops any pod whose controller ownerRef chain does not resolve to the target workload (directly for StatefulSet/DaemonSet pods, via the owning ReplicaSet for Deployment/Argo Rollout pods). A bare debug pod carrying the same labels, or a pod belonging to another workload with an overlapping selector, is never resized or evicted — the skip is logged so overlapping selectors are easy to diagnose.

When `Ongoing` mode is active and `inPlace=true`, the patcher walks each running pod and:

1. Compares the pod spec against the current recommendation.
   - **Spec differs** — a resize is submitted with the new values, even if a previous resize is still pending. The kubelet re-evaluates pending resizes against the new desired state, so a recommendation that has since been lowered can succeed where the old one was infeasible.
   - **Spec already at target** — the kubelet's verdict on the staged resize decides. On k8s ≥ 1.33 the verdict is read from the `PodResizePending` and `PodResizeInProgress` pod conditions; on 1.31–1.32 from the (since-deprecated) `pod.status.resize` field. Both are consulted, so detection works across the full supported range:
     - **`Infeasible`** — the node cannot satisfy the request: the spec carries the target resources but they never landed. The pod is evicted (unconditionally — the spec matching the recommendation must not be mistaken for "already resized") so the scheduler can place the replacement elsewhere; the webhook injects the new resources into the replacement.
     - **`Error`** (reported on the `PodResizeInProgress` condition, kubelet ≥ 1.34) — the kubelet accepted the resize but failed while actuating it, and does not retry on its own. Same eviction fallback as `Infeasible`, so the pod doesn't run on its old allocation forever while the spec claims the target.
     - **`Deferred`** — the kubelet accepted the request but is waiting on conditions (e.g. room freed by another pod terminating). Skipped; the kubelet will apply it without further intervention.
     - **No pending resize** — the pod is at target; nothing to do. An unrecognized future verdict is logged and left to the kubelet.
2. Issues `PATCH /api/v1/.../pods/<name>/resize` (k8s 1.33+).
3. If the API server returns `NotFound` for the subresource (k8s 1.31–1.32), the same call is retried as a direct pod patch.
4. `Invalid` rejections are interpreted per path:
   - **On the `/resize` subresource** — the subresource being served means the feature is available, so `Invalid` is a *per-pod* validation failure (the resize would change the pod's QoS class, decrease a memory limit with a `NotRequired` resize policy, …). Only that pod falls back to eviction (the webhook re-injects on the replacement); in-place stays enabled for every other pod.
   - **On the direct pod patch** (1.31–1.32) — resource mutation is only accepted there when the `InPlacePodVerticalScaling` gate is on, so `Invalid` means the gate is off cluster-wide. The patcher flips `inPlace=false` for the rest of the process and falls back to eviction. The flip is per-process, so a restart re-attempts in-place if the gate has been re-enabled meanwhile.

Sidecar (restartable init) containers are resized in a **separate** `/resize` call so a sidecar rejection on older clusters cannot block the regular-container resize. Failures on the sidecar call are logged and ignored — new requests will land at next pod creation via webhook injection.

## Eviction fallback

On any cluster where `inPlace=false` (auto-detected as < 1.31, or runtime-flipped on Invalid):

- Stale pods are evicted **one at a time** via the Eviction API. After each eviction the patcher waits for the workload's selector to become **quiescent** — the evicted pod is gone and no peer is `Pending` or `Running`-but-`NotReady` — before evicting the next pod. This caps the disruption to (at most) one pod per workload at a time and avoids stampeding a workload with many stale pods.
- The quiescence check uses pod state, not a frozen Ready-count baseline, so it handles HPA scale-down naturally: if the autoscaler decides not to provision a replacement for an evicted pod, the remaining peers stay Ready and the wait returns immediately.
- The wait has a fixed timeout (default 5 minutes, tunable via `controller.recycleReplacementTimeout` / `--recycle-replacement-timeout`). The default is sized to cover node-autoscaling latency: when Karpenter or cluster-autoscaler has to provision a fresh node for the replacement pod, the cold start (node boot + image pull + init containers) regularly takes 2–3 minutes and can hit 5+ on slow registries. Setting the timeout too low produces false-positive aborts during normal scale-up; raise it on clusters with slow provisioning, lower it on tight clusters where you want faster surfacing of stuck rollouts. When the budget elapses the loop aborts for this reconcile so a genuinely stuck workload does not get more pods taken down before the next reconcile can re-evaluate.
- **Crash-loop circuit breaker.** During the post-eviction wait, if any pod in the workload's selector enters `CrashLoopBackOff`, the loop aborts immediately. This guards against a bad recommendation cascading through every pod in the workload — the next reconcile picks up the recommendation drift and re-computes.
- **StatefulSet ordering.** Pods owned by a StatefulSet are evicted in descending ordinal order (e.g. `web-2 → web-1 → web-0`), matching the StatefulSet controller's update semantics. Pods owned by other workload kinds (Deployment, DaemonSet, …) are evicted in alphabetical name order so reconciles stay deterministic.
- 429 responses (PodDisruptionBudget blocking eviction) are logged and skipped — the next reconcile cycle will retry.
- The workload controller (Deployment / StatefulSet / etc.) replaces the evicted pod from the updated template; the webhook injects the latest recommendation into the replacement at admission time.

CronJobs are special-cased: the controller never mutates the CronJob spec (which would cause GitOps drift) and never evicts a job pod (which would kill the run). Job pods are enumerated via the `batch.kubernetes.io/job-name` label and confirmed by controller ownerRef back to the Job (which is itself ownerRef-checked against the CronJob), so a bystander pod carrying the label is never touched. On clusters that support in-place resize, currently-running job pods are resized via the `pods/resize` subresource using the same machinery as Deployments — including for `restartPolicy: Never`/`OnFailure` pods on k8s ≥ 1.35. If the cluster does not support in-place resize, the running pod is left untouched and the next scheduled run picks up the new resources from the webhook.

## Caveats

- **Memory shrink may force restart.** When a memory request is **lowered**, some kubelet versions return `Deferred` until the next container start because the cgroup cannot shrink while the workload is using more than the new limit. k8s-sustain accepts this — the new value lands on the next pod restart, no recycling needed.
- **Memory grow is always live.** Increasing memory requests/limits is applied without restart on supported kernels.
- **CPU is always resizable in-place.** No restart, no kubelet deferral.
- **VPA conflicts.** Running Vertical Pod Autoscaler alongside k8s-sustain on the same pods produces conflicting patches. Use the `k8s.sustain.io/policy` annotation to opt workloads in selectively, and exclude those workloads from VPA targets.
- **Resize status inspection:**

  ```bash
  # k8s ≥ 1.33: pending-resize verdict lives in pod conditions
  kubectl get pod my-pod -o jsonpath='{.status.conditions[?(@.type=="PodResizePending")]}'
  # k8s 1.31–1.32: deprecated status field
  kubectl get pod my-pod -o jsonpath='{.status.resize}'
  # actual resources currently allocated to the containers
  kubectl get pod my-pod -o jsonpath='{.status.containerStatuses[*].resources}'
  ```

## Disabling in-place updates

To force eviction-based behaviour even on supported clusters (e.g. while validating a new k8s version), the feature can be disabled at runtime via the `Invalid`-rejection fallback path: scale the controller to zero, ensure the cluster's `InPlacePodVerticalScaling` feature gate is off, and restart. This is not yet exposed as a Helm value — file an issue if you need a clean toggle.
