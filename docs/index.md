---
title: Home
hide:
  - navigation
  - toc
---

<div class="ks-hero" markdown>

<span class="ks-hero__eyebrow">Open source · Kubernetes operator</span>

<h1 class="ks-hero__title">Right-size Kubernetes workloads. <em>Automatically.</em></h1>

<p class="ks-hero__lead">
k8s-sustain reads the CPU and memory your containers actually use from Prometheus and turns that history into requests and limits — applied in place, with no rolling restarts on modern clusters and no manual tuning.
</p>

<div class="ks-hero__actions" markdown>

[Quick Start](getting-started/quick-start.md){ .md-button .md-button--primary }
[Read the concepts](concepts/architecture.md){ .md-button }

</div>

<div class="ks-hero__install" markdown>

```bash
helm install k8s-sustain oci://ghcr.io/noony/helm-charts/k8s-sustain \
  --namespace k8s-sustain --create-namespace
```

</div>

</div>

## Why it matters { .ks-section-title }

<p class="ks-section-lead">
Over-provisioning is the quiet default in most clusters, and it has a cost beyond the cloud bill.
</p>

<div class="ks-why" markdown>

<div class="ks-why__card ks-why__card--cost" markdown>

:material-cash-multiple:{ .ks-why__icon }

### Idle capacity is billed

Requests are set from worst-case guesses and rarely revisited. Every unused core and gigabyte is still reserved, scheduled and paid for.

</div>

<div class="ks-why__card ks-why__card--energy" markdown>

:material-lightning-bolt:{ .ks-why__icon }

### Idle capacity burns energy

Over-provisioned nodes draw power, generate heat and drive demand for more hardware, adding to the footprint of cloud infrastructure.

</div>

<div class="ks-why__card ks-why__card--open" markdown>

:material-leaf:{ .ks-why__icon }

### Right-sizing for everyone

From a solo developer's side project to a platform team running thousands of workloads: free, open source and no manual tuning.

</div>

</div>

<div class="ks-statement" markdown>

We believe resource optimisation should be *accessible to everyone*. Every cluster that right-sizes its workloads wastes less energy and needs fewer machines to do the same job.

</div>

## How it works { .ks-section-title }

<p class="ks-section-lead">
One annotation opts a workload into a <code>Policy</code>. From there the pipeline runs on its own.
</p>

<div class="ks-steps" markdown>

<div markdown>

### Observe

Prometheus recording rules aggregate per-container CPU and memory usage by workload. Nothing to instrument.

</div>

<div markdown>

### Recommend

The controller takes a configurable percentile over the lookback window, adds headroom, respects min/max clamps, OOM floors and autoscaler targets.

</div>

<div markdown>

### Apply

The admission webhook injects resources into new pods; the controller resizes running ones in place (k8s ≥ 1.33) or recycles them behind their PodDisruptionBudget.

</div>

</div>

```yaml
metadata:
  annotations:
    k8s.sustain.io/policy: my-policy
```

The annotation is honoured on the pod template, on the workload's own `metadata.annotations`, or on its Namespace — most specific first. See the [Annotation reference](reference/annotation.md) for precedence and the opt-out escape hatch.

## Two update modes { .ks-section-title }

<p class="ks-section-lead">
Choose per workload kind how far k8s-sustain may go.
</p>

<div class="ks-modes" markdown>

<div class="ks-mode" markdown>

<p class="ks-mode__head" markdown="span">:material-shield-plus-outline:{ .ks-mode__icon } <span class="ks-mode__name">OnCreate</span> <span class="ks-mode__when">on every new pod</span></p>

The mutating admission webhook injects the cached recommendation before the pod is scheduled. Running pods are never touched.

</div>

<div class="ks-mode ks-mode--ongoing" markdown>

<p class="ks-mode__head" markdown="span">:material-autorenew:{ .ks-mode__icon } <span class="ks-mode__name">Ongoing</span> <span class="ks-mode__when">on a configurable interval</span></p>

