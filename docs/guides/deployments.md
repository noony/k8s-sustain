# Deployments & StatefulSets

## Ongoing mode with eviction (k8s < 1.31)

The controller evicts stale pods via the Eviction API. The workload controller (Deployment/StatefulSet) creates replacement pods, and the webhook injects the latest recommendations at creation time. PodDisruptionBudgets are respected.

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
  rightSizing:
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

k8s-sustain works seamlessly alongside HPAs. Recommendations are based on workload-level Prometheus signals — the sum of CPU/memory across all replicas — so HPA scaling in or out does not perturb the recommendation. No autoscaler object (HPA or KEDA ScaledObject) is ever modified.

See the [KEDA guide](keda.md) for details on KEDA integration and scale-to-zero handling.
