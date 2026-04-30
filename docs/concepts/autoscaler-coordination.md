# Autoscaler Coordination

When a workload is driven by a Horizontal Pod Autoscaler (HPA) or a KEDA
`ScaledObject` targeting CPU/memory `averageUtilization`, k8s-sustain's
vertical recommendations can fight the autoscaler. Higher requests collapse
the HPA's utilization signal, causing it to scale replicas down; the
recommender then sees the same per-pod usage and raises requests again on
the next cycle.

`spec.rightSizing.autoscalerCoordination` shapes per-pod requests so the
HPA's signal stays meaningful.

## Overhead formula (always-on when enabled)

When `enabled: true`, the recommender multiplies each affected resource's
request by `(100 / hpa_target_pct) × 1.10`. Example: baseline `100m` CPU,
HPA target 70% → adjusted request = `100 × 110 / 70 = 158m`. At baseline
load, HPA utilization sits at `100 / 158 ≈ 63%` — comfortably below 70%. A
10% load bump pushes utilization over the target and HPA scales out.

The 1.10 safety margin is fixed in v1.

## Replica-budget correction (opt-in)

`replicaBudgetAnchor` (0.0–1.0) enables an additional CPU-only adjustment
that nudges requests up or down based on the workload's position in
`[minReplicas, maxReplicas]`:

```text
target_replicas = round(min + anchor × (max - min))
factor          = clamp(current_replicas / target_replicas, 0.5, 2.0)
cpu_request    *= factor
```

Anchor `0.10` means "sit ~10% into the replica budget at steady state" —
leaving room to scale out. Workloads above the target get denser pods
(factor > 1); workloads below get thinner pods (factor < 1). Memory is not
adjusted because memory consumption doesn't track requests the way CPU
does.

## Detection rules

- HPA matched by `scaleTargetRef`; `ScaledObject` by `spec.scaleTargetRef`.
- Both objects targeting one workload → `ScaledObject` wins (KEDA owns the
  HPA).
- Only HPA `Resource` metrics with `averageUtilization` count. Other metric
  types (`ContainerResource`, `AverageValue`, `External`, `Object`, `Pods`)
  are ignored for overhead, but a workload using them still receives any
  CPU/memory utilization-based adjustment if present.
- KEDA CRD missing → `ScaledObject` lookup is skipped silently.

## Configuration

```yaml
apiVersion: k8s.sustain.io/v1alpha1
kind: Policy
metadata:
  name: example
spec:
  rightSizing:
    autoscalerCoordination:
      enabled: true              # overhead formula
      replicaBudgetAnchor: 0.10  # optional; enables CPU replica correction
    resourcesConfigs:
      cpu:
        window: 96h
        requests:
          percentile: 95
          headroom: 10
```

## Observability

The metric `k8s_sustain_coordination_factor{namespace, owner_kind,
owner_name, resource, kind}` records the multiplier applied. `kind` is
`overhead` or `replica`. The value is `1.0` when no effect was applied.
