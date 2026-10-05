# k8s-sustain

Right-sizes the CPU and memory of Kubernetes workloads from their observed usage, governed by cluster-wide Policies that workloads opt into.

## Language

### Workloads

**Identity**:
The unit a recommendation is computed for: one namespace, owner kind and owner name, possibly spanning several member objects. Every count of "workloads" is a count of identities.
_Avoid_: target, entry, workload (when the unit is meant)

**Member**:
One live object (a Deployment, Job, bare Pod, …) that belongs to an identity. A member is governed by the Policy it opts into when that Policy exists, manages its kind and selects it. An identity's containers are the union of its governed members' containers, the newest member's winning for a container several declare, and its age runs from its earliest governed member or from when it was first seen, whichever is older. A finished standalone Job and a CronJob's Jobs are never members.
_Avoid_: representative

**Departed**:
An identity with no live members, whose WorkloadRecommendation is kept for the retention window and still recomputed by the Policy it names. A standalone Job that has finished is departed.
_Avoid_: inactive, gone

**Conflicted**:
An identity whose members are governed by different Policies. No Policy governs it until they agree: its Recommendation is frozen as the last governing Policy left it, nothing is recomputed or applied, and only pods opting into that Policy still receive it, for the retention window. A member that opts out, or whose Policy does not accept it, is no party to a conflict.

**Too young**:
An identity younger than its Policy's minimum age, which gets no Recommendation yet.

### Recommendations

**Policy**:
A cluster-wide rule set that governs the identities whose members it accepts: which kinds it manages, how it computes recommendations, and how it applies them. An identity is governed by at most one Policy.

**WorkloadRecommendation**:
The stored record of one identity's Recommendation, the Policy that computed it, and the outcome of the controller's last decision for it (Computed, NoData, Too young, FetchFailed, Conflicted). The governing Policy writes it; the webhook only asks for one, and records the containers of identities no reconcile sees alive.
_Avoid_: cache entry, wlrec (outside kubectl)

**Recommendation**:
The requests and limits an identity's pods should run, as held in its WorkloadRecommendation. It is served only to pods of the Policy that computed it: an identity another Policy adopts gets none until that Policy computes its own. Applying it (resizing, evicting, injecting) is a separate step that recommend-only, OnCreate or a suppressed decrease can withhold.
_Avoid_: applied recommendation, current recommendation

**Signal**:
An observed input that contributes one stage to a Recommendation: the CPU and memory usage of the busiest replica, which set the requests, and recent OOM kills, which can only raise the memory request. An identity whose usage cannot be read gets no Recommendation; one whose OOM kills cannot be read is recommended without them.
_Avoid_: metric, input (when one source is meant)

**Trace**:
The record of how a Recommendation was derived: for each container and resource, the value after every stage of the computation that ran (usage percentile, OOM floor, headroom, min/max clamp, autoscaler coordination and its factors, limit). Stored in the WorkloadRecommendation with the Recommendation it explains, never on its own.
_Avoid_: breakdown, explanation, debug info

**Running resources**:
The requests and limits a pod is actually running with.

**Simulation**:
A recommendation recomputed on demand by the dashboard, possibly under what-if settings. It is never what gets applied.
_Avoid_: preview, dry-run (dry-run means recommend-only)

### Health

**Risk state**:
The single most actionable condition of an identity, in this precedence: Conflicted, then Blocked, then At risk, then Drifted, then Safe.

**Blocked**:
The controller keeps failing to apply an identity's Recommendation and is backing off. One failing member is enough to block the identity.

**At risk**:
An identity with an OOM kill in the last 24 hours.

**Drifted**:
An identity with pods whose running resources differ from its Recommendation by more than its Policy tolerates; a decrease the Policy suppresses is not drift.
