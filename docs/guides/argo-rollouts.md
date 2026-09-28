# Argo Rollouts

k8s-sustain right-sizes workloads managed by [Argo Rollouts](https://argoproj.github.io/argo-rollouts/) (`Rollout` objects) the same way it handles native Deployments.

!!! note "Argo Rollouts vs Argo CD"
    This page is about the Argo Rollouts controller, which manages canary and blue-green deployment strategies. For GitOps with Argo CD, see the [Argo CD integration guide](argocd.md).

## Prerequisites

The Argo Rollouts CRD (`rollouts.argoproj.io`) must be installed before any Policy sets `argoRollout` (in either mode). The controller lists Rollouts for every such Policy, and if the CRD is missing that list fails and aborts the reconcile of the whole Policy, including its other kinds.

## Walkthrough

Annotate the Rollout (pod template, its own `metadata.annotations`, or its Namespace — see [Resolution order](../reference/annotation.md#resolution-order)):

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

Enable `argoRollout` on the Policy:

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

## Verification

After a reconcile, check the `ResourcesUpdated` events on the Rollout and the pods' resources — see [Verifying applied resources](../concepts/update-modes.md#verifying-applied-resources). The Rollout's pod template is never changed.

```bash
kubectl get events -n example --field-selector involvedObject.name=example-app,reason=ResourcesUpdated
```

## Notes

- **Modes.** The webhook resolves `Pod → ReplicaSet → Rollout` and injects the recommendation at admission, so `argoRollout: OnCreate` works without extra configuration. `Ongoing` additionally updates running pods like a Deployment — see [Deployments](deployments.md#what-happens-on-each-reconcile).
- **Canary and blue-green.** k8s-sustain has no awareness of Rollout steps or pauses. Every pod owned (via a ReplicaSet) by the Rollout is a candidate, stable and canary alike, and a paused Rollout is treated like any other.
- **Analysis runs.** Right-sizing changes only pod resources, never the Rollout spec, so analysis runs behave the same as without k8s-sustain.
- **RBAC.** The controller's ClusterRole grants read-only access (`get`, `list`, `watch`) to `argoproj.io/rollouts`.
