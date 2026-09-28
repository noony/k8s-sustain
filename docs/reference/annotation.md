<!-- Source of truth: api/v1alpha1/policy_types.go (annotation and label constants) and internal/policymatch/resolve.go (ResolvePolicy) -->

# Annotation Reference

## `k8s.sustain.io/policy`

This annotation is the **only** way to opt a workload into a policy. The value is the name of a cluster-scoped `Policy` object.

```yaml
metadata:
  annotations:
    k8s.sustain.io/policy: <policy-name>
```

## Resolution order

The controller, webhook, dashboard and OOM watcher all resolve the policy through `internal/policymatch.ResolvePolicy`, which reads three levels, most specific first:

| Level | Where | Notes |
|---|---|---|
| Pod template | `spec.template.metadata.annotations` (CronJob: `spec.jobTemplate.spec.template.metadata.annotations`; bare Pod: the Pod's own `metadata.annotations`) | Pods inherit it, so the webhook reads it with no extra lookups. Cheapest and most explicit. |
| Workload metadata | `metadata.annotations` on the Deployment/StatefulSet/DaemonSet/CronJob/Job/Rollout | For charts that expose `metadata.annotations` but no pod-template annotation knob. Not consulted for bare Pods. |
| Namespace | `metadata.annotations` on the Namespace | Opts in every supported workload in the namespace at once. |

The first level that carries either a non-empty `k8s.sustain.io/policy` or `k8s.sustain.io/opt-out: "true"` decides; less specific levels are not read. An **empty** `k8s.sustain.io/policy` value falls through to the next level, so a Helm value that renders empty is not an opt-out.

The annotation opts the workload in; the Policy must **also** accept it. A resolved policy applies only when its `spec.selector` (`namespaces` and `labelSelector`) matches the workload and the namespace is not in the operator's `--excluded-namespaces`. The annotation names the policy, the selector narrows it. This also makes Namespace opt-in delegated rather than sovereign: namespace owners choose among the policies whose selector already reaches them; they cannot grant themselves one.

### Opting out

`k8s.sustain.io/opt-out: "true"` at a level ends resolution with no policy, so a more specific opt-out beats a less specific opt-in:

```yaml
metadata:
  annotations:
    k8s.sustain.io/opt-out: "true"
```

Only the literal string `"true"` opts out.

---

## Annotations and labels

Every `k8s.sustain.io/` key the operator reads or writes:

| Key | Kind | On | Written by | Meaning |
|---|---|---|---|---|
| `k8s.sustain.io/policy` | annotation | pod template, workload, Namespace, bare Pod | you | Opts into the named Policy. See [Resolution order](#resolution-order). |
| `k8s.sustain.io/opt-out` | annotation | pod template, workload, Namespace, bare Pod | you | `"true"` stops resolution with no policy. |
| `k8s.sustain.io/owner-name` | annotation | pod template, bare Pod | you | Overrides the workload identity: a bare Pod becomes kind `Pod` with this name; pods with a real owner are grouped under this name. Must be a valid label value (RFC 1123, ≤ 63 chars); invalid or empty values are ignored. Read from the pod template only, never from workload metadata. See [Standalone Pods & Identity Grouping](../guides/standalone-pods-and-grouping.md). |
| `k8s.sustain.io/owner-name` | label | Pod | webhook | Mirror of the annotation, added at admission so kube-state-metrics (`kube_pod_labels`) exposes it to the recording rules. |
| `k8s.sustain.io/policy` | label | `WorkloadRecommendation` | controller, webhook | Policy that produced the recommendation; used to scope list calls. |
| `k8s.sustain.io/stub` | label | `WorkloadRecommendation` | webhook | `"true"` on a [cold-start stub](../concepts/workload-recommendations.md#cold-start-stub-recommendations) created at admission. Provenance only. |
| `k8s.sustain.io/cleanup` | finalizer | `Policy` | controller | Deletes the policy's `WorkloadRecommendation`s and metric series before the Policy is removed. |

---

## Placement by workload kind

=== "Deployment"

    ```yaml
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-app
      namespace: production
    spec:
      template:
        metadata:
          annotations:
            k8s.sustain.io/policy: production-rightsizing
        spec:
          containers:
            - name: app
              image: nginx:1.27
    ```

=== "StatefulSet"

    ```yaml
    apiVersion: apps/v1
    kind: StatefulSet
    metadata:
      name: my-db
      namespace: production
    spec:
      template:
        metadata:
          annotations:
            k8s.sustain.io/policy: production-rightsizing
        spec:
          containers:
            - name: db
              image: postgres:15
    ```

=== "DaemonSet"

    ```yaml
    apiVersion: apps/v1
    kind: DaemonSet
    metadata:
      name: my-agent
      namespace: monitoring
    spec:
      template:
        metadata:
          annotations:
            k8s.sustain.io/policy: monitoring-rightsizing
        spec:
          containers:
            - name: agent
              image: busybox:1.36
    ```

=== "Rollout / Job"

    Same as Deployment: the annotation goes in `spec.template.metadata.annotations`.

=== "CronJob"

    ```yaml
    apiVersion: batch/v1
    kind: CronJob
    metadata:
      name: my-job
      namespace: production
    spec:
      schedule: "0 * * * *"
      jobTemplate:
        spec:
          template:
            metadata:
              annotations:
                k8s.sustain.io/policy: production-rightsizing  # (1)!
            spec:
              containers:
                - name: worker
                  image: busybox:1.36
    ```

    1. Note: the annotation is two levels deep — inside `jobTemplate.spec.template`.

=== "Bare Pod"

    ```yaml
    apiVersion: v1
    kind: Pod
    metadata:
      name: etl-run-42
      namespace: production
      annotations:
        k8s.sustain.io/policy: production-rightsizing
        k8s.sustain.io/owner-name: etl-daily  # (1)!
    spec:
      containers:
        - name: worker
          image: busybox:1.36
    ```

    1. Required: a pod with no controller owner is only managed under an owner-name identity.

=== "Namespace"

    ```yaml
    apiVersion: v1
    kind: Namespace
    metadata:
      name: production
      annotations:
        k8s.sustain.io/policy: production-rightsizing
    ```

---

## Adding the annotation imperatively

```bash
# Deployment
kubectl patch deployment my-app -n production \
  --type=merge \
  -p='{"spec":{"template":{"metadata":{"annotations":{"k8s.sustain.io/policy":"production-rightsizing"}}}}}'

# CronJob
kubectl patch cronjob my-job -n production \
  --type=merge \
  -p='{"spec":{"jobTemplate":{"spec":{"template":{"metadata":{"annotations":{"k8s.sustain.io/policy":"production-rightsizing"}}}}}}}'
```

---

## Removing a workload from a policy

Delete the annotation from whichever level set it (pod template, workload metadata, or Namespace) — or, if a more specific level should stop inheriting a less specific one's opt-in, set `k8s.sustain.io/opt-out: "true"` on that more specific level instead. The controller will stop reconciling the workload on the next interval; existing resources are not reverted.

```bash
kubectl annotate deployment my-app -n production \
  k8s.sustain.io/policy- \
  --overwrite
```

!!! note
    The `-` suffix on the annotation key tells `kubectl annotate` to remove it.

---

## How the annotation is consumed

Each component passes these levels to `ResolvePolicy`:

| Component | What it passes as the three levels |
|-----------|-------------------------------------|
| **Controller** | `workload.spec.template.metadata.annotations` (or `cronJob.spec.jobTemplate.spec.template.metadata.annotations`) as the pod-template level, the workload object's own `metadata.annotations`, and the Namespace's `metadata.annotations` |
| **Webhook** | `pod.metadata.annotations` as the pod-template level (pods inherit it automatically, so this is usually all it needs); when the pod itself carries neither the policy nor the opt-out annotation, it also reads the owning workload's `metadata.annotations` and the Namespace's `metadata.annotations` before giving up |
| **Dashboard** | the same three levels as the controller, read directly from the Kubernetes API for display and simulation |
| **OOM watcher** | `pod.metadata.annotations`, the owning workload's `metadata.annotations`, and the Namespace's `metadata.annotations` — resolved lazily (the pod template first, since it costs nothing; the owner and Namespace reads are only paid for if it does not decide) on every fresh OOM kill, since a pod opted in above the pod-template level carries no annotation of its own for the watcher's event predicate to see (see [Pod OOM watcher](../concepts/architecture.md#pod-oom-watcher)) |

### The webhook's cost control does not fully bound itself

When a Policy has an empty `selector`, every pod CREATE pays for up to two owner Gets (top-level owner, then its annotations for unannotated pods). Two ~30s TTL caches with `singleflight` collapse a rolling restart's burst to at most two Gets per owner, so a workload-level annotation change can take up to 30s to reach admission.
