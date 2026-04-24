# HPA-Aware Right-Sizing Design

## Problem

k8s-sustain adjusts pod resource requests based on observed usage. HPA computes utilization as `actual_usage / requests`. When k8s-sustain changes requests, utilization shifts instantly — even though actual load hasn't changed — causing HPA to scale up or down based on a ghost signal.

This is the same fundamental conflict that makes VPA and HPA incompatible when both target CPU/memory utilization.

## Goal

Make k8s-sustain HPA/KEDA-aware so that right-sizing and horizontal autoscaling coexist safely, without requiring users to manually tune percentiles or headroom to compensate.

## Approach

Three configurable modes in the Policy CRD, under `spec.rightSizing.updatePolicy.hpa`:

### Mode 1: `HpaAware` (default)

Factor the HPA's target utilization into the recommendation formula so that right-sizing doesn't disturb HPA scaling decisions.

**Current formula:**

```
request = p_percentile × (1 + headroom)
```

**HPA-aware formula:**

```
request = p_percentile × (1 + headroom) / (target_utilization / 100)
```

Example with p95 = 400m, headroom = 20%, HPA target = 80%:

| Scenario | Requests | Utilization at 400m | HPA reaction |
|---|---|---|---|
| Before k8s-sustain | 1000m (over-provisioned) | 40% | wants to scale down |
| k8s-sustain without HPA awareness | 480m | 83% | scales up (wrong) |
| k8s-sustain with HPA awareness | 600m | 67% | calm (correct) |

The HPA-aware value (600m) still saves 40% vs the original 1000m, but leaves the right amount of room for HPA to make correct scaling decisions. HPA scales up only when actual usage exceeds 480m — which is the correct threshold.

### Mode 2: `UpdateTargetValue`

Convert the HPA's CPU/memory metric from `Utilization` (percentage of requests) to `AverageValue` (absolute), then apply recommendations normally. This fully decouples the two concerns.

**Before:**

```yaml
metrics:
- type: Resource
  resource:
    name: cpu
    target:
      type: Utilization
      averageUtilization: 80
```

**After (computed by k8s-sustain):**

```yaml
metrics:
- type: Resource
  resource:
    name: cpu
    target:
      type: AverageValue
      averageValue: 800m  # = old_requests × target_utilization / 100
```

For KEDA-managed workloads, k8s-sustain patches the ScaledObject instead of the HPA directly, since KEDA reconciles HPAs from ScaledObjects and would revert direct HPA modifications.

Detection: if the HPA has an `ownerReference` pointing to a ScaledObject, patch the ScaledObject.

### Mode 3: `Ignore`

Current behavior. No HPA detection, no formula adjustment. Users accept the risk of HPA/k8s-sustain interaction.

## Policy API

New types added to `api/v1alpha1/policy_types.go`:

```go
// HpaMode defines how the controller interacts with Horizontal Pod Autoscalers.
// +kubebuilder:validation:Enum=HpaAware;UpdateTargetValue;Ignore
type HpaMode string

const (
    HpaModeHpaAware          HpaMode = "HpaAware"
    HpaModeUpdateTargetValue HpaMode = "UpdateTargetValue"
    HpaModeIgnore            HpaMode = "Ignore"
)

// HpaResourceConfig allows overriding the auto-detected HPA target utilization
// for a specific resource dimension.
type HpaResourceConfig struct {
    // TargetUtilizationOverride overrides the auto-detected HPA target utilization
    // for this resource. When set, the controller uses this value instead of reading
    // the HPA spec. Value is a percentage (1-100).
    // +optional
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=100
    TargetUtilizationOverride *int32 `json:"targetUtilizationOverride,omitempty"`
}

// HpaConfig configures how the controller interacts with HPAs targeting
// the same workloads.
type HpaConfig struct {
    // Mode determines the HPA interaction strategy.
    // Default: HpaAware.
    // +optional
    // +kubebuilder:default=HpaAware
    Mode HpaMode `json:"mode,omitempty"`
    // CPU holds optional overrides for CPU-related HPA settings.
    // +optional
    CPU *HpaResourceConfig `json:"cpu,omitempty"`
    // Memory holds optional overrides for memory-related HPA settings.
    // +optional
    Memory *HpaResourceConfig `json:"memory,omitempty"`
}
```

Added to `RightSizingUpdatePolicy`:

