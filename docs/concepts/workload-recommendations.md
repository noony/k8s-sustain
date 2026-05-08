# Workload Recommendations (cache CRD)

`WorkloadRecommendation` is a namespaced custom resource that caches the most recent recommendation the controller computed for a single workload. It exists to keep the admission webhook serving useful resource hints when Prometheus is briefly unavailable.

## Why it exists

The webhook needs five Prometheus queries to compute an injection on every pod CREATE. If Prometheus is unreachable — restart, network partition, OOM, etc. — the circuit breaker (`internal/prometheus/breaker.go`) opens and `buildRecommendations` returns `ErrCircuitOpen`. Without a cache, the webhook would fail open and admit pods with whatever requests the workload's pod template specifies, undoing any rightsizing for new pods until Prometheus recovers.

`WorkloadRecommendation` provides a last-known-good fallback that lives in the cluster API, so:

- All webhook replicas see the same fallback value (no per-replica drift).
- Webhook restarts during the outage don't lose the cache.
- Operators can inspect what the webhook would inject with `kubectl get wlrec`.

## How it works

1. **Write path (controller).** After every successful `reconcileWorkload`, the controller upserts a `WorkloadRecommendation` whose name is `<lowercase-kind>-<workload-name>` in the workload's namespace. The `status.containers` map carries the same per-container CPU/memory request and limit values that the recycle path applied. Writes are skipped when the new recommendation is byte-identical to the previous one, so etcd write amplification scales with *change*, not workload count.

2. **Read path (webhook).** When `buildRecommendations` returns *any* error — circuit-open, timeout, malformed Prometheus response — the webhook calls `fetchCachedRecommendations`. If a `WorkloadRecommendation` exists and its `status.observedAt` is within the staleness window (default **30 minutes**), the webhook injects from the cache instead of failing open.

3. **GC path (controller).** Cleanup runs through three independent strategies so a `WorkloadRecommendation` cannot outlive its reason for existing:

   - **Per-cycle sweep.** At the end of every reconcile, the controller lists `WorkloadRecommendation`s carrying the `k8s.sustain.io/policy: <policy>` label and deletes any whose target workload is no longer in the matched set (workload deleted, namespace excluded, annotation removed, kind disabled). Cluster-wide list with no server-side filter would scale O(WLR-count) with cluster size — the label scopes the list server-side and keeps the sweep cheap.
   - **Policy-deletion finalizer.** The controller adds the `k8s.sustain.io/cleanup` finalizer to every `Policy`. On `kubectl delete policy`, the finalizer blocks Policy removal until every owned `WorkloadRecommendation` is deleted. This is the only path that GUARANTEES cleanup because it runs synchronously with the user's delete operation.
   - **Orphan reaper.** A background goroutine scans every `WorkloadRecommendation` in the cluster on a tick (default 10 min) and deletes any whose `spec.policy` references a Policy that no longer exists. Catches WLRs orphaned by `kubectl delete policy --grace-period=0 --force` (which skips finalizers entirely), controller crashes mid-delete, and Policies renamed before the per-cycle sweep ran.

   Foreign-policy entries — those carrying a different policy label — are never touched by any of these strategies.

## Schema

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: WorkloadRecommendation
metadata:
  name: deployment-web              # <kind>-<name>
  namespace: example
spec:
  policy: production-rightsizing    # owning policy
  workloadRef:
    kind: Deployment
    namespace: example
    name: web
status:
  observedAt: 2026-05-01T12:34:56Z  # webhook trusts ≤30m old
  source: prometheus                # "prometheus" or "fallback"
  containers:
    app:
      cpuRequest: 250m
      memoryRequest: 256Mi
      cpuLimit: 500m
      memoryLimit: 512Mi
      removeCpuLimit: false      # true when the policy says NoLimit
      removeMemoryLimit: false
```

The `removeCpuLimit` / `removeMemoryLimit` flags carry the explicit "strip the limit" intent (Policy `NoLimit`). They are needed because nil `cpuLimit`/`memoryLimit` alone cannot distinguish "leave alone" (`KeepLimit` / no strategy) from "remove". On Prometheus outage the webhook reads these from cache so `NoLimit` policies keep stripping limits during the outage.

The resource is namespaced and inherits namespace deletion semantics: removing the namespace removes every recommendation in it.

## Outage behaviour

- **Prometheus down, controller cache fresh (≤30m).** Webhook serves cached recommendations on every admission. Steady-state injection latency drops because no Prometheus call is made; admission stays fully functional.
- **Prometheus down, controller cache stale (>30m).** Webhook treats the entry as missing, falls open with the workload's template requests, and logs `prometheus circuit open and no fresh cache`. Admission still succeeds.
- **Prometheus down, no entry yet (brand-new workload).** Same as above — webhook fails open with template requests. The first reconcile after Prometheus recovers populates the cache.
- **Prometheus up.** Webhook always prefers the live query; the cache is only consulted on Prometheus failure.

## Observability

- `kubectl get wlrec -A` lists every cached recommendation.
- `kubectl describe wlrec deployment-web -n example` shows the full status for one workload.
- The status `observedAt` field tells operators the freshness of the data the webhook would serve right now.

## Tuning

The staleness window is set at `webhook.Handler.CacheStaleness` (default `DefaultCacheStaleness = 30m`). Future versions may expose this via Helm; today it requires a binary build.

## RBAC

The controller's ClusterRole grants `get;list;watch;create;update;patch;delete` on `workloadrecommendations` (and its `/status` subresource). The webhook reuses the controller ServiceAccount today, so the same rules cover read access. The dashboard ClusterRole (when enabled) has read-only access on the resource so operators can browse it from the UI.

### Why the grant is cluster-wide

`WorkloadRecommendation` is a namespaced resource and a Policy can target every namespace in the cluster (`spec.selector.namespaces: []` matches all). The controller therefore needs cluster-wide write access to create/update/delete WLRs anywhere a matched workload lives. The webhook needs cluster-wide reads to look up a cached entry for any pod that lands at admission, regardless of namespace.

Kubernetes RBAC has no native concept of "label-scoped reads" — a `Role` is bound to a single namespace, and a `ClusterRole` cannot be filtered by labels at evaluation time. So even though every WLR carries the `k8s.sustain.io/policy: <name>` label (used to scope the controller's *list* calls server-side), the **RBAC grant itself** must remain unconstrained.

### Hardening options

The grant is unavoidable, but you can layer additional controls:

- **Restrict the namespaces a Policy can target.** Operators provision the Policy CRDs; users don't. Treat Policy authorship as an admin operation in your manifests pipeline rather than letting any namespace owner author one.
- **NetworkPolicy on the webhook.** Limit ingress to the apiserver Service IP only — prevents anyone in the cluster from invoking `/mutate` directly.
- **Audit policy.** Log every k8s API access from the controller / webhook ServiceAccount for an audit trail of WLR mutations. Useful when investigating whether a stale recommendation came from a legitimate reconcile or out-of-band tampering.
- **Disable the dashboard** in production deployments where its read-only WLR access isn't needed (`dashboard.enabled: false`). The dashboard runs under its own narrower ClusterRole, but removing it eliminates a class of attack surface entirely.

If your environment requires cell-level isolation between namespaces, the cleanest answer today is **one Helm release per cell** with `selector.namespaces` set so each release manages only its own slice, and `installCRDs=false` on all but one. The CRD is shared cluster-wide, but each controller's WLR mutations stay within its declared namespaces.
