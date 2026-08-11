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

**Bare pods are never evicted, in any mode.** There is no controller that
could recreate the pod, so eviction would simply destroy the workload.

**`Ongoing` bare pods are resized in place.** Eviction needs a controller
behind it; an in-place resize does not. So with `pod: Ongoing` on a cluster
that supports `InPlacePodVerticalScaling` (k8s ≥ 1.33), the controller
corrects the running pods of the identity directly through the `pods/resize`
subresource — otherwise a long-running Airflow task would stay on whatever it
was admitted with for its entire life. Below 1.33 there is no in-place
support, so nothing is applied to a running bare pod and the recommendation
only reaches the next pod through the webhook.

**Mind the `restartPolicy` caveat.** k8s 1.33 and 1.34 do not cover pods whose
`restartPolicy` is `Never` or `OnFailure`; full coverage of those arrives in
**k8s ≥ 1.35**. This is not a footnote for Airflow readers:
`KubernetesPodOperator` creates `restartPolicy: Never` pods by default, so on a
1.33/1.34 cluster the resize is rejected per pod and the running task keeps its
admitted resources. The rejection is logged and skipped — it never falls back
to eviction — and the next pod of the identity is still injected correctly at
admission. This is the same version caveat that applies to
[Job and CronJob pods](jobs-and-cronjobs.md), which run with the same
`restartPolicy` values.

Which pods count as "the running pods of the identity" is decided by the same
grouping rule the controller uses to discover them: no controller
`ownerReference`, a valid `owner-name`, matching policy annotation. A pod that
merely carries the mirrored `owner-name` **label** but belongs to a
ReplicaSet is not a member and is never touched by this path — that is the
bare-pod counterpart of the ownerRef check protecting every other kind.

**One owner-name, one policy.** The group is claimed by the policy named on the
first of its pods the controller sees. A pod that shares the
`(namespace, owner-name)` but names a *different* policy in
`k8s.sustain.io/policy` is excluded from the group entirely: it is not a member,
it never supplies the group's containers, and it is never resized under the
group's recommendation. Both pods map to the same `WorkloadRecommendation` name,
so there is no second identity for the odd pod to belong to — this is a
configuration conflict, not a supported layout. The controller logs it on every
reconcile:

```text
bare pods share an owner-name identity but name a different policy; they are
excluded from the group and will not be rightsized under it
```

Fix it by giving those pods their own `k8s.sustain.io/owner-name`, or by
aligning their `k8s.sustain.io/policy` annotation with the rest of the group.

One tradeoff to be aware of: an in-place **memory** resize can restart the
container, which for an Airflow task means losing in-flight work. This is not
specific to bare pods — `Job` and `CronJob` already accept it, on the same
reasoning that resizing the running pod is the only way to correct it after
creation. Downsize suppression (`downsizeThreshold`) bounds how often it can
fire. If your tasks cannot tolerate a restart, use `pod: OnCreate`: the
recommendation is still computed and cached, and the webhook still injects it
into the next pod of the identity.

### Keep the window long relative to the duty cycle

A recurring bare-pod identity is only alive for part of the wall clock — an
Airflow task that runs for 30 seconds every two minutes is running about a
quarter of the time. Two settings interact with that, and it is worth knowing
which one protects you.

The 10-minute age gate does **not**: it is a floor on how long k8s-sustain has
known the identity, not on how much data it has, so a duty-cycled workload
clears it with proportionally less usage history than a continuously-running
one. The `window` is what protects you, because a window long relative to the
duty cycle still spans many complete runs.

The failure mode when it does not: if the window is short enough that it
frequently contains only the first seconds of a run — the part where the CPU
rate rule has not stabilised — the percentile lands near zero, the hard floors
(1m CPU, 1Mi memory) take over, and the identity gets a near-floor
recommendation that a real run cannot fit in.

**This is not reachable on a default install.** The default `window` is `168h`
(7 days), which covers thousands of runs of any realistic recurring task; you
would have to configure a window measured in minutes to see it. Where it does
show up is in short-window experiments — the `recurring` local-testing
scenario reproduces the dramatic, OOM-killing form deliberately at
`WINDOW=2m`, but the same identity also misbehaves at the harness *default*
`WINDOW=10m`: CPU floors to the 1m minimum and stays pinned there for the
whole observation window, with no self-heal, even though memory stays
realistic and no pods are killed at that window. If you deliberately shorten
`window` for a recurring workload, size it against the workload's *period*,
not its runtime.

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

