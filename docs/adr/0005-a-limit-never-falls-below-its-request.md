# 5. A limit never falls below its request

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

The recommender shapes a resource's request through a chain of stages (usage percentile, OOM floor, headroom, min/max clamp, autoscaler coordination) and then derives its limit from the Policy's limit strategy. Two strategies keep the limit the container already has: `keepLimit`, and no strategy at all, which is the default. `keepLimitRequestRatio` falls back to keeping it when the container has no ratio to take.

A kept limit knows nothing of the new request. The OOM floor raises a killed container's memory request to 1.2 × the limit it was killed at, so for a container that keeps its limit the floor is above the limit by construction; headroom, `minAllowed` and coordination can push any request past it too. The Recommendation then holds a request above the limit the pod will run with.

The apiserver rejects such a pod. An in-place resize is refused as invalid; the patcher falls back to eviction; the replacement pod is admitted by the webhook with the same invalid pair and rejected; the patcher's replacement wait sees nothing pending, takes the workload for settled and evicts the next pod. Without a PodDisruptionBudget the whole workload goes in one pass, and it is an OOM-looping workload, the one the floor exists to rescue. The `oom-kill` test scenario hid this because it uses `equalsToRequest`.

## Decision

**When a limit results, derived or kept from the container, and it is below the final request, the limit is raised to the request.**

- The guard is the last stage of the limit side, after the strategy and any signal that may raise the limit. It runs in the recommender, so the controller, the webhook and the dashboard's simulations all see the same pair.
- The limit gives way, never the request. The request is what the signals asked for, the OOM floor included; capping it at an old limit would put an OOM-killed container straight back under the limit it was killed at.
- A removed limit (`noLimit`), or a container without a limit that keeps having none, needs no guard: no limit is above every request.
- The trace records the raise as its own `guard` stage, exactly when it changed the limit, so a raised limit is never mistaken for one the Policy's strategy chose.
- Where pods are written, container injection (`workload.Inject`, which the patcher and the webhook share) applies the same rule to the pair it finally writes, so a Recommendation stored before the guard, or a request decrease the downsize tolerance withholds while letting the limit's through, still never leaves a request above its limit.

## Consequences

- A Policy that keeps limits no longer produces Recommendations the apiserver rejects. A workload whose request outgrows its limit gets its limit raised to the request, which for that resource makes request and limit equal until the request falls back below the container's own limit.
- `keepLimit` no longer means "never touch the limit": it means "keep the limit unless it is below the request". The Policy reference says so.
- Two places enforce the rule: the recommender, for every Recommendation it computes, and container injection, for every pair written to a pod. Neither rejects anything; both raise the limit, and injection logs each limit it raised.
