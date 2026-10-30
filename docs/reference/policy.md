<!-- Source of truth: api/v1alpha1/policy_types.go -->

# Policy CRD

`Policy` is a cluster-scoped custom resource that defines how workloads should be right-sized.

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
```

---

## Full example

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: production-rightsizing
spec:
  selector:
    namespaces: [production]
  rightSizing:
    recommendOnly: false        # set true to dry-run just this policy
    excludeInitContainers: false
    update:
      types:
        deployment: Ongoing
        statefulSet: Ongoing
        daemonSet: Ongoing
        argoRollout: Ongoing
        cronJob: OnCreate
        job: OnCreate
        pod: Ongoing
      eviction:
        ignoreAutoscalerSafeToEvictAnnotations: false
    autoscalerCoordination:
      enabled: true
      replicaBudgetAnchor: 0.10
    resourcesConfigs:
      cpu:
        window: 168h
        requests:
          percentile: 95
          headroom: 10
          minAllowed: 10m
          maxAllowed: 4000m
        limits:
          keepLimitRequestRatio: true
        downsizeThreshold:
          percent: 5
          minDecrease: 10m
      memory:
        window: 168h
        requests:
          percentile: 95
          headroom: 20
          minAllowed: 32Mi
          maxAllowed: 8Gi
        limits:
          equalsToRequest: true
        downsizeThreshold:
          percent: 5
          minDecrease: 15Mi
```

---

## `spec.selector`

