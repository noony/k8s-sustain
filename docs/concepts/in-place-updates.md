# In-Place Updates

Kubernetes 1.27 introduced the `InPlacePodVerticalScaling` feature gate (alpha), which became beta (on by default) in Kubernetes 1.29. It allows changing a pod's resource requests and limits **without restarting the container**.

k8s-sustain automatically detects whether the cluster supports this feature and enables it when available.

## How it works

When `Ongoing` mode is active and the cluster supports in-place updates:

1. The controller patches the workload's pod template with new resources
2. It then lists all running, non-terminating pods matched by the workload's selector
3. For each pod, it checks the pod's `status.resize` field:
   - **`Infeasible`**: the node cannot satisfy the request — the pod is evicted so the scheduler can place the replacement elsewhere
   - **`Deferred`**: the kubelet accepted the request but is waiting on conditions (e.g. a memory decrease that requires container restart) — skipped; the kubelet will apply it without further action
   - **`InProgress` / not set**: proceeds to patch `spec.containers[*].resources` directly
4. The kubelet applies the new resources without restarting the container

Pods that are terminating or not in `Running` phase are skipped.

## Cluster version detection

The operator detects the server version at startup using the discovery API:

```
major=1, minor≥27 → in-place updates enabled
```

You can check whether it is enabled in the controller logs:

```
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
