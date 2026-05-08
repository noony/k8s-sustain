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

When `Ongoing` mode is active and `inPlace=true`, the patcher walks each running pod and:

1. Reads `pod.status.resize` (kubelet's report on the previous resize attempt):
   - **`Infeasible`** — the node cannot satisfy the request. The pod is evicted so the scheduler can place the replacement elsewhere; the webhook injects the new resources into the replacement.
   - **`Deferred`** — the kubelet accepted the request but is waiting on conditions (e.g. memory decrease that requires container restart). Skipped; the kubelet will apply it without further intervention.
   - **`InProgress` / unset** — proceeds with the patch.
2. Issues `PATCH /api/v1/.../pods/<name>/resize` (k8s 1.33+).
3. If the API server returns `NotFound` for the subresource (k8s 1.31–1.32), the same call is retried as a direct pod patch.
4. If the API server returns `Invalid` (the feature gate is off — possible on a 1.31+ cluster with custom flags), the patcher flips `inPlace=false` for the rest of the reconcile cycle and falls back to eviction. The flip is per-process, so the next reconcile re-attempts in-place if the gate has been re-enabled meanwhile.

Sidecar (restartable init) containers are resized in a **separate** `/resize` call so a sidecar rejection on older clusters cannot block the regular-container resize. Failures on the sidecar call are logged and ignored — new requests will land at next pod creation via webhook injection.

## Eviction fallback

On any cluster where `inPlace=false` (auto-detected as < 1.31, or runtime-flipped on Invalid):

- Stale pods are evicted one at a time via the Eviction API.
- 429 responses (PodDisruptionBudget blocking eviction) are logged and skipped — the next reconcile cycle will retry.
- The workload controller (Deployment / StatefulSet / etc.) replaces the evicted pod from the updated template; the webhook injects the latest recommendation into the replacement at admission time.

CronJobs are special-cased: their pods are short-lived job runs that are never recycled. The controller patches the `JobTemplate` so future runs use the updated resources.

## Caveats

- **Memory shrink may force restart.** When a memory request is **lowered**, some kubelet versions return `Deferred` until the next container start because the cgroup cannot shrink while the workload is using more than the new limit. k8s-sustain accepts this — the new value lands on the next pod restart, no recycling needed.
- **Memory grow is always live.** Increasing memory requests/limits is applied without restart on supported kernels.
- **CPU is always resizable in-place.** No restart, no kubelet deferral.
- **VPA conflicts.** Running Vertical Pod Autoscaler alongside k8s-sustain on the same pods produces conflicting patches. Use the `k8s.sustain.io/policy` annotation to opt workloads in selectively, and exclude those workloads from VPA targets.
- **Resize status inspection:**

  ```bash
  kubectl get pod my-pod -o jsonpath='{.status.resize}'
  kubectl get pod my-pod -o jsonpath='{.status.containerStatuses[*].resources}'
  ```

## Disabling in-place updates

To force eviction-based behaviour even on supported clusters (e.g. while validating a new k8s version), the feature can be disabled at runtime via the `Invalid`-rejection fallback path: scale the controller to zero, ensure the cluster's `InPlacePodVerticalScaling` feature gate is off, and restart. This is not yet exposed as a Helm value — file an issue if you need a clean toggle.
