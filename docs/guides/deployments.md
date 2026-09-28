# Deployments, StatefulSets & DaemonSets

k8s-sustain right-sizes Deployments, StatefulSets and DaemonSets through the same code path: the webhook injects the recommendation into every new pod, and in `Ongoing` mode the controller also brings running pods up to date. The workload spec is never patched.

## Opt in

Annotate the workload as shown in the [Quick Start](../getting-started/quick-start.md#3-opt-in-a-deployment), then enable the kinds on a Policy:

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: web-rightsizing
spec:
  rightSizing:
    update:
      types:
        deployment: Ongoing
        statefulSet: Ongoing
        daemonSet: Ongoing
    resourcesConfigs:
      cpu:
        window: 168h
        requests: { percentile: 95, headroom: 10 }
        limits:   { keepLimitRequestRatio: true }
      memory:
        window: 168h
        requests: { percentile: 95, headroom: 20 }
        limits:   { keepLimitRequestRatio: true }
```

Both the annotation and the Policy's selector must match the workload — see [Resolution order](../reference/annotation.md#resolution-order).

## What happens on each reconcile

Every `--reconcile-interval` (default `5m`) the controller recomputes the recommendation and, in `Ongoing` mode, updates the pods that are not yet running it:

- Running pods are resized in place where the cluster supports it, otherwise evicted so the webhook sizes the replacement — see [Version matrix](../concepts/in-place-updates.md#version-matrix) and [Eviction fallback](../concepts/in-place-updates.md#eviction-fallback).
- Evictions go one pod at a time and respect PDBs and the other [eviction safeguards](../concepts/update-modes.md#eviction-safeguards).
- Small request decreases are suppressed by the [downsize threshold](../concepts/update-modes.md#downsize-threshold); increases always apply.

`OnCreate` skips the recycling step: running pods keep their resources until the next rollout. To check the result, see [Verifying applied resources](../concepts/update-modes.md#verifying-applied-resources).

## Notes

- **Keeping existing requests.** There is no per-container pinning. `requests.keepRequest: true` on `cpu` or `memory` leaves that request untouched on every container the Policy manages; use a separate Policy for workloads that need it. See the [Policy reference](../reference/policy.md).
- **Combining with HPA.** Recommendations are computed from the busiest replica's per-pod percentile (`max by` across pods), which is invariant to replica count, so HPA scale-out does not perturb the recommendation. To shape requests so the HPA's utilization target stays meaningful, enable [Autoscaler Coordination](../concepts/autoscaler-coordination.md). See also the [KEDA guide](keda.md).
- **DaemonSets and `updateStrategy: OnDelete`.** Evicting a pod deletes it, so the DaemonSet controller recreates it with the webhook-injected resources, whichever update strategy is set.
- **Node agents.** Log shippers, CNI plugins and node exporters run on every node and are disruptive to OOM-kill; use a higher percentile (p99) and generous memory headroom for them.
- **Headroom suggestions.**

  | Workload type | CPU headroom | Memory headroom |
  |---------------|-------------|----------------|
  | Web/API servers | 10–20% | 20–30% |
  | Batch workers | 5–10% | 10–15% |
  | Memory-intensive | 5% | 30–50% |
  | CPU-burst workloads | 20–30% | 10% |
  | Node agents (DaemonSets) | 15% | 25%+ |
