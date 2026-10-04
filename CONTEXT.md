# k8s-sustain

Right-sizes the CPU and memory of Kubernetes workloads from their observed usage, governed by cluster-wide Policies that workloads opt into.

## Language

### Workloads

**Identity**:
The unit a recommendation is computed for: one namespace, owner kind and owner name, possibly spanning several member objects. Every count of "workloads" is a count of identities.
_Avoid_: target, entry, workload (when the unit is meant)

**Member**:
One live, opted-in object (a Deployment, Job, bare Pod, …) that belongs to an identity. An identity's containers are the union of its members' containers, and its age runs from its earliest member or from when it was first seen, whichever is older.
_Avoid_: representative

**Departed**:
An identity with no live members, whose WorkloadRecommendation is kept for the retention window. A standalone Job that has finished is departed.
_Avoid_: inactive, gone

**Conflicted**:
An identity whose members opt into different Policies. No Policy governs it until they agree.

**Too young**:
An identity younger than its Policy's minimum age, which gets no Recommendation yet.

### Recommendations

**Policy**:
A cluster-wide rule set that governs the identities opted into it: which kinds it manages, how it computes recommendations, and how it applies them.

**WorkloadRecommendation**:
The stored record of one identity's Recommendation, or of why it has none, written by the controller.
_Avoid_: cache entry, wlrec (outside kubectl)

**Recommendation**:
The requests and limits an identity's pods should run, as held in its WorkloadRecommendation. Applying it (resizing, evicting, injecting) is a separate step that recommend-only, OnCreate or a suppressed decrease can withhold.
_Avoid_: applied recommendation, current recommendation

**Running resources**:
The requests and limits a pod is actually running with.

**Simulation**:
A recommendation recomputed on demand by the dashboard, possibly under what-if settings. It is never what gets applied.
_Avoid_: preview, dry-run (dry-run means recommend-only)

### Health

**Risk state**:
The single most actionable condition of an identity, in this precedence: Conflicted, then Blocked, then At risk, then Drifted, then Safe.

**Blocked**:
The controller keeps failing to apply an identity's Recommendation and is backing off.

**At risk**:
An identity with an OOM kill in the last 24 hours.

**Drifted**:
An identity with pods whose running resources differ from its Recommendation by more than its Policy tolerates; a decrease the Policy suppresses is not drift.
