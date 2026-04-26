# KEDA Integration

k8s-sustain works alongside [KEDA](https://keda.sh) without any configuration. Workloads scaled by a KEDA ScaledObject are right-sized just like HPA-managed workloads, with no risk of the right-sizer fighting KEDA's scaling decisions.

## How it works

The right-sizer reads workload-level Prometheus metrics — the **sum** of CPU/memory across all replicas of a workload — rather than per-pod averages. Because the sum is invariant to replica count, KEDA scaling out from 3 → 6 pods does not change the recommendation. The right-sizer never patches HPAs or ScaledObjects.

When both an HPA and a ScaledObject target the same workload (the normal KEDA setup, since KEDA reconciles a ScaledObject into an HPA), the ScaledObject is treated as canonical. The HPA is read-only.

For workloads using KEDA scale-to-zero, the right-sizer falls back to the ScaledObject's `minReplicaCount` (or 1) when computing the per-pod recommendation, so we never divide by zero during quiet windows.

## Configuration

None required. Detection is automatic.

## Requirements

The KEDA `ScaledObject` CRD does not need to be installed. When absent, k8s-sustain logs an informational message at startup and falls back to HPA-only detection.
