# Argo Rollouts

k8s-sustain right-sizes workloads managed by [Argo Rollouts](https://argoproj.github.io/argo-rollouts/) (`Rollout` objects) the same way it handles native Deployments and StatefulSets.

!!! note "Argo Rollouts vs Argo CD"
    This page is about the Argo Rollouts controller, which manages canary and blue-green deployment strategies. For GitOps with Argo CD, see the [Argo CD integration guide](argocd.md).

## Goal

Apply right-sizing to a `Rollout` workload in `Ongoing` mode.

## Prerequisites

- Argo Rollouts CRD (`rollouts.argoproj.io/v1alpha1`) installed in the cluster. k8s-sustain unconditionally registers the `Rollout` type into its scheme at startup, but the controller's reconcile loop will fail when listing rollouts if the CRD is not present and a Policy enables `argoRollout: Ongoing`.
- A Prometheus instance reachable from the controller.
- k8s-sustain installed (see [Installation](../getting-started/installation.md)).

## Walkthrough

### 1. Annotate the Rollout's pod template

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata:
  name: example-app
  namespace: example
spec:
  replicas: 3
  selector:
    matchLabels:
      app: example-app
  template:
    metadata:
      labels:
        app: example-app
      annotations:
        k8s.sustain.io/policy: production-rightsizing
    spec:
      containers:
        - name: app
          image: nginx:1.27
          resources:
            requests: { cpu: 100m, memory: 256Mi }
            limits:   { cpu: 200m, memory: 512Mi }
  strategy:
    canary:
      steps:
        - setWeight: 25
        - pause: { duration: 60s }
        - setWeight: 100
```

### 2. Apply a Policy that includes `argoRollout`

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: production-rightsizing
spec:
  rightSizing:
    update:
      types:
        argoRollout: Ongoing
    resourcesConfigs:
      cpu:    { window: 168h, requests: { percentile: 95, headroom: 10 } }
      memory: { window: 168h, requests: { percentile: 95, headroom: 20 } }
```

### 3. Watch a reconcile cycle

```bash
kubectl logs -n k8s-sustain deploy/k8s-sustain-controller -f
```

Expected log lines: `recommendation computed`, `recycling pods` for the matching ReplicaSet of the Rollout.

## Verification

```bash
kubectl get rollout example-app -n example -o yaml | yq '.spec.template.spec.containers[].resources'
```

Expected: the values in the Rollout's pod template are **unchanged** (k8s-sustain never patches the workload spec). The injected requests appear on the running pods only:

```bash
kubectl get pods -n example -l app=example-app -o yaml | yq '.items[].spec.containers[].resources'
```

## Notes

- **`Ongoing` mode only.** Right-sizing for `Rollout` workloads is currently supported only in `Ongoing` mode. The admission webhook does not walk the `Pod → ReplicaSet → Rollout` owner chain (it walks `Pod → ReplicaSet → Deployment`), so `OnCreate` does not inject resources into Rollout-owned pods. On Kubernetes 1.31+, `Ongoing` updates the running pods in place; on older versions the controller falls back to eviction and replacement pods come up with the resources defined in the Rollout's pod template (the webhook does not patch them).
- **Canary and blue-green steps.** k8s-sustain operates on the pods currently owned by the active ReplicaSet. A paused Rollout is not perturbed: the controller recycles stale pods only when their requests drift outside the policy's clamps.
- **Analysis runs.** Right-sizing changes affect only pod resources, never the Rollout spec, so analysis runs behave the same as without k8s-sustain.
- **RBAC.** The controller's ClusterRole includes `argoproj.io/rollouts` with `get`, `list`, `watch` verbs (read-only).
