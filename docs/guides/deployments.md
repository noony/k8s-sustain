# Deployments & StatefulSets

k8s-sustain right-sizes Deployments and StatefulSets uniformly: the controller recycles stale pods (in-place on Kubernetes 1.31+, eviction on older versions) and the webhook injects fresh recommendations into replacement pods.

## Goal

Right-size a Deployment (or StatefulSet) in `Ongoing` mode without disrupting running traffic.

## Prerequisites

- A Deployment or StatefulSet with a `k8s.sustain.io/policy` annotation on its pod template.
- A k8s-sustain `Policy` matching the workload (see [Installation](../getting-started/installation.md)).
- A Prometheus instance reachable from the controller.

## Walkthrough

### 1. Annotate the workload

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
        k8s.sustain.io/policy: web-rightsizing
    spec:
      containers:
        - name: app
          image: nginx:1.27
          resources:
            requests: { cpu: 100m, memory: 256Mi }
            limits:   { cpu: 200m, memory: 512Mi }
```

### 2. Apply the Policy

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
        requests: { percentile: 95, headroom: 10 }
        limits:   { keepLimitRequestRatio: true }
      memory:
        window: 168h
        requests: { percentile: 95, headroom: 20 }
        limits:   { keepLimitRequestRatio: true }
```

## Verification

After a reconcile cycle, inspect a running pod:

```bash
kubectl get pods -n example -l app=example-app \
  -o yaml | yq '.items[0].spec.containers[].resources'
```

The Deployment's pod template stays unchanged:

```bash
kubectl get deploy example-app -n example \
  -o yaml | yq '.spec.template.spec.containers[].resources'
```

These differ — the webhook mutates pods at admission, not the workload spec.

## Notes

- **`Ongoing` vs `OnCreate`.** `Ongoing` keeps running pods aligned with the latest recommendation by recycling them on drift. `OnCreate` only injects at admission and lets the controller leave running pods alone. See [Update Modes](../concepts/update-modes.md).
- **In-place vs eviction.** On Kubernetes 1.31+ the controller patches running pods in place; on older versions it falls back to PDB-respecting eviction. See [In-Place Updates](../concepts/in-place-updates.md).
- **Pinned containers.** If a container already has a non-zero CPU request when the webhook intercepts the pod, k8s-sustain leaves it unchanged. Use this to pin specific sidecars while letting the main container be managed.
- **Combining with HPA.** Recommendations are computed from the workload-level Prometheus signal (sum across replicas), so HPA scale-out does not perturb the recommendation. To shape requests so the HPA's utilization target stays meaningful, enable [Autoscaler Coordination](../concepts/autoscaler-coordination.md). See also the [KEDA guide](keda.md).
- **Headroom suggestions.**

  | Workload type | CPU headroom | Memory headroom |
  |---------------|-------------|----------------|
  | Web/API servers | 10–20% | 20–30% |
  | Batch workers | 5–10% | 10–15% |
  | Memory-intensive | 5% | 30–50% |
  | CPU-burst workloads | 20–30% | 10% |
