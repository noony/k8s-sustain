# Node Optimization (Out of Scope)

k8s-sustain optimizes the **pod layer**: it right-sizes per-container CPU and
memory requests and limits so each pod asks for what it actually uses. It does
**not** optimize the **node layer** — the number, size, and type of the
machines those pods run on. That is a separate problem, and the cloud providers
already solve it well. Use a dedicated node autoscaler alongside k8s-sustain.

## Two layers, two tools

| Layer | Question it answers | Owner |
|-------|--------------------|-------|
| **Pod requests** | How much CPU/memory should this container reserve? | **k8s-sustain** |
| **Node pool** | How many nodes, of which size/type, should back these pods? | Node autoscaler (Karpenter, Cluster Autoscaler, GKE compute classes) |

The two are complementary and reinforce each other. Right-sizing requests is
what makes node autoscaling effective: the scheduler bin-packs pods by their
*requests*, so tightening requests lets the autoscaler pack more pods per node
and scale the fleet down. Inflated requests waste capacity no node autoscaler
can reclaim — the nodes look "full" even while the CPUs sit idle.

!!! tip "Order of operations"
    Roll out k8s-sustain first so requests reflect real usage, then let the node
    autoscaler consolidate the freed capacity. Right-sizing without node
    autoscaling reclaims headroom *inside* nodes but never removes a node;
    node autoscaling without right-sizing scales a fleet sized for fiction.

## Why k8s-sustain stays out of the node layer

Node provisioning is deeply provider-specific — instance families, spot/
preemptible lifecycles, zonal capacity, reservations, and pricing all differ per
cloud and change constantly. The providers ship first-party controllers that
already do this far better than a generic operator could, and reimplementing
them would add a large, cloud-coupled surface that duplicates mature tooling.
k8s-sustain deliberately stops at the pod boundary and hands the node layer to
the tools below.

## Recommended node autoscalers

=== "AWS (EKS)"

    Use [**Karpenter**](https://karpenter.sh/). It provisions right-sized nodes
    just-in-time from pending pods, consolidates underutilized nodes, and
    handles spot interruptions. Pairs naturally with k8s-sustain: tighter pod
    requests give Karpenter a more accurate target to bin-pack and consolidate
    against.

=== "GKE"

    **First-party (recommended):** use
    [**compute classes**](https://cloud.google.com/kubernetes-engine/docs/concepts/about-custom-compute-classes)
    with Node Auto-Provisioning, or **GKE Autopilot** where Google manages nodes
    entirely. Compute classes let you declare fallback priorities (e.g. Spot →
    on-demand) and machine families per workload while GKE handles provisioning
    and consolidation. GKE's node autoscaling is built on the Cluster Autoscaler
    backed by Managed Instance Groups — there is no official Google or
    `kubernetes-sigs` Karpenter provider for GKE.

    **Karpenter on GKE (preview):** if you specifically want Karpenter's model
    on GCP, a community provider exists —
    [`cloudpilot-ai/karpenter-provider-gcp`](https://github.com/cloudpilot-ai/karpenter-provider-gcp).
    It is in **preview and not recommended for production**; for production GKE
    clusters, prefer the first-party compute classes / Autopilot path above.

=== "Azure / other"

    [**Karpenter for Azure** (AKS Node Auto-Provisioning)](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision)
    brings the same model to AKS. On any cluster, the
    [**Cluster Autoscaler**](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler)
    is the portable, provider-agnostic baseline when a Karpenter-style
    provisioner isn't available.

## What about VPA?

The Kubernetes [Vertical Pod Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler)
occupies the *same* layer as k8s-sustain (pod requests), not the node layer —
it is an alternative, not a complement. See
[Architecture](architecture.md) and the
[Recommendation Pipeline](recommendation-pipeline.md) for how k8s-sustain
approaches that layer. For horizontal (replica) scaling alongside vertical
right-sizing, see [Autoscaler Coordination](autoscaler-coordination.md).
