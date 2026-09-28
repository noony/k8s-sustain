# In-Place Updates

Kubernetes 1.33 enables in-place pod resize (`InPlacePodVerticalScaling`, beta and on by default) with the `pods/resize` subresource. It changes a pod's requests and limits **without recreating the pod**.

k8s-sustain detects support at startup and picks the code path. Clusters below 1.33 (k8s-sustain itself needs [1.29 or later](../getting-started/prerequisites.md#kubernetes)) fall back to PDB-respecting eviction.

## Version matrix

| k8s version | k8s-sustain behaviour |
|-------------|-----------------------|
| **1.29 – 1.32** | `inPlace=false` → eviction path. Stale pods are evicted via the Eviction API; the webhook injects the recommendation into the replacement. CronJob, Job and bare-pod pods are not touched. |
| **1.33+** | `inPlace=true`. All resizes go through `pods/resize`; sidecar (restartable init) containers are resized in a separate call. |

The gate is the server version alone: the controller compares `major.minor` against 1.33 and does not probe feature gates.

!!! warning "`restartPolicy: Never` / `OnFailure` pods need 1.35"
    Job pods, CronJob runs and most bare pods (Airflow's `KubernetesPodOperator` uses `Never`) run with `restartPolicy: Never` or `OnFailure`. On 1.33 and 1.34 the API server rejects resizing those pods; the rejection is logged per pod and, for these kinds, never escalated to eviction. Full coverage starts at **1.35**.

## How the runtime path is chosen

At controller startup the discovery API is queried for the server version. The result is logged:

```text
INFO  InPlacePodVerticalScaling support  enabled=true   server=v1.33.2
INFO  InPlacePodVerticalScaling support  enabled=false  server=v1.30.5
```

In both modes only pods owned by the target workload are touched — see [Eviction safeguards](update-modes.md#eviction-safeguards).

`Pod`-kind targets have no ownerRef or selector; their membership comes from the grouping rule instead — see [Bare pods](#bare-pods).

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

On clusters below 1.33, and for pods whose in-place resize fails on newer clusters, stale pods are evicted. The guards (ownership check, PDBs, `safe-to-evict`, one pod at a time, crash-loop halt, StatefulSet ordering) are listed in [Eviction safeguards](update-modes.md#eviction-safeguards). Details of the wait between evictions:

- Quiescence is judged from pod state (evicted pod gone, no peer `Pending` or `Running`-but-not-Ready), not a Ready-count baseline, so HPA scale-down is handled: if no replacement is provisioned, the remaining peers stay Ready and the wait returns immediately.
- The wait times out after `--recycle-replacement-timeout` (Helm `controller.recycleReplacementTimeout`, default 5m), sized to cover a node-autoscaler provisioning a fresh node for the replacement. When it elapses, the loop stops for this reconcile so a stuck workload loses no more pods.
- The workload controller replaces the evicted pod from its unchanged template; the webhook injects the latest recommendation at admission.

## Kinds that are never evicted

Three kinds opt out of the eviction path entirely, on the same reasoning: evicting the pod would destroy work that nothing will redo. For all three, in-place resize is the *only* post-creation correction, so on a cluster without in-place support the running pod simply keeps its original resources.

### CronJobs and Jobs

The controller never mutates the CronJob or Job spec and never evicts a job pod (which would kill the run). Job pods are found via the `batch.kubernetes.io/job-name` label and confirmed by controller ownerRef back to the Job (itself ownerRef-checked against the CronJob). Running job pods are resized in place when the cluster supports it (see the [version matrix](#version-matrix)); otherwise they finish on their original resources and, for a CronJob, the next run gets the new values from the webhook. A standalone Job (`job: Ongoing`) has no next run, so in-place resize is its only correction.

### Bare pods

Bare pods opted in via `k8s.sustain.io/owner-name` (kind `Pod`) are the third member of that family, and the strongest case of it: no controller exists that could recreate an evicted bare pod, so eviction would not disrupt the workload — it would delete it. Under `pod: Ongoing` their running pods **are** resized in place, through the same `pods/resize` machinery; an in-place resize needs no controller behind it, and without it a long-running Airflow task would keep whatever it was admitted with for its entire life. Under `pod: OnCreate`, nothing is applied to a running pod at all and the recommendation reaches the identity's next pod through the webhook.

Membership is decided by the grouping rule rather than a label selector: a pod with no controller `ownerReference`, a valid `k8s.sustain.io/owner-name`, and a `k8s.sustain.io/policy` annotation matching the policy that claimed the group. A ReplicaSet-owned pod that carries the mirrored `owner-name` label is therefore never a member, and a pod opted into a different policy is logged and skipped.

A container with `resizePolicy: RestartContainer` for memory restarts on a memory resize, which for a task means losing in-flight work (see [Caveats](#caveats)). Use `pod: OnCreate` if that is not acceptable. See [Standalone Pods & Identity Grouping](../guides/standalone-pods-and-grouping.md).

## Caveats

- **Any resize can be deferred or infeasible.** CPU and memory, up or down: the kubelet reports `Deferred` while the node lacks free capacity and `Infeasible` when the request exceeds what the node can ever offer. `Deferred` resizes are left to the kubelet; `Infeasible` ones fall back to eviction (except for the never-evicted kinds).
- **Memory limit decreases** only complete once the container's usage is below the new limit; until then the resize stays in progress.
- **`resizePolicy: RestartContainer`** on a container restarts it for that resource's resize. For Job, CronJob and bare pods that can discard in-flight work.
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
