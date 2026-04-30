# Argo CD Integration

k8s-sustain coexists with Argo CD GitOps without any `ignoreDifferences` configuration: k8s-sustain only mutates Pods (via the webhook) and recycles them (via the controller), and never touches the workload spec that Argo CD tracks.

## Goal

Run a workload under Argo CD GitOps with k8s-sustain right-sizing it, and confirm Argo CD remains in `Synced` state across reconcile cycles.

## Prerequisites

- An Argo CD installation managing the target namespace.
- A Git repository containing the workload manifest with a `k8s.sustain.io/policy` annotation on the pod template.
- A k8s-sustain `Policy` matching the workload (see [Installation](../getting-started/installation.md)).
- Read access to a Prometheus instance from the controller.

## Walkthrough

### 1. Annotate the workload's pod template in Git

```yaml
# apps/example-app/deployment.yaml in your gitops repo
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
            limits:   { cpu: 200m, memory: 512Mi }
```

### 2. Define the Argo CD `Application`

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: example-app
  namespace: argocd
spec:
  destination:
    namespace: example
    server: https://kubernetes.default.svc
  source:
    repoURL: https://example.com/gitops-repo.git
    path: apps/example-app
    targetRevision: main
  syncPolicy:
    automated: { selfHeal: true, prune: true }
```

Apply the `Application`. Argo CD syncs the Deployment.

### 3. Wait for a k8s-sustain reconcile cycle

Reconciles run on the `--reconcile-interval` (default `5m`). To accelerate a first observation, restart the controller pod or trigger a Policy update.

## Verification

After a reconcile cycle that recycles a pod, Argo CD must remain `Synced`:

```bash
argocd app get example-app -o json | jq '.status.sync.status'
```

Expected output: `"Synced"`.

The pod's resources reflect the recommendation:

```bash
kubectl get pods -n example -l app=example-app \
  -o yaml | yq '.items[].spec.containers[].resources'
```

The Deployment's pod template is **unchanged** from what is in Git:

```bash
kubectl get deploy example-app -n example \
  -o yaml | yq '.spec.template.spec.containers[].resources'
```

These two outputs differ — that is expected, since the webhook mutates pods but never the workload spec.

## Notes

- **No `ignoreDifferences` needed.** k8s-sustain never patches workload specs. The webhook intercepts `Pod CREATE` admission and injects resources into the resulting pod manifest; the controller recycles stale pods (in-place on Kubernetes 1.31+, eviction on older versions). Argo CD tracks workload specs, so there is no diff to ignore.
- **`selfHeal: true` is safe.** Since Argo CD never sees a diff caused by k8s-sustain, it has nothing to revert.
- **Argo Rollouts.** If you use Argo Rollouts (`Rollout` objects) instead of native Deployments, see the [Argo Rollouts guide](argo-rollouts.md).
