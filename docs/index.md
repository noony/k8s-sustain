# k8s-sustain

**k8s-sustain** is a Kubernetes operator that automatically right-sizes workload resource requests and limits using historical Prometheus metrics. It reduces cloud waste and carbon footprint without requiring manual tuning.

## Why k8s-sustain?

Most Kubernetes clusters run significantly over-provisioned. Engineers set resource requests based on worst-case estimates, and those numbers rarely get revisited. The result is idle CPU and memory that still costs money, consumes energy, and contributes to the environmental footprint of cloud infrastructure.

**We believe resource optimization should be accessible to everyone** — from a solo developer running a side project to a platform team managing thousands of workloads. Every cluster that right-sizes its workloads wastes less energy and needs fewer resources to do the same job. By keeping this tool free and open source, we hope to make it easy for any organization to reduce waste and do its part to preserve the planet.

---

## How it works

k8s-sustain continuously observes CPU and memory usage through Prometheus recording rules and produces per-container recommendations at a configurable percentile. It then applies those recommendations in one of two modes:

| Mode | Mechanism | When |
|------|-----------|------|
| **OnCreate** | Mutating admission webhook injects resources before the pod is scheduled | Each new pod creation |
| **Ongoing** | Controller recycles stale pods (in-place on k8s ≥ 1.31, PDB-respecting eviction otherwise); webhook injects resources into new pods | On a configurable interval |

A workload opts in to a policy by setting a single annotation on its pod template:

```yaml
metadata:
  annotations:
    k8s.sustain.io/policy: my-policy
```

---

## Supported workloads

| Workload | OnCreate | Ongoing |
|----------|----------|---------|
| Deployment | ✅ | ✅ |
| StatefulSet | ✅ | ✅ |
| DaemonSet | ✅ | ✅ |
| CronJob | ✅ | ✅ |

---

## Key features

- **Percentile-based recommendations** — p50 through p99, configurable per policy
- **Per-container granularity** — each container gets its own recommendation
- **In-place pod updates** — no rolling restart when the cluster supports `InPlacePodVerticalScaling` (k8s ≥ 1.31)
- **Recommend-only mode** — dry-run mode that logs recommendations without touching any workloads
- **Web dashboard** — explore policies, view workload metrics, and simulate parameter changes
- **Three independent components** — controller (Ongoing), admission webhook (OnCreate), and dashboard can run separately
- **Headroom control** — add a safety buffer on top of the observed percentile
- **Limit strategies** — keep existing ratio, set equal to request, remove limit, or use a custom multiplier
- **Prometheus-native** — ships pre-computed recording rules; no external dependency beyond Prometheus

---

## Quick navigation

<div class="grid cards" markdown>

- :material-rocket-launch: **[Quick Start](getting-started/quick-start.md)**

    Install the chart and apply your first policy in five minutes.

- :material-book-open: **[Policy CRD](reference/policy.md)**

    Full API reference for the `Policy` resource.

- :material-tag: **[Annotation](reference/annotation.md)**

    How to opt a workload into a policy.

- :material-console: **[CLI](reference/cli.md)**

    `k8s-sustain start`, `webhook`, and `dashboard` flags.

- :material-monitor-dashboard: **[Dashboard](guides/dashboard.md)**

    Explore policies, view metrics, and simulate changes.

</div>