Restricts which namespaces and workloads this policy applies to. The selector only **narrows**: a workload must also opt in with the `k8s.sustain.io/policy` annotation (see [Resolution order](annotation.md#resolution-order)). The controller, webhook, dashboard and OOM watcher decide it in one place (`internal/governance`, see [ADR 0004](../adr/0004-governance-is-decided-on-the-pod-template.md)), so they all honour the same selector.

The controller, webhook and OOM watcher also honour their operator-level `--excluded-namespaces` flag (see [CLI reference](./cli.md)); a workload in an excluded namespace is never managed regardless of selector configuration. A malformed `spec.selector.labelSelector` (one the CRD admits but Kubernetes cannot parse, such as an unknown operator) matches no pod template, so the policy governs no workload. The webhook fails open rather than denying a pod that opts into it: whichever level the pod opts in at, it is admitted without resources injected and the webhook logs a warning with reason `selector-invalid`.

### `spec.selector.namespaces`

A list of namespace names to target. An empty list matches all namespaces. Duplicate entries are collapsed — naming a namespace twice targets it once, not twice.

```yaml
spec:
  selector:
    namespaces:
      - production
      - staging
```

### `spec.selector.labelSelector`

A standard Kubernetes [label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors) that restricts which workloads are governed by this policy. An empty selector matches all workloads in the targeted namespaces.

It matches the **pod template's** labels — `spec.template.metadata.labels` (a CronJob's `spec.jobTemplate.spec.template.metadata.labels`, a bare Pod's own `metadata.labels`), the labels every pod of the workload carries — never the workload object's own `metadata.labels`. Labels that only the `Deployment` carries select nothing; put them on its pod template.

Labels the workload controller adds to pods (`pod-template-hash`, `controller-uid`, `batch.kubernetes.io/job-name`, `statefulset.kubernetes.io/pod-name`, …) are not on the pod template, so the controller never sees them while the webhook may. Do not select on them.

```yaml
spec:
  selector:
    labelSelector:
      matchLabels:
        team: platform
      matchExpressions:
        - key: app
          operator: In
          values: [api, worker]
```

---

## `spec.rightSizing`

### `spec.rightSizing.update`

#### `spec.rightSizing.update.types`

Defines which workload kinds are managed and in what mode. Omitting a kind means that kind is unmanaged by this policy.

| Field | Type | Description |
|-------|------|-------------|
| `deployment` | `OnCreate` \| `Ongoing` | Manages `Deployment` objects |
| `statefulSet` | `OnCreate` \| `Ongoing` | Manages `StatefulSet` objects |
| `daemonSet` | `OnCreate` \| `Ongoing` | Manages `DaemonSet` objects |
| `argoRollout` | `OnCreate` \| `Ongoing` | Manages Argo Rollouts `Rollout` objects |
| `cronJob` | `OnCreate` \| `Ongoing` | Manages `CronJob` objects |
| `job` | `OnCreate` \| `Ongoing` | Manages standalone `Job` objects |
| `pod` | `OnCreate` \| `Ongoing` | Manages bare pods that opt in via `k8s.sustain.io/owner-name` (no controller owner). Never evicted in either mode — nothing would recreate the pod; `Ongoing` resizes the running pods in place on k8s ≥ 1.33, with full coverage of `restartPolicy: Never`/`OnFailure` (Airflow's default) only on k8s ≥ 1.35. See [Standalone Pods & Identity Grouping](../guides/standalone-pods-and-grouping.md) |

See [Update Modes](../concepts/update-modes.md) for the difference between `OnCreate` and `Ongoing`.

#### `spec.rightSizing.update.eviction`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ignoreAutoscalerSafeToEvictAnnotations` | bool | `false` | Evict pods even when annotated `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`. By default such pods are never evicted; in-place resizes are unaffected either way |

### `spec.rightSizing.autoscalerCoordination`

Shapes per-pod requests when an HPA or KEDA `ScaledObject` targets the
workload, so the autoscaler's utilization signal stays meaningful. See
[Autoscaler Coordination](../concepts/autoscaler-coordination.md) for the
formulas and detection rules.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enables the overhead formula `(100 / hpa_target_pct) × 1.10` for CPU/memory resources targeted on `averageUtilization`. |
| `replicaBudgetAnchor` | float (0.0–1.0) | unset | Optional. Enables CPU replica-budget correction. The fraction into `[minReplicas, maxReplicas]` at which the workload should sit at steady state (typical: `0.10`). When unset, replica correction is disabled. |

### `spec.rightSizing.excludeInitContainers`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `excludeInitContainers` | bool | `false` | When `true`, init containers (including restartable sidecar init containers) are skipped for any workload this policy targets. By default, init containers are recommended and resized like regular containers. |

Init containers are treated uniformly with regular containers by default —
both classic (one-shot) and sidecar (`restartPolicy: Always`) init containers
get a recommendation, and webhook injection updates their requests/limits at
pod creation. The controller recycles pods only when a regular container or
sidecar init container drifts from the recommendation; classic init containers
have already exited when the pod is `Running`, so their drift is picked up on
next pod creation rather than triggering an immediate recycle.

### `spec.rightSizing.recommendOnly`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `recommendOnly` | bool | `false` | When `true`, this policy runs in dry-run: recommendations are still computed, exported as metrics and cached as `WorkloadRecommendation` objects, but the controller never recycles or resizes pods and the webhook never injects resources. ORed with the global `--recommend-only` flag — the flag is a master switch, so an explicit `false` here cannot override it. |

Use this to onboard one team's workloads in dry-run while other policies
actively right-size. Inspect the resulting recommendations with
`kubectl get wlrec -A` before flipping the field to `false`.

### `spec.rightSizing.resourcesConfigs`

Configures recommendations for CPU and memory independently.

#### `cpu` / `memory`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `window` | string | `168h` | Historical lookback window for the percentile query (e.g. `96h`, `14d`). Must be a Prometheus duration: `^([0-9]+(m\|h\|d\|w\|y))+$`. |

#### `cpu.requests` / `memory.requests`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `percentile` | int32 | `95` | Percentile of usage to use as the recommendation. Range `1`–`100`. `100` resolves to the maximum sample over the window — useful for memory when you never want to undershoot the observed peak. |
| `headroom` | int32 | `0` | Safety buffer added on top of the observed percentile value, as a percentage. Range `0`–`100` (`20` = ×1.2). |
| `keepRequest` | bool | `false` | When `true`, the request is not changed. Cannot be combined with `headroom`, `percentile`, `minAllowed`, or `maxAllowed` (those have no effect when the request is kept; rejected by CRD validation). |
| `minAllowed` | Quantity | — | Floor value for the computed request. Must be `<= maxAllowed` if both are set. |
| `maxAllowed` | Quantity | — | Cap value for the computed request. Must be `>= minAllowed` if both are set. |

#### `cpu.limits` / `memory.limits`

At most one of the following fields may be set (enforced by CRD validation). If none is set, the existing limit is kept, as with `keepLimit`.

| Field | Type | Description |
|-------|------|-------------|
| `keepLimit` | bool | Keep the existing limit, unless it is below the new request: then it is raised to the request |
| `keepLimitRequestRatio` | bool | Preserve the current limit-to-request ratio (e.g. if limit was 2× request, it stays 2× the new request). A container without a request or a limit has no ratio and keeps its limit, or keeps having none |
| `equalsToRequest` | bool | Set the limit equal to the new request (Guaranteed QoS) |
| `noLimit` | bool | Remove the limit entirely |
| `requestsLimitsRatio` | float64 | Set limit = request × ratio (e.g. `1.5` sets limit to 150% of request). Must be `>= 1`. |

Whatever the strategy, a limit never ends below the request: the API server rejects such a pod and such a resize, so a kept limit the new request outgrows (an OOM floor, headroom, `minAllowed` or autoscaler coordination can each push it there) is raised to the request. The request is never lowered to fit the limit. The `WorkloadRecommendation`'s trace records the raise as its `guard` stage ([ADR 0005](../adr/0005-a-limit-never-falls-below-its-request.md)). The webhook and the in-place resize hold the same line when they write a container: when the limit it would end up with is below its request — a limit kept from a pod whose running limit differs from its template, or a limit decrease applied while the [downsize threshold](#cpudownsizethreshold-memorydownsizethreshold) withholds the request's — they raise that limit to the request.

#### `cpu.downsizeThreshold` / `memory.downsizeThreshold`

Suppresses pod recycling for resource **decreases** that are too small to be
worth the disruption. **Increases always apply immediately** (under-provisioning
risks OOM kills and CPU throttling). A decrease is applied only when it meets or
exceeds `max(percent% × current, minDecrease)`.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `percent` | int32 | `5` | Minimum decrease as a percentage of the current value. Range `0`–`100`. |
| `minDecrease` | Quantity | `10m` (CPU) / `15Mi` (memory) | Minimum decrease as an absolute quantity. Dominates the band at small workload sizes, so tiny pods are not recycled to reclaim a few millicores / MiB. |

Set both `percent: 0` and `minDecrease: "0"` to disable suppression for that
resource (every decrease is applied).

The threshold gates only the controller's recycling of **running** pods. The
admission webhook always injects the exact recommendation at pod creation — a
new pod has nothing to disrupt.

---

## Status

The controller writes a single `Ready` condition to `status.conditions`.

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: ReconciliationSucceeded
      message: All 3 workloads have been processed.
      lastTransitionTime: "2024-01-01T00:00:00Z"
```

Every count in the message is a count of workload identities: workloads
grouped under a shared `k8s.sustain.io/owner-name` count once, together, and
so does each departed identity whose retained recommendation the cycle
recomputed. An identity counts as processed when the cycle worked on at
least one of its workloads, and as failed when at least one of them failed.

These three reasons are the complete set the controller writes to
`Policy.status.conditions`:

| Reason | Status | Meaning |
|--------|--------|---------|
| `ReconciliationSucceeded` | True | Every matching workload was processed, and no workload identity is Blocked |
| `PartialFailure` | False | Some workloads failed this cycle and/or some workload identities are Blocked (including those whose workloads this cycle skipped in retry backoff); see `message`, which names only what actually happened |
| `ListFailed` | False | The cycle could not even build its work list — listing the matching workloads failed, or listing the policy's `WorkloadRecommendation` objects did. No workload was processed; the next cycle still runs at the earliest retry an earlier cycle scheduled |

A workload fails when its Prometheus inputs cannot be read, when evicting or
resizing its pods fails, or when it is left alone because its identity's
recommendation could not be stored for the webhook (its
`WorkloadRecommendation` could not be claimed, or the write was rejected).
Every such failure counts once toward `PartialFailure` and leaves the identity
[Blocked](../reference/metrics.md#drift-retry-autoscaler) until a later
cycle succeeds, whether the error is transient or permanent:

- A **transient** error (Prometheus errors and timeouts, API 5xx, 404 Not
  Found, 409 Conflict and 429) is retried with exponential backoff, 30s
  doubling with each consecutive transient failure up to 5 minutes (a
  permanent error in between starts it over). Each attempt records a
  `ReconciliationRetryScheduled` Warning event on the workload object naming
  when the next one runs, and the policy's next cycle is scheduled for that
  time when it comes before `--reconcile-interval`.
- A **permanent** error (an API rejection such as 403, 401 or Invalid, e.g.
  missing RBAC on `pods/eviction`, or a workload the controller refuses to
  apply to: one without a UID or a valid selector) records one `ReconciliationFailed`
  Warning event on the workload object when the workload enters the failure,
  and is retried on every cycle without backoff, so fixing its cause takes
  effect at the next cycle.

---

## RBAC

`Policy` is a cluster-scoped resource. Users who create or modify policies need:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: policy-admin
rules:
  - apiGroups: [k8s.sustain.io]
    resources: [policies]
    verbs: [get, list, watch, create, update, patch, delete]
```
