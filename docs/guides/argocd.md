# Argo CD Integration

When k8s-sustain runs in **Ongoing** mode it patches workload resource requests and limits directly on the cluster. Argo CD detects these changes as out-of-sync diffs because the live state no longer matches the manifests stored in Git.

To prevent Argo CD from flagging or reverting resource changes made by k8s-sustain, configure `ignoreDifferences` to ignore fields managed by the `k8s-sustain` field manager.

## Cluster-wide configuration (recommended)

The simplest approach is to tell Argo CD to ignore all fields owned by the `k8s-sustain` managed-fields manager. Add this to the `argocd-cm` ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: argocd-cm
  namespace: argocd
data:
  resource.customizations.ignoreDifferences.all: |
    managedFieldsManagers:
      - k8s-sustain
```

This covers Deployments, StatefulSets, DaemonSets, and CronJobs — any resource that k8s-sustain patches — without needing to enumerate specific paths or kinds.

!!! tip
    This is the recommended approach. It automatically covers all current and future resource types managed by k8s-sustain and requires no maintenance when new workload kinds are added.

## Per-Application configuration

If you prefer to scope the exclusion to specific Applications rather than cluster-wide, add `ignoreDifferences` to each Application:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
spec:
  ignoreDifferences:
    - group: apps
      kind: Deployment
      managedFieldsManagers:
        - k8s-sustain
    - group: apps
      kind: StatefulSet
      managedFieldsManagers:
        - k8s-sustain
    - group: apps
      kind: DaemonSet
      managedFieldsManagers:
        - k8s-sustain
    - group: batch
      kind: CronJob
      managedFieldsManagers:
        - k8s-sustain
```

You can further restrict with `name` and `namespace` fields if needed.

## Self-heal and sync policy

If your Application has `selfHeal: true`, Argo CD will revert the resource changes made by k8s-sustain on each sync cycle — even if you configure `ignoreDifferences`. To prevent this, make sure `RespectIgnoreDifferences=true` is set in the sync options:

```yaml
spec:
  syncPolicy:
    automated:
      selfHeal: true
    syncOptions:
      - RespectIgnoreDifferences=true
```

## OnCreate mode

If you use only the **OnCreate** update mode, k8s-sustain never patches workload specs — it only mutates Pods at admission time via a webhook. In this case no `ignoreDifferences` configuration is needed because Argo CD tracks the workload spec, not the running Pod spec.
