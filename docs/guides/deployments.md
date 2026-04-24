# Deployments & StatefulSets

## Ongoing mode with eviction (k8s < 1.31)

The controller evicts stale pods via the Eviction API. The workload controller (Deployment/StatefulSet) creates replacement pods, and the webhook injects the latest recommendations at creation time. PodDisruptionBudgets are respected.

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: web-rightsizing
spec:
  update:
    types:
      deployment: Ongoing
      statefulSet: Ongoing
  rightSizing:
    resourcesConfigs:
      cpu:
        window: 168h
        requests:
          percentile: 95
          headroom: 10
        limits:
          keepLimitRequestRatio: true
      memory:
        window: 168h
        requests:
          percentile: 95
          headroom: 20
        limits:
          keepLimitRequestRatio: true
```

Opt in your Deployment:

```yaml
spec:
  template:
    metadata:
      annotations:
        k8s.sustain.io/policy: web-rightsizing
```

## Ongoing mode with in-place updates (k8s ≥ 1.31)

No additional configuration is needed. The operator detects the cluster version at startup and automatically uses in-place pod patching instead of eviction.

## OnCreate mode — set resources once, don't touch again

Use `OnCreate` if you want to inject a good initial resource profile when pods are first created, but don't want the controller to continuously update them. Existing running pods are not affected.

```yaml
spec:
  update:
    types:
      deployment: OnCreate
```

## Protecting specific containers

If a container already has a non-zero CPU request set when the webhook intercepts the pod, k8s-sustain will not overwrite it. This allows you to manually pin resources on specific containers while letting others be managed.

## Headroom recommendations

| Workload type | CPU headroom | Memory headroom |
|---------------|-------------|----------------|
| Web/API servers | 10–20% | 20–30% |
| Batch workers | 5–10% | 10–15% |
| Memory-intensive | 5% | 30–50% |
| CPU-burst workloads | 20–30% | 10% |

## Combining with HPA

k8s-sustain right-sizes **requests**, which HPA uses to compute utilization. By default, k8s-sustain is **HPA-aware** and automatically adjusts its recommendations so HPA scaling decisions remain correct.

### How it works

HPA computes: `utilization = actual_usage / requests`. If k8s-sustain changes requests, utilization shifts — even though load hasn't changed. The `HpaAware` mode (default) factors the HPA's target utilization into the recommendation formula:

```
request = observed_usage × (1 + headroom) / (hpa_target / 100)
```

This means k8s-sustain sets requests to a value where HPA's utilization math produces the correct scaling behavior.

**Example** (p95 = 400m, headroom = 20%, HPA target = 80%):

| Scenario | Requests | Utilization | HPA reaction |
|---|---|---|---|
| Before k8s-sustain | 1000m | 40% | wants to scale down |
| Without HPA awareness | 480m | 83% | scales up (wrong) |
| With HPA awareness | 600m | 67% | calm (correct) |

### Configuration

```yaml
spec:
  rightSizing:
    updatePolicy:
      hpa:
        mode: HpaAware      # default — auto-detect and adjust
        cpu:
          targetUtilizationOverride: 75  # optional, overrides auto-detected value
```

### Modes

- **`HpaAware`** (default) — Auto-detect HPAs, adjust recommendation formula. No HPA objects are modified.
- **`UpdateTargetValue`** — Convert HPA metrics from percentage-based (`Utilization`) to absolute (`AverageValue`), fully decoupling the two systems. The HPA object is modified.
- **`Ignore`** — No HPA awareness. Use this only if your HPA scales on custom metrics (queue depth, requests/sec) that are unaffected by resource changes.

### KEDA

When using KEDA, k8s-sustain auto-detects that the HPA is managed by a ScaledObject. `HpaAware` mode (default) works seamlessly with KEDA. See the [KEDA guide](keda.md) for details.
