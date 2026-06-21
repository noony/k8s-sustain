# Managing Policies with Helm

The `k8s-sustain-policies` chart renders [Policy](../reference/policy.md) objects
from a values list, so you can version and roll them out with Helm or GitOps
tools such as Argo CD and Flux. It is separate from the operator chart: the
operator (and its CRDs) is installed by the `k8s-sustain` chart, while this chart
manages only `Policy` custom resources.

!!! note
    `Policy` objects are **cluster-scoped**. Their `name` is used verbatim, so
    names must be unique across every release of this chart to avoid collisions.

## Prerequisites

- The `k8s-sustain` operator chart is installed (it provides the `Policy` CRD).

## Defining policies

Each entry in `policies` becomes one `Policy`. The `spec` maps 1:1 to the
[Policy CRD spec](../reference/policy.md) and is passed through unchanged.

```yaml
# policies-values.yaml
policies:
  - name: staging-rightsizing
    labels:
      team: platform
    spec:
      selector:
        namespaces: [staging]
      rightSizing:
        update:
          types:
            deployment: Ongoing
        resourcesConfigs:
          cpu:
            requests: { percentile: 95, headroom: 10 }
          memory:
            requests: { percentile: 95, headroom: 20 }
```

Install or upgrade:

```bash
helm upgrade --install k8s-sustain-policies \
  charts/k8s-sustain-policies \
  -f policies-values.yaml
```

Verify:

```bash
kubectl get policies.k8s.sustain.io
```

## GitOps with Argo CD

Run one Argo CD `Application` for the operator and another for the policies, so
app teams can change policies without redeploying the operator:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: k8s-sustain-policies
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/your-org/your-config.git
    path: charts/k8s-sustain-policies
    targetRevision: main
    helm:
      valueFiles:
        - ../../envs/staging/policies-values.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: k8s-sustain
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
```

Because Policies are cluster-scoped, keep all of a cluster's policies in a single
release (or carefully partition names) to keep Argo CD pruning predictable.
