<!-- Source of truth: api/v1alpha1/policy_types.go (PolicyAnnotation, OptOutAnnotation) and internal/policymatch/resolve.go (ResolvePolicy) -->

# Annotation Reference

## `k8s.sustain.io/policy`

This annotation is the **only** way to opt a workload into a policy. It is honoured at three levels, most specific first:

| Level | Where | Notes |
|---|---|---|
| Pod template | `spec.template.metadata.annotations` | Pods inherit it, so the admission webhook reads it with no extra lookups. Cheapest and most explicit. |
| Workload metadata | `metadata.annotations` on the Deployment/StatefulSet/DaemonSet/CronJob/Job/Rollout | For charts that expose `metadata.annotations` but no pod-template annotation knob. |
| Namespace | `metadata.annotations` on the Namespace | Opts in every supported workload in the namespace at once. |

The first level that says anything wins. A less specific level is consulted only when every more specific one is silent.

```yaml
metadata:
  annotations:
    k8s.sustain.io/policy: <policy-name>
```

The value is the name of a cluster-scoped `Policy` object.

### Opting out

`k8s.sustain.io/opt-out: "true"` at any level excludes the workload from a policy it would otherwise inherit from a less specific level:

```yaml
metadata:
  annotations:
    k8s.sustain.io/opt-out: "true"
```

Only the literal string `"true"` opts out. An **empty** `k8s.sustain.io/policy` value is not an opt-out — it falls through to the next level, so a Helm value that renders empty behaves as it always has.

### Namespace opt-in is delegated, not sovereign

A Namespace annotation only takes effect for a `Policy` whose own `spec.selector` (`namespaces` and `labelSelector`) already reaches that namespace, and the operator's `--excluded-namespaces` remains a hard deny. Namespace owners choose among the policies offered to them; they cannot grant themselves one.

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

All four components resolve the annotation through the same function, `internal/policymatch.ResolvePolicy(template, workloadMeta, namespace)`, so they can never drift on which policy a workload is opted into:

| Component | What it passes as the three levels |
|-----------|-------------------------------------|
| **Controller** | `workload.spec.template.metadata.annotations` (or `cronJob.spec.jobTemplate.spec.template.metadata.annotations`) as the pod-template level, the workload object's own `metadata.annotations`, and the Namespace's `metadata.annotations` |
| **Webhook** | `pod.metadata.annotations` as the pod-template level (pods inherit it automatically, so this is usually all it needs); when the pod itself carries neither the policy nor the opt-out annotation, it also reads the owning workload's `metadata.annotations` and the Namespace's `metadata.annotations` before giving up |
| **Dashboard** | the same three levels as the controller, read directly from the Kubernetes API for display and simulation |
| **OOM watcher** | `pod.metadata.annotations`, the owning workload's `metadata.annotations`, and the Namespace's `metadata.annotations` — resolved lazily (the pod template first, since it costs nothing; the owner and Namespace reads are only paid for if it does not decide) on every fresh OOM kill, since a pod opted in above the pod-template level carries no annotation of its own for the watcher's event predicate to see (see [Pod OOM watcher](../concepts/architecture.md#pod-oom-watcher)) |

### The webhook's cost control does not fully bound itself

The webhook pre-gates the owner/Namespace reads above behind `anyPolicyCovers`: if no Policy's `selector` could claim a pod at all, it skips them entirely. This *is* the cost control for a cluster with at least one selector-scoped Policy — most pod creates in most namespaces are skipped before any extra read.

It does **not** bound cost on a cluster where a Policy has an empty `selector` (no `namespaces`, no `labelSelector`) — the default, and what [Quick Start](../getting-started/quick-start.md) produces. Such a Policy covers every pod in the cluster, so the pre-gate always passes and every pod CREATE it admits pays for owner resolution: `resolveCachedPodOwner` (via a Deployment/Rollout pod's ReplicaSet, or a CronJob pod's Job) does one Get to find the top-level owner. This Get is not scoped to the multi-level opt-in chain — `admit()` needs the same top-level owner to look up the `WorkloadRecommendation` for **every** pod it injects into, pod-template-annotated (the common case, since every existing user annotates the pod template) or not. A pod with no annotation of its own additionally goes through the opt-in chain (`resolveOptIn`), which needs a second Get — `ownerAnnotations`, of the resolved top-level object's own `metadata.annotations` — to evaluate the workload level; that second Get *is* scoped to unannotated pods, since an annotated pod's own annotation already decides the policy without ever reading its owner's. A rolling restart of a large Deployment therefore issues up to two owner Gets per replica in the worst case — not one.

The webhook absorbs this with two small in-memory caches (`internal/webhook/ownercache.go`), both TTL ~30s and both consulted before their respective Get: `Handler.ownerRefCache`, keyed by the pod's own immediate controller ownerRef (`namespace/kind/name/UID` of its ReplicaSet or Job), bounds the first Get — shared by **every** pod CREATE that reaches owner resolution at all, whether it took the pod-template-annotated path or the multi-level opt-in chain; `Handler.ownerAnnCache`, keyed by `(namespace, kind, name)` of the resolved top-level object, bounds the second Get and is reached only via the multi-level opt-in chain. N pods created behind the same owner in a burst — exactly what a rolling restart produces — share both cache keys, so the pair collapses from up to 2N Gets to at most 2.

Both caches also collapse a *concurrent* cold-start burst, not just the steady-state (already-warm) case: N pods admitted at the same instant behind one owner — the exact shape of a rolling restart — issue one in-flight Get via [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight), with every other admission waiting on that Get's result instead of each starting its own. A waiter still respects its own admission deadline: if it times out before the in-flight Get returns, only that one admission gives up and fails open (with the pod resolved on template resources); the Get itself keeps running regardless, to completion or its own 2s `apiCallTimeout` budget (whichever comes first) — not just for as long as some other admission is still waiting on it, but also to populate the cache for the next burst even if every waiter has already given up.

A panic inside that shared Get is contained rather than fatal. Running it on a `singleflight` goroutine puts it outside the HTTP handler's panic-recovery middleware, and `singleflight` deliberately re-raises a leader panic on a bare goroutine that nothing can recover — so, unguarded, one nil deref under an owner Get would abort the whole webhook process and (under `failurePolicy: Fail`) block every Pod CREATE in the cluster until it restarted. The webhook therefore recovers such a panic itself, logs it with its stack, counts it on [`k8s_sustain_webhook_panic_total`](metrics.md#k8s_sustain_webhook_panic_total) under `singleflight/ownerRef` or `singleflight/ownerAnnotations`, and hands it to the leader and every waiter as an ordinary error — which they fail open on like any other failed Get. Nothing is cached, and the next admission retries.

The trade-off: a workload that gains its opt-in annotation at the workload level may take up to the cache's TTL to be picked up by admission. This is acceptable because the controller reconciles independently of admission on its own interval regardless of what the webhook does.
