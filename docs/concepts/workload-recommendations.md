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

3. **GC path (controller).** At the end of every reconcile cycle, the controller lists all `WorkloadRecommendation` objects produced by the policy and deletes any whose target workload is no longer in the matched set (workload deleted, namespace excluded, annotation removed, kind disabled). Foreign-policy entries are never touched.

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
