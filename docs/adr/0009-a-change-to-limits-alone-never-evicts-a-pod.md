# 9. A change to limits alone never evicts a pod

- **Status:** Accepted
- **Date:** 2026-10-31

## Context

The apply step drives a member's pods toward its Recommendation: it resizes a pod in place where the cluster supports it (Kubernetes 1.33 and later), and otherwise evicts it so the webhook injects the Recommendation into the replacement. Until now any difference counted, requests or limits alike. A rising limit always applied, as an increase the downsize threshold never withholds, so on a cluster without in-place resize a Recommendation that changed nothing but a limit evicted every pod of the workload, one at a time. The same pods counted as stale until then, so the identity showed as Drifted.

A limit is not what a pod is scheduled with: the scheduler and the node's admission read requests. A pod whose requests already match runs where it would run anyway; only its ceiling differs, and its next replacement gets the new ceiling from the webhook. Restarting it for that alone trades a disruption for a ceiling the workload has not hit yet. Limit-only changes are about to become common: the CPU burst boost is moving to the limit side of the chain, where it raises and lowers limits without touching requests.

The same question reached the in-place path too. A limit-only resize rejected as invalid (it would change the pod's QoS class, for one) fell back to eviction, and so did a staged resize the kubelet reported `Infeasible` or `Error`, whatever it changed: a memory limit lowered below the container's usage stays in error until usage drops.

## Decision

**A change to limits alone never evicts a pod, and a pod waiting for one is not Drifted.**

- A pod's change is a change to limits alone when its requests already match the Recommendation within the Policy's downsize tolerance and only its limits differ. A limit the guard raises because the request rose (ADR 0005) belongs to that request change, which is applied as before.
- A change to limits alone is applied by an in-place resize where the cluster supports it. Where it does not, where the pod is not Running, or where the resize is rejected, the pod is left alone; its replacement gets the limits from the webhook.
- A staged resize the kubelet reports `Infeasible` or `Error` is evicted only when it changes a request, which the resources the kubelet reports it enacted tell, the spec already carrying the target. A container whose enacted resources are not reported counts as a request change.
- One predicate in `internal/workload` decides both what to apply to a pod and whether it counts stale, so the two cannot disagree: a pod counts stale only for a request change. A withheld change to limits alone, like a decrease the downsize threshold suppresses, is not drift.

## Consequences

- Pods may run their old limits until they are replaced by a rollout, a scale event or a node drain. The `k8s_sustain_workload_stale_pods` series and the Drifted Risk state exclude them, so nothing reports that wait.
- The Recommendation and its trace still show the new limit, and new pods get it from the webhook: the stored Recommendation is what the workload converges to, not what every running pod runs.
- On a cluster with in-place resize, a change to limits alone is applied to running pods as before; only its eviction fallbacks are gone.
- A workload can still be evicted for a request change that carries a limit change with it, and the replacement gets both.