Because the group has one `WorkloadRecommendation`, it also has one
`status.observedResources` snapshot and one recommendation in
`status.containers`, and **both are computed once per identity rather than once
per member**. The snapshot is the **union** of the members' containers, so a
container only `app-green` declares is still sized and still recommended; where
both members declare the same container name under different requests/limits (a
group mid-migration), the entry from the member whose `kind/namespace/name`
sorts first is kept whole, rather than mixing one member's request with
another's limit. The recommendation is then computed against that union, using
the autoscaler of the first member (in the same sort order) that has one, and
treating the identity as being as old as its oldest member.

**If members of an owner-name group differ in autoscaler state — one has an
HPA and a sibling doesn't, or they're governed by different autoscaler kinds —
the first sorted member's autoscaler governs the shared recommendation for
every member**, including the ones it doesn't actually target. This is a
misconfiguration, not a supported migration state (unlike the container-set
union above, which is expected to be transient): a member with no autoscaler
of its own can silently inherit a value shaped by an HPA that has no view of
it, over- or under-provisioning it depending on the coordination factor. The
controller detects and reports this — a `V(1)` log naming the identity and the
disagreeing members, plus `k8s_sustain_group_autoscaler_mismatch_total` — but
does not change the selection; fixing it means making the group's autoscaler
state consistent, typically by attaching (or removing) an HPA/ScaledObject on
the other members.

Every one of those choices is decided by the members' own names or by an
aggregate over all of them, so the stored snapshot and the stored recommendation
do not depend on the order the API server lists the members in, nor on which
member's work finishes first. Discovery and the computation phase both write
`status.observedResources` (discovery via `EnsureExists`, computation via
`wlrcache.Upsert`), but always the same value — the merged snapshot computed
once per identity — so the two writers can never disagree, and a group whose
members and metrics are unchanged therefore costs **no status write at all** on
subsequent reconciles, rather than one write per member per cycle.

Applying is still per real Deployment: each member aligns its own pods against
that one shared recommendation, narrowed to the containers that member actually
declares.

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

A bare pod like the Airflow example above can start and finish entirely
between two controller reconciles, so the controller's periodic pass may never
catch that identity alive — and between runs the group appears in no workload
listing at all. Cold start covers this: the webhook computes nothing, but when
it admits a pod for an identity that has no `WorkloadRecommendation`, it
creates an empty **stub** and records the admitted pod's container set on it.
From that moment the identity is in the controller's work-list and is
recomputed on every reconcile interval, whether or not a pod of the group is
running at the time.

What this means for a bare-pod identity in practice:

- **First pod ever.** Admitted with its template resources — admission cannot
  wait for a Prometheus query it no longer makes — and a stub is created.
- **Next reconcile.** The identity is recomputed. With no accumulated history
  yet, nothing is written: while a pod of the group is running the identity is
  a live target and its status is left as it stands, and once the group is
  empty the identity is *departed*, so a cycle that finds nothing records
  `status.source: nodata`. Either way it means "nothing computed yet", not
  "give up" — it is retried every cycle, and nothing has to be deleted, reaped
  or recreated to force a fresh attempt.
- **Once the identity has usable samples**, a recommendation is computed and
  cached — and every subsequent pod of that `owner-name` is injected at
  admission. The
  [workload-age gate](../concepts/recommendation-pipeline.md#stages) applies
  to bare pods exactly like every other kind: a `Pod`-kind identity younger
  than 10 minutes is skipped. That was not always true — bare pods used to be
  exempt, back when a bare pod could never be recycled at all and a near-zero
  percentile had nothing to act on, but that premise stopped holding once
  `Ongoing` bare pods started being resized in place, so the exemption was
  removed. The exposure of the gate applying is narrow in practice — it reads
  the *earliest* of the pod's creation time and the identity's first-seen
  time, so a recurring identity clears it on its long-lived cache object
  anyway, and a brand-new identity usually has no samples to recommend from at
  all. What gates a bare-pod identity in practice is usually simply whether
  its percentile queries return anything yet — the age gate mostly matters for
  a brand-new identity with partial warm-up samples in its first 10 minutes.

So a recurring bare-pod identity converges on its own: the cache object
outlives the individual pods, so it keeps ageing past the gate and Prometheus
history keeps accumulating against the one `owner-name`, even across gaps when
no pod of the group exists. A genuinely one-off pod that runs once and never
returns does not converge, and that is the intended outcome — there is nothing
to right-size for a second time. See
[Cold start](../concepts/workload-recommendations.md#cold-start-stub-recommendations).

Once a cache entry does exist and the pod later completes and is deleted, it
stays visible on the dashboard as an **inactive** row for the retention
window (`--recommendation-retention`, default `168h`) — see
[Retention for ephemeral workloads](../concepts/workload-recommendations.md#retention-for-ephemeral-workloads).
