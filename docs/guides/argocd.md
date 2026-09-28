# Argo CD Integration

k8s-sustain coexists with Argo CD without any `ignoreDifferences` configuration: it sizes pods at admission and resizes or evicts running pods, but never touches the workload spec that Argo CD tracks.

## Walkthrough

Commit the workload to Git with the `k8s.sustain.io/policy` annotation (see the [Quick Start](../getting-started/quick-start.md#3-opt-in-a-deployment) for an example Deployment), and point an `Application` at it:

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

Policies themselves can be managed from Git too — see [Managing policies with Helm](managing-policies-with-helm.md#gitops-with-argo-cd).

## Verification

After a reconcile cycle that updates pods (default interval `5m`), the Application stays `Synced`:

```bash
argocd app get example-app -o json | jq '.status.sync.status'
```

The pods carry the recommendation while the Deployment in Git is unchanged — see [Verifying applied resources](../concepts/update-modes.md#verifying-applied-resources).

## Notes

- **`selfHeal: true` is safe.** Argo CD never sees a diff caused by k8s-sustain, so it has nothing to revert.
- **Argo Rollouts.** For `Rollout` objects, see the [Argo Rollouts guide](argo-rollouts.md).
- **Sync-wave hook Jobs.** A `PreSync`/`PostSync` hook Job annotated with `k8s.sustain.io/policy` is a standalone Job to k8s-sustain. Its first run is admitted with template resources and leaves a cold-start stub; later runs of the same Job name get the recommendation once it is computed — see [Cold start](../concepts/workload-recommendations.md#cold-start-stub-recommendations). Once the Job is deleted, its entry stays as an inactive row for the retention window — see [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads).