Everything OnCreate does, plus the controller brings running pods up to date: in place on k8s ≥ 1.33, PDB-respecting eviction otherwise.

</div>

</div>

<div class="ks-kinds" markdown>

<p class="ks-kinds__label">Supported in both modes</p>

<p class="ks-kinds__list">
<span>Deployment</span><span>StatefulSet</span><span>DaemonSet</span><span>Argo Rollout</span><span>Job</span><span>CronJob</span>
</p>

</div>

<p class="ks-note" markdown="span">:material-information-outline: CronJob, Job and bare-pod workloads are never evicted: their running pods are only ever resized in place, since eviction would discard in-flight work and nothing would recreate a bare pod. See [Update modes](concepts/update-modes.md).</p>

## Built for production clusters { .ks-section-title }

<p class="ks-section-lead">
Every safeguard a platform team expects, and nothing that touches your GitOps-managed specs.
</p>

<div class="grid cards" markdown>

- :material-chart-bell-curve-cumulative:{ .lg .middle } **Percentile-based recommendations**

    ---

    p50 through p99, configurable per policy and per resource, with a headroom buffer on top of the observed value.

- :material-arrow-expand-vertical:{ .lg .middle } **In-place pod updates**

    ---

    Zero-restart resizes through the `pods/resize` subresource when the cluster supports it, PDB-respecting eviction otherwise.

- :material-source-branch-check:{ .lg .middle } **GitOps-safe by design**

    ---

    Workload specs are never patched. Resources reach pods only through admission and resize, so Argo CD and Flux never see drift.

- :material-eye-outline:{ .lg .middle } **Recommend-only mode**

    ---

    Dry-run globally or per policy: recommendations are computed, cached and logged without touching any workload.

- :material-fire-alert:{ .lg .middle } **OOM-aware memory floors**

    ---

    An `OOMKilled` container re-triggers reconciliation immediately and bumps the memory floor, VPA-style.

- :material-scale-balance:{ .lg .middle } **Autoscaler coordination**

    ---

    Requests are shaped so HPA and KEDA utilisation targets stay meaningful instead of fighting the right-sizer.

- :material-cube-outline:{ .lg .middle } **Per-container granularity**

    ---

    Sidecars and init containers get their own recommendation, clamped by the policy's min and max.

- :material-monitor-dashboard:{ .lg .middle } **Web dashboard**

    ---

    Explore policies, chart real usage against requests, and simulate percentile or headroom changes before applying them.

- :material-shield-lock-outline:{ .lg .middle } **Authenticated Prometheus**

    ---

    Bearer token, basic auth, custom headers and TLS, including multi-tenant Thanos, Mimir and Cortex gateways.

</div>

## See it in action { .ks-section-title }

<p class="ks-section-lead">
The dashboard shows current requests against observed usage and what each policy would change.
</p>

![Workload resizing dashboard](assets/dashboard-workload-resizing.png){ .ks-shot }

## Explore the docs { .ks-section-title }

<div class="grid cards" markdown>

- :material-rocket-launch:{ .lg .middle } **[Quick Start](getting-started/quick-start.md)**

    ---

    Install the chart and apply your first policy in five minutes.

- :material-book-open-variant:{ .lg .middle } **[Policy CRD](reference/policy.md)**

    ---

    Full API reference for the `Policy` resource.

- :material-tag-outline:{ .lg .middle } **[Annotation](reference/annotation.md)**

    ---

    How to opt a workload into a policy, and how to opt one out.

- :material-console:{ .lg .middle } **[CLI](reference/cli.md)**

    ---

    `k8s-sustain start`, `webhook` and `dashboard` flags.

- :material-sitemap-outline:{ .lg .middle } **[Architecture](concepts/architecture.md)**

    ---

    Controller, webhook and dashboard, and how they share one pipeline.

- :material-shield-check-outline:{ .lg .middle } **[Security](security.md)**

    ---

    Signed images, SBOMs and SLSA provenance for every release.

</div>
