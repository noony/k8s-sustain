# In-Place Updates

Kubernetes 1.27 introduced the `InPlacePodVerticalScaling` feature gate (alpha), which became beta (on by default) in Kubernetes 1.31 and GA in Kubernetes 1.33. It allows changing a pod's resource requests and limits **without restarting the container**.

k8s-sustain automatically detects whether the cluster supports this feature and enables it when available.

## How it works

When `Ongoing` mode is active and the cluster supports in-place updates:

1. The controller lists all running, non-terminating pods matched by the workload's selector
2. For each pod, it checks the pod's `status.resize` field:
   - **`Infeasible`**: the node cannot satisfy the request — the pod is evicted so the scheduler can place the replacement elsewhere (the webhook injects the latest resources into the new pod)
   - **`Deferred`**: the kubelet accepted the request but is waiting on conditions (e.g. a memory decrease that requires container restart) — skipped; the kubelet will apply it without further action
   - **`InProgress` / not set**: proceeds to patch `spec.containers[*].resources` via the pod's `/resize` subresource
3. The kubelet applies the new resources without restarting the container

On Kubernetes 1.33+, pod resource changes go through the `/resize` subresource. On Kubernetes 1.31-1.32, the controller falls back to a direct pod patch.

Pods that are terminating or not in `Running` phase are skipped.

## Automatic fallback

If the API server rejects an in-place pod patch (e.g. the `InPlacePodVerticalScaling` feature gate is disabled), the controller automatically falls back to PDB-respecting eviction-based updates for the rest of the reconcile cycle. No manual intervention is needed.

## Cluster version detection

The controller detects the server version at startup using the discovery API:

```text
major=1, minor>=31 → in-place updates enabled
```

The feature gate was alpha (disabled by default) in Kubernetes 1.27-1.30. The controller requires >= 1.31 where it is beta and enabled by default.

You can check whether it is enabled in the controller logs:

```text
INFO  InPlacePodVerticalScaling support  enabled=true
```

## Caveats

- **CPU is always resizable in-place.** Memory resize may require a container restart if the requested memory exceeds the current cgroup limit. The kubelet handles this transparently.
- **VPA conflicts.** If you run Vertical Pod Autoscaler alongside k8s-sustain, ensure they do not target the same pods to avoid conflicting patches.
- **Resource resize status.** You can inspect the resize status on a pod:
  ```bash
  kubectl get pod my-pod -o jsonpath='{.status.containerStatuses[*].resources}'
  ```

## Disabling in-place updates

To force eviction-based behavior even on supported clusters, you can disable the feature by overriding the detection at deploy time. This is not exposed as a Helm value today — file a GitHub issue if you need it.