```go
type RightSizingUpdatePolicy struct {
    IgnoreAutoscalerSafeToEvictAnnotations bool       `json:"ignoreAutoscalerSafeToEvictAnnotations,omitempty"`
    // Hpa configures interaction with Horizontal Pod Autoscalers.
    // When nil, defaults to HpaAware mode with no overrides (auto-detect and adjust).
    // +optional
    Hpa *HpaConfig `json:"hpa,omitempty"`
}
```

Minimal Policy example:

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: production
spec:
  rightSizing:
    updatePolicy:
      hpa:
        mode: HpaAware  # default, can be omitted
    resourcesConfigs:
      cpu:
        requests:
          percentile: 95
          headroom: 20
  update:
    types:
      deployment: Ongoing
```

With overrides:

```yaml
spec:
  rightSizing:
    updatePolicy:
      hpa:
        mode: HpaAware
        cpu:
          targetUtilizationOverride: 75
        memory:
          targetUtilizationOverride: 80
```

## Controller Changes

### HPA Detection

New internal package or file `internal/controller/hpa.go`:

1. List HPAs in the workload's namespace using `autoscaling/v2` API
2. Match by `scaleTargetRef.kind` and `scaleTargetRef.name`
3. For each matched HPA, extract per-resource `averageUtilization` from `spec.metrics`
4. If override is set in the Policy, use the override instead
5. Return a struct with detected CPU/memory target utilizations (or nil if no HPA found)

### Recommendation Adjustment

In `reconcileWorkload()`, after `buildRecommendations()`:

- If mode is `HpaAware` and an HPA was detected with CPU utilization target:
  - Adjust: `cpuRequest = cpuRequest / (cpuTargetUtilization / 100)`
- Same for memory if the HPA has a memory utilization metric
- If no HPA found, no adjustment (regardless of mode)

### UpdateTargetValue Patching

In `reconcileWorkload()`, after applying recommendations:

- If mode is `UpdateTargetValue` and HPA uses `Utilization` type:
  - Compute `averageValue = current_requests × target_utilization / 100`
  - Check if HPA has an ownerReference to a ScaledObject
    - If yes: patch the ScaledObject's trigger config
    - If no: patch the HPA metric directly
  - Emit event on the workload: `HpaTargetUpdated`

### Graceful Degradation

- If KEDA CRD is not installed, skip ScaledObject detection (log info once at startup)
- If HPA list fails (RBAC missing), log warning and proceed as `Ignore` mode
- If HPA uses only custom/external metrics (no CPU/memory utilization), no adjustment needed

## RBAC

New ClusterRole rules:

```yaml
# Required for all HPA modes (detection)
- apiGroups: ["autoscaling"]
  resources: ["horizontalpodautoscalers"]
  verbs: ["get", "list", "watch"]

# Required only for UpdateTargetValue mode
- apiGroups: ["autoscaling"]
  resources: ["horizontalpodautoscalers"]
  verbs: ["patch"]

# Required only for UpdateTargetValue mode with KEDA
- apiGroups: ["keda.sh"]
  resources: ["scaledobjects"]
  verbs: ["get", "list", "patch"]
```

## Dependencies

- `keda.sh` types package (types only, no controller dependency) — same pattern as Argo Rollouts
- Optional: if KEDA CRD is not present, ScaledObject features degrade gracefully

## Helm Chart Changes

- `templates/rbac.yaml` — add HPA and ScaledObject rules
- `values.yaml` — no new values needed (mode is per-Policy, not global)

## Events

New event reasons on workload objects:

- `HpaDetected` — informational, emitted when an HPA is found targeting the workload
- `HpaTargetUpdated` — emitted when `UpdateTargetValue` mode patches an HPA or ScaledObject
- `HpaKedaOwned` — warning if `UpdateTargetValue` tries to patch a KEDA-managed HPA but ScaledObject detection fails

## Documentation

- `docs/concepts/architecture.md` — add HPA-awareness to the reconciliation flow diagram
- `docs/reference/policy.md` — document `hpa` config under `updatePolicy`
- `docs/guides/deployments.md` — rewrite "Combining with HPA" section with three modes and examples
- `docs/guides/keda.md` — new guide for KEDA integration

## Testing

- Unit tests for HPA detection logic (match by scaleTargetRef, extract utilization)
- Unit tests for recommendation adjustment formula (HpaAware mode)
- Unit tests for HPA/ScaledObject patch generation (UpdateTargetValue mode)
- Unit tests for graceful degradation (no HPA found, KEDA CRD missing, custom metrics only)
- Unit tests for override behavior (Policy override takes precedence over detected value)
