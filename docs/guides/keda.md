# KEDA Integration

k8s-sustain works alongside [KEDA](https://keda.sh) (Kubernetes Event-Driven Autoscaling). KEDA creates and manages HPA objects based on ScaledObject definitions.

## How it works

When k8s-sustain detects an HPA targeting a workload, it checks if the HPA is owned by a KEDA ScaledObject (via `ownerReferences`). This affects behavior in `UpdateTargetValue` mode:

- **Native HPA** — k8s-sustain patches the HPA directly
- **KEDA-managed HPA** — `UpdateTargetValue` mode is not yet supported for KEDA-managed HPAs. k8s-sustain detects the KEDA ownership and emits a warning event. Use `HpaAware` mode (default) instead, which works correctly with KEDA without modifying any objects.

In `HpaAware` mode (default), no objects are modified regardless of whether KEDA manages the HPA — the recommendation formula is adjusted internally.

## Configuration

No special configuration is needed. k8s-sustain auto-detects KEDA ownership.

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: keda-workloads
spec:
  rightSizing:
    updatePolicy:
      hpa:
        mode: HpaAware  # recommended for KEDA workloads
  update:
    types:
      deployment: Ongoing
```

## Custom metrics

If your KEDA ScaledObject uses only custom or external metrics (e.g., queue depth, Kafka lag), there is no conflict with k8s-sustain. The `HpaAware` mode detects this and applies no adjustment — right-sizing proceeds normally.

## Requirements

KEDA ScaledObject support requires the KEDA CRD to be installed. If the CRD is not present, k8s-sustain gracefully skips ScaledObject detection and logs an informational message at startup.
