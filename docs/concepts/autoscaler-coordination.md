# Autoscaler Coordination

When a workload is driven by a Horizontal Pod Autoscaler (HPA) or a KEDA
`ScaledObject` targeting CPU/memory `averageUtilization`, k8s-sustain's
vertical recommendations can fight the autoscaler. Higher requests collapse
the HPA's utilization signal, causing it to scale replicas down; the
recommender then sees the same per-pod usage and raises requests again on
the next cycle.

`spec.rightSizing.autoscalerCoordination` shapes per-pod requests so the
HPA's signal stays meaningful.

## Why a vertical recommender needs to know about HPA

The naïve recommendation is "look at p95 CPU usage and set the request to
that, plus headroom." For an HPA-managed workload that's a feedback trap:

1. Workload runs at p95 = 100m. Recommender sets request = 100m + headroom.
2. HPA targets 70% utilization on CPU. With request = 100m and usage = 100m,
   utilization = 100% → HPA scales out.
3. New replicas land. Per-pod usage drops to (say) 60m.
4. Next reconcile: recommender sees p95 = 60m, lowers request to 60m.
5. Utilization climbs again, HPA scales out *again*.
6. Loop forever. Replicas drift up; per-pod usage drifts down; nothing
   stabilises.

The fix: pad the request enough that steady-state utilisation lands a
notch below the HPA target, leaving room for actual load increases to be
the only thing that triggers scale-out.

## Overhead formula (always-on when enabled)

When `enabled: true`, the recommender multiplies each affected resource's
request by `safetyMargin / hpa_target_pct`, where `safetyMargin = 110`.

Worked through:

- Workload p95 usage = 100m, HPA `averageUtilization` target = 70%.
- Adjusted request = `100 × 110 / 70 ≈ 158m`.
- Steady-state utilisation = `usage / request = 100 / 158 ≈ 63%` — about
  **7 percentage points below the 70% HPA target** (≈ 9% relative).
- A 10% load bump pushes per-pod usage to 110m → utilisation `110 / 158 ≈
  70%` → HPA scales out.

That 7-percentage-point cushion is what stops the feedback loop in the
section above: real load growth — not request inflation — is what
crosses the threshold.

The 1.10 safety margin is hard-coded in v1. Knobs to expose it are not on
the roadmap; the math gets fragile when users start tuning it for "less
overhead" without modelling their actual traffic shape.

### Caveats

- **HPA stabilization windows are not modelled.** During a scale-out
  event, replica count is in flux and per-pod usage swings. The
  recommender sizes from the busiest replica's per-pod percentile over the
  recommendation window (`spec.rightSizing.resourcesConfigs.<cpu|memory>.window`),
  which is inherently invariant to replica count — `max by` picks the
  hottest pod at each instant regardless of how many replicas exist. A
  workload that scales 3↔30 multiple times a day will still see noisier
  recommendations than one that rarely scales (cold-start spikes on new
  pods land in the envelope) — pick a longer window if your traffic has
  daily cycles.
- **HPA target changes propagate next cycle.** If you change the HPA's
  `averageUtilization` from 70% to 50%, the next k8s-sustain reconcile
  picks up the new target and re-shapes requests. The HPA's own scaling
  reaction is faster (seconds), so you'll briefly see scale-out before
  the new requests land. Acceptable for routine target tuning; consider
  draining traffic if you're making a big change.
- **`Resource` + `averageUtilization` only.** `ContainerResource`,
  `Pods`, `Object`, and `External` metrics don't drive overhead. KEDA
  triggers on Kafka lag, queue depth, etc. are ignored too — only the
  CPU/memory triggers KEDA materialises into the underlying HPA count.

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

**Worked example.** A workload with `minReplicas=3, maxReplicas=30,
anchor=0.10`:

- `target_replicas = round(3 + 0.10 × 27) = round(5.7) = 6`.
- Workload currently at 12 replicas (above target): `factor = clamp(12/6, 0.5, 2.0) = 2.0`. CPU request doubles → bigger pods, fewer of them as the HPA scales in toward 6.
- Workload currently at 3 replicas (at min, below target): `factor = clamp(3/6, 0.5, 2.0) = 0.5`. CPU request halves → thinner pods, the HPA scales out toward 6 to keep utilisation at target.
- Workload currently at 6 replicas (on target): `factor = 1.0`, no change.

The clamp `[0.5, 2.0]` exists so a single reconcile can't catastrophically up- or down-size a pod. Steady-state convergence takes a few reconcile cycles, which is fine for a knob that's optimising for monthly cost rather than real-time response.

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
