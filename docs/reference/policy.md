# Policy CRD

`Policy` is a cluster-scoped custom resource that defines how workloads should be right-sized.

```
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
  update:
    types:
      deployment: Ongoing
      statefulSet: Ongoing
      daemonSet: Ongoing
      argoRollout: Ongoing
      cronJob: OnCreate
  rightSizing:
    updatePolicy:
      ignoreAutoscalerSafeToEvictAnnotations: false
      hpa:
        mode: HpaAware
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
      memory:
        window: 168h
        requests:
          percentile: 95
          headroom: 20
          minAllowed: 32Mi
          maxAllowed: 8Gi
        limits:
          equalsToRequest: true
```

---

## `spec.selector`

Restricts which namespaces and workloads this policy applies to. Used by the controller when listing workloads to reconcile.

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

## `spec.update`

### `spec.update.types`

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

---

## `spec.rightSizing`

### `spec.rightSizing.updatePolicy`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ignoreAutoscalerSafeToEvictAnnotations` | bool | `false` | Skip the cluster-autoscaler `safe-to-evict` annotation check when restarting pods |
| `hpa` | object | — | Configure interaction with Horizontal Pod Autoscalers (see below) |

### `spec.rightSizing.updatePolicy.hpa`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `HpaAware` | HPA interaction strategy: `HpaAware`, `UpdateTargetValue`, or `Ignore` |
| `cpu` | object | — | CPU-specific HPA overrides |
| `memory` | object | — | Memory-specific HPA overrides |

**Modes:**

| Mode | Description |
|------|-------------|
| `HpaAware` | Auto-detect HPAs and adjust requests so HPA utilization math remains correct. Formula: `request = base_recommendation / (target_utilization / 100)` |
| `UpdateTargetValue` | Convert HPA metrics from `Utilization` to `AverageValue` (absolute), then apply recommendations normally. For KEDA workloads, patches the ScaledObject instead |
| `Ignore` | No HPA awareness. Current behavior — use at your own risk with HPA |

### `spec.rightSizing.updatePolicy.hpa.cpu` / `hpa.memory`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `targetUtilizationOverride` | int32 | — | Override the auto-detected HPA target utilization (1-100). When set, the controller uses this value instead of reading the HPA spec |

### `spec.rightSizing.resourcesConfigs`

Configures recommendations for CPU and memory independently.

#### `cpu` / `memory`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `window` | string | `168h` | Historical lookback window for the percentile query (e.g. `96h`, `14d`) |

#### `cpu.requests` / `memory.requests`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `percentile` | int32 | `95` | Percentile of usage to use as the recommendation (50–99) |
| `headroom` | int32 | `0` | Safety buffer added on top of the observed percentile value |
| `keepRequest` | bool | `false` | When `true`, the request is not changed |
| `minAllowed` | Quantity | — | Floor value for the computed request |
| `maxAllowed` | Quantity | — | Cap value for the computed request |

#### `cpu.limits` / `memory.limits`

Exactly one of the following fields should be set. If none is set, the existing limit is kept unchanged.

| Field | Type | Description |
|-------|------|-------------|
| `keepLimit` | bool | Leave the existing limit unchanged |
| `keepLimitRequestRatio` | bool | Preserve the current limit-to-request ratio (e.g. if limit was 2× request, it stays 2× the new request) |
| `equalsToRequest` | bool | Set the limit equal to the new request (Guaranteed QoS) |
| `noLimit` | bool | Remove the limit entirely |
| `requestsLimitsRatio` | float64 | Set limit = request × ratio (e.g. `1.5` sets limit to 150% of request) |

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
