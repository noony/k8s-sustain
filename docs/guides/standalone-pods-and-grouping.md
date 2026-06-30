# Standalone Pods & Identity Grouping

The `k8s.sustain.io/owner-name` annotation lets a pod declare a logical
workload identity that overrides the one k8s-sustain would otherwise derive
from its `ownerReferences`. It covers two cases:

1. **Bare pods with no controller owner** — e.g. Airflow's
   `KubernetesPodOperator`, which by default launches a Pod directly with no
   parent Job.
2. **Grouping multiple owned workloads under one identity** — e.g. blue/green
   Deployments `app-blue` and `app-green` sharing one recommendation.

## How it works

Setting `k8s.sustain.io/owner-name: <value>` on a pod (directly, for bare
pods; or on a pod template, for owned workloads) does two things:

- The admission webhook mirrors the annotation onto a pod **label** of the
  same name at admission time, provided the value is also valid as a
  Kubernetes label value (RFC 1123, 63 characters or fewer — annotation values
  can be longer, but label values cannot, so pick a value that satisfies the
  stricter rule).
- kube-state-metrics (configured via `metricLabelsAllowlist` in the chart's
  `values.yaml`) exposes that label as `kube_pod_labels`, which a recording
  rule folds into the recommendation pipeline's `k8s_sustain:pod_workload`
  series. Without this, the override would have no Prometheus data to query
  — see the design rationale in
  `docs/superpowers/specs/2026-06-30-owner-name-annotation-design.md` if
  you're modifying this mechanism.

An invalid annotation value (fails label-value validation) is treated as
absent: no label is set, no override applies, and the pod is never rejected
— the webhook always fails open.

## Bare pods (Airflow example)

A Policy must opt bare pods in via `rightSizing.update.types.pod`, exactly
like every other kind:

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: airflow-tasks
spec:
  selector:
    namespaces: ["airflow"]
  rightSizing:
    update:
      types:
        pod: OnCreate
```

Configure your Airflow task to set both annotations on the Pod it launches
(via `KubernetesPodOperator`'s `annotations` parameter):

```python
KubernetesPodOperator(
    ...
    annotations={
        "k8s.sustain.io/policy": "airflow-tasks",
        "k8s.sustain.io/owner-name": "etl-daily",
    },
)
```

Every run of this task — each a separate Pod with a random name suffix —
aggregates under the single identity `Pod/etl-daily` within the `airflow`
namespace. The webhook injects resources at admission like any other
`OnCreate` kind.

**`Ongoing` mode never recycles bare pods.** There is no controller that
could recreate the pod after an eviction or in-place resize, so even with
`pod: Ongoing` configured, the controller computes and caches a
recommendation (useful for the webhook's Prometheus-outage fallback) but
never evicts or resizes the pod itself.

## Grouping owned workloads (blue/green example)

Set the same annotation on the pod template of each Deployment you want
grouped:

```yaml
spec:
  template:
    metadata:
      annotations:
        k8s.sustain.io/policy: my-policy
        k8s.sustain.io/owner-name: app
```

Both `app-blue` and `app-green` report into one shared `Deployment/app`
identity — one Prometheus query aggregate, one `WorkloadRecommendation`
object (`deployment-app`, not `deployment-app-blue` /
`deployment-app-green`). **Recycling stays per-real-Deployment**: each
Deployment's own pods are evicted or resized independently, using the shared
computed recommendation — grouping affects identity and data, not which pods
get touched.

## Namespace scoping

For bare pods, the grouping key is `namespace + owner-name`, not
`owner-name` alone. Two pods in different namespaces using the same
`owner-name` value produce two separate `WorkloadRecommendation` objects, one
per namespace. Cross-namespace grouping is not supported.

## Dashboard

`Pod` is a regular workload kind in the dashboard: it appears in the
workload list and facet filters, supports the simulator (`ownerKind: Pod`,
`ownerName: <owner-name value>`), and resolves recommendations the same way
as every other kind. Since a bare-pod identity has no single Kubernetes
object backing it, the dashboard groups live pods by `(namespace,
owner-name)` the same way the controller does (`workload.GroupBarePods`) —
the most recently created pod in the group supplies the displayed
containers and annotations.
