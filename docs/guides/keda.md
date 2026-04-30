# KEDA Integration

k8s-sustain detects [KEDA](https://keda.sh) `ScaledObject`s automatically and shapes recommendations so the autoscaler's utilization signal stays meaningful — no configuration required.

## Goal

Run a workload scaled by a `ScaledObject` and confirm k8s-sustain's recommendation stays consistent across scaling events.

## Prerequisites

- KEDA installed in the cluster (the `ScaledObject` CRD is sufficient).
- A workload with a `k8s.sustain.io/policy` annotation on its pod template.
- A k8s-sustain `Policy` matching the workload (see [Installation](../getting-started/installation.md)).
- A Prometheus instance reachable from the controller.

## Walkthrough

### 1. Annotate the Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: example-app
  namespace: example
spec:
  replicas: 3
  selector: { matchLabels: { app: example-app } }
  template:
    metadata:
      labels: { app: example-app }
      annotations:
        k8s.sustain.io/policy: production-rightsizing
    spec:
      containers:
        - name: app
          image: nginx:1.27
          resources:
            requests: { cpu: 100m, memory: 256Mi }
```

### 2. Define a `ScaledObject`

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: example-app
  namespace: example
spec:
  scaleTargetRef:
    name: example-app
    kind: Deployment
  minReplicaCount: 1
  maxReplicaCount: 10
  triggers:
    - type: cpu
      metricType: Utilization
      metadata: { value: "70" }
```

### 3. Enable autoscaler coordination

To shape requests so KEDA's utilization signal stays meaningful, enable the overhead formula on the Policy:

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: production-rightsizing
spec:
  rightSizing:
    autoscalerCoordination:
      enabled: true
    resourcesConfigs:
      cpu:    { window: 168h, requests: { percentile: 95, headroom: 10 } }
      memory: { window: 168h, requests: { percentile: 95, headroom: 20 } }
```

See [Autoscaler Coordination](../concepts/autoscaler-coordination.md) for the formula and detection rules.

## Verification

Confirm the controller observed the `ScaledObject`:

```bash
kubectl logs -n k8s-sustain deploy/k8s-sustain-controller \
  | grep -E 'scaledObject|coordination'
```

The metric `k8s_sustain_coordination_factor` should report the applied multiplier (`kind="overhead"`) for the workload.

## Notes

- **Workload-level signal.** k8s-sustain queries the **sum** of CPU/memory across all replicas, not per-pod averages. The sum is invariant to replica count, so KEDA scaling 3 → 6 pods does not change the recommendation.
- **HPA + ScaledObject co-existence.** KEDA itself manages an HPA on behalf of each `ScaledObject`. When k8s-sustain finds both targeting the same workload, the `ScaledObject` is canonical and the HPA is ignored for autoscaler-coordination purposes.
- **Scale-to-zero.** When `minReplicaCount: 0` is configured and the current replica count is 0, the per-pod division falls back to `max(1, minReplicaCount)` so the recommendation does not divide by zero during quiet windows.
- **CRD-absent behavior.** If the KEDA CRD is not installed, the `ScaledObject` lookup returns no match silently and recommendations proceed using HPA-only detection.
- **No HPA / ScaledObject patches.** k8s-sustain never modifies an HPA or a `ScaledObject`. Both are read-only inputs to the recommender.
