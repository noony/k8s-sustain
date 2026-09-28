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
  same name, so the value must also be a valid label value (RFC 1123, 63
  characters or fewer).
- kube-state-metrics (configured via `metricLabelsAllowlist` in the chart's
  `values.yaml`) exposes that label in `kube_pod_labels`, which a recording
  rule folds into `k8s_sustain:pod_workload`. Without it the override has no
  Prometheus data to query.

An invalid value is treated as absent: no label is set, no override applies,
and the pod is never rejected.

## Bare pods (Airflow example)

A Policy opts bare pods in via `rightSizing.update.types.pod`:

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

Set both annotations on the Pod the task launches (via
`KubernetesPodOperator`'s `annotations` parameter):

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
aggregates under the single identity `Pod/etl-daily` in the `airflow`
namespace, and the webhook injects resources at admission.

**Bare pods are never evicted, in any mode** — nothing would recreate them.
With `pod: Ongoing`, the controller resizes the identity's running pods in
place where the cluster supports it; otherwise the recommendation only reaches
the next pod through the webhook. `KubernetesPodOperator` creates
`restartPolicy: Never` pods, which have their own version requirement for
in-place resize — see [Version matrix](../concepts/in-place-updates.md#version-matrix)
and [Bare pods](../concepts/in-place-updates.md#bare-pods).

An in-place **memory** resize can restart the container, which for an Airflow
task means losing in-flight work. The
[downsize threshold](../concepts/update-modes.md#downsize-threshold) bounds how
often it fires. If your tasks cannot tolerate a restart, use `pod: OnCreate`.

**Group membership.** A pod belongs to the identity when it has no controller
`ownerReference`, a valid `owner-name`, and a resolved policy (pod or Namespace
annotation — see [Resolution order](../reference/annotation.md#resolution-order))
matching the group's. A ReplicaSet-owned pod that merely carries the mirrored
`owner-name` label is never touched by this path.

**One owner-name, one policy.** The group is claimed by the policy named on the
first of its pods the controller sees. A pod sharing the
`(namespace, owner-name)` but naming a *different* policy is excluded from the
group: it never supplies containers and is never resized under the group's
recommendation. The controller logs it on every reconcile:

```text
bare pods share an owner-name identity but name a different policy; they are
excluded from the group and will not be rightsized under it
```

Fix it by giving those pods their own `owner-name`, or by aligning their
`k8s.sustain.io/policy` annotation with the rest of the group.

**Namespace scoping.** The grouping key is `namespace + owner-name`. The same
`owner-name` in two namespaces produces two separate identities; cross-namespace
grouping is not supported.

### Cold start and retention

A bare pod can start and finish between two reconciles. The first pod of a new
identity is admitted with its template resources and leaves a stub
`WorkloadRecommendation`, so the controller keeps recomputing the identity even
while no pod is running; later pods are injected once a recommendation exists.
A one-off pod that never returns does not converge, which is intended. See
[Cold start](../concepts/workload-recommendations.md#cold-start-stub-recommendations)
and [Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads).

For recurring tasks, keep the Policy `window` long relative to the task's
period (the default `168h` is). A window of a few minutes can mostly contain
the first seconds of runs and drive the recommendation to the floor; the
`recurring` scenario in [Local testing](local-testing.md#recurring) reproduces
this.

## Grouping owned workloads (blue/green example)

Set the same annotation on the pod template of each Deployment to group:

```yaml
spec:
  template:
    metadata:
      annotations:
        k8s.sustain.io/policy: my-policy
        k8s.sustain.io/owner-name: app
```

Both `app-blue` and `app-green` report into one `Deployment/app` identity — one
Prometheus aggregate and one `WorkloadRecommendation` (`deployment-app`).
The recommendation covers the **union** of the members' containers, so a
container only `app-green` declares is still sized. **Applying stays per real
Deployment**: each Deployment's own pods are resized or evicted independently,
against the shared recommendation narrowed to the containers that Deployment
declares.

**Keep the members' autoscaler state consistent.** If one member has an
HPA/ScaledObject and another has none (or a different kind), a single member's
autoscaler shapes the shared recommendation for all of them, which can over- or
under-provision the others. The controller reports this with a `V(1)` log and
`k8s_sustain_group_autoscaler_mismatch_total`; fix it by attaching or removing
the autoscaler on the other members.

## Dashboard

`Pod` is a regular workload kind in the dashboard: it appears in the workload
list and facet filters, and the simulator accepts `ownerKind: Pod`,
`ownerName: <owner-name value>`. Live pods are grouped by
`(namespace, owner-name)` the same way the controller groups them.
