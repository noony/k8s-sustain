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
  rightSizing:
    update:
      types:
        deployment: Ongoing
        statefulSet: Ongoing
        daemonSet: Ongoing
        argoRollout: Ongoing
        cronJob: OnCreate
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

Restricts which namespaces and workloads this policy applies to. Both the controller (when listing workloads to reconcile) and the admission webhook (when deciding whether to inject resources at pod creation) honour the same selector — so a workload outside the policy's scope is left alone by both components.

The webhook additionally honours the operator-level `--excluded-namespaces` flag (see [CLI reference](./cli.md)); a pod in an excluded namespace is admitted unchanged regardless of selector configuration. If `spec.selector.labelSelector` is malformed, the webhook fails open (admits the pod without mutation and logs a warning) rather than denying it.

### `spec.selector.namespaces`

A list of namespace names to target. An empty list matches all namespaces.

```yaml
spec:
  selector:
    namespaces:
      - production
      - staging
```

### `spec.selector.labelSelector`

A standard Kubernetes [label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors) that restricts which workloads are governed by this policy. An empty selector matches all workloads in the targeted namespaces.

The controller matches against the workload object's `metadata.labels` (e.g. the `Deployment`'s own labels). The webhook matches against the *pod*'s labels (the pod template's `metadata.labels`, which become the labels of every replica).

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
| `headroom` | int32 | `0` | Safety buffer added on top of the observed percentile value |
| `keepRequest` | bool | `false` | When `true`, the request is not changed. Cannot be combined with `headroom`, `percentile`, `minAllowed`, or `maxAllowed` (those have no effect when the request is kept; rejected by CRD validation). |
| `minAllowed` | Quantity | — | Floor value for the computed request. Must be `<= maxAllowed` if both are set. |
| `maxAllowed` | Quantity | — | Cap value for the computed request. Must be `>= minAllowed` if both are set. |

#### `cpu.limits` / `memory.limits`

At most one of the following fields may be set (enforced by CRD validation). If none is set, the existing limit is kept unchanged.

| Field | Type | Description |
|-------|------|-------------|
| `keepLimit` | bool | Leave the existing limit unchanged |
| `keepLimitRequestRatio` | bool | Preserve the current limit-to-request ratio (e.g. if limit was 2× request, it stays 2× the new request) |
| `equalsToRequest` | bool | Set the limit equal to the new request (Guaranteed QoS) |
| `noLimit` | bool | Remove the limit entirely |
| `requestsLimitsRatio` | float64 | Set limit = request × ratio (e.g. `1.5` sets limit to 150% of request). Must be `>= 1`. |

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
resource (every decrease is applied, as before this feature existed).

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
      message: All targeted workloads have been processed.
      lastTransitionTime: "2024-01-01T00:00:00Z"
```

| Reason | Status | Meaning |
|--------|--------|---------|
| `ReconciliationSucceeded` | True | All matching workloads were processed successfully |
| `ReconciliationFailed` | False | One or more workloads failed; see `message` for details |
| `NamespaceResolutionFailed` | False | Could not list namespaces |
| `InvalidSelector` | False | The label selector is malformed |

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
