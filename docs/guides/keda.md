# KEDA Integration

k8s-sustain detects [KEDA](https://keda.sh) `ScaledObject`s automatically. With [autoscaler coordination](../concepts/autoscaler-coordination.md) enabled on the Policy, it also shapes requests so the autoscaler's utilization signal stays meaningful.

## Walkthrough

Opt in the workload as shown in the [Quick Start](../getting-started/quick-start.md#3-opt-in-a-deployment) (annotation `k8s.sustain.io/policy: production-rightsizing`), then define its `ScaledObject`:

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

Enable the kind and coordination on the Policy:

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: production-rightsizing
spec:
  rightSizing:
    update:
      types:
        deployment: Ongoing
    autoscalerCoordination:
      enabled: true
    resourcesConfigs:
      cpu:    { window: 168h, requests: { percentile: 95, headroom: 10 } }
      memory: { window: 168h, requests: { percentile: 95, headroom: 20 } }
```

See [Autoscaler Coordination](../concepts/autoscaler-coordination.md) for the formula and detection rules.

## Verification

Each reconcile that finds an autoscaler emits an `AutoscalerDetected` event on the workload:

```bash
kubectl get events -n example --field-selector reason=AutoscalerDetected
```

With coordination enabled, the `k8s_sustain_coordination_factor` metric reports the applied multiplier (`kind="overhead"`) for the workload.

## Notes

- **Per-pod signal.** k8s-sustain sizes from the busiest replica's per-pod percentile (`max by` across pods), not a sum or per-pod average, so KEDA scaling 3 → 6 pods does not change the recommendation.
- **HPA + ScaledObject co-existence.** KEDA manages an HPA on behalf of each `ScaledObject`. When both target the same workload, the `ScaledObject` is canonical and the HPA is ignored.
- **Scale-to-zero.** When the workload scales to 0, the recording rule produces no samples for the idle period and the percentile is computed from the periods the workload was running.
- **CRD-absent behaviour.** If the KEDA CRD is not installed, the `ScaledObject` lookup is skipped silently and only HPAs are detected.
- **Read-only.** k8s-sustain never modifies an HPA or a `ScaledObject`.
