# 10. An OOM kill is counted once

- **Status:** Accepted
- **Date:** 2026-10-31

## Context

Four readers react to a container's OOM kills, and each held its own meaning of "a kill at t":

- **The OOM watcher** (`internal/oomwatch`) walked each pod's container statuses three times, once for its event predicate, once for its dedup and once for the records, each time for a `LastTerminationState` that is `OOMKilled`.
- **The killed-at limit** (`k8s_sustain:container_oom_limit_24h:bytes`), the OOM floor's anchor, sampled the memory limit while `changes(restarts[2m]) > 0` and `last_terminated_reason` was `OOMKilled`.
- **The 24h count** (`k8s_sustain:workload_oom_24h`), which drives the OOM floor's recency, the age gate's excuse and the dashboard's At risk state, was `increase(restarts[24h]) × last_terminated_reason{OOMKilled}`.
- **The chart markers** (`QueryOOMKillEvents`) built raw kube-state-metrics PromQL in Go: `increase(restarts[lookback]) × (last_terminated_reason == 1)`, then detected rising steps of the smeared result.

They disagreed. The 24h count used to be the union of that product and `max_over_time(last_terminated_reason{OOMKilled}[24h])`; kube-state-metrics keeps that series at 1 for as long as the kill is the container's last termination, the rest of the pod's life if it never fails again, so one kill kept an identity At risk, floored and excused from the age gate forever. Removing that branch (D1) left a count that multiplies every restart in the window by the *current* last termination: a container OOM-killed and then restarted for another reason counted no kill, one restarted for another reason and then OOM-killed counted two. None of the four counted a kill of a container that never restarts, such as a `restartPolicy: Never` Job's: Kubernetes keeps that kill in the container's current state, never in its last termination. `hack/signalcontract` checked that the rules existed and how they aggregated, not what they meant.

## Decision

**An OOM kill is one definition, recorded once by one sparse recording rule, `k8s_sustain:container_oom_kills`, and every reader derives from it.**

- A kill is exactly one of:
  - a restart whose last termination is `OOMKilled`;
  - the rising edge of a current `OOMKilled` termination of a container that will not restart: its pod's `restartPolicy` is `Never`.

  A container whose pod restarts it (`Always`, `OnFailure`) is counted by its restart alone, even when a scrape catches its brief terminated state first.
- The rule records each kill at `(namespace, pod, container)`, joined to `k8s_sustain:pod_workload` so it carries the identity's `owner_kind` and `owner_name`, at the evaluation that first sees it. Its value is the number of kills first seen then; between kills the series has no sample. Restarts are compared with the value one group interval (1m) earlier, so consecutive evaluations count each restart once, with `last_over_time` bridging a short scrape gap; a current termination counts only when absent for the previous 24h, so a scrape gap cannot count it twice. It is the last rule of the `k8s_sustain.workload_mapping` group: a group's rules run in order at one instant, so a pod killed before its first scrape is already mapped when its kill is recorded, and a kill recorded without an identity would never be recorded again.
- Every reader derives from it:
  - `workload_oom_24h` is `sum_over_time` of the rule over 24h, per identity and container, with the labels the OOM signal reads;
  - `container_oom_limit_24h:bytes` samples the limit at each kill, the minimum over the 5 minutes before it, so k8s-sustain's own post-kill raise is never taken for the limit the kernel killed at;
  - the dashboard's chart markers read the rule with the identity's selectors, summed over each chart step; Go builds no OOM PromQL from kube-state-metrics series any more;
  - the OOM watcher turns a pod into its kills with one function, `oomKills`, by the same definition, and its predicate, dedup and records all consume that list. When a pod's owner chain cannot be read, it records nothing rather than recording under the pod's immediate controller (a CronJob's Job, a Deployment's ReplicaSet: identities the inventory never holds), and the pod's next status event retries.
- `hack/signalcontract` requires a promtool fixture for every rule a signal computes through, so the kill rule is covered like the rules the OOM signal reads, and requires its source series in the OOM signal's contract and the bundled keep-list.

Rejected: gating a not-restarting container's kill on `kube_pod_container_status_last_terminated_timestamp` or on the pod's completion time. The former reports the last termination, which such a container never has; the latter needs another series and only dates the pod, not the container.

## Consequences

- A not-restarting container's kill counts: a `restartPolicy: Never` Job's, CronJob's or bare pod's OOM kill floors its memory and makes its identity At risk, from Prometheus and from the watcher alike.
- A kill stops counting 24h after it was recorded, whatever its container does since. A container OOM-killed and then restarted for another reason still counts its kill; one that runs clean afterwards stops counting it after 24h.
- A kill keeps counting after its pod is deleted: it carries its identity, so the 24h count and the killed-at limit no longer join the pod's current mapping.
- A bring-your-own Prometheus must keep two more kube-state-metrics series, `kube_pod_container_status_terminated_reason` and `kube_pod_restart_policy`. Without them, kills of containers that never restart are not counted; restarts still are. The bundled Prometheus keeps them. When the terminated-reason series first reaches Prometheus, a `Never` pod whose status still holds an old OOM kill counts it once, as though it had just happened.
- A kill is missed when no evaluation can compare it: a restart before the container's first scrape or inside a kube-state-metrics gap longer than 10 minutes, or a pod killed and deleted between two scrapes. The live watcher still sees most of these.
- Cardinality is sparse: the kill rule writes a sample only at a kill, so a cluster without OOM kills stores nothing for it, and the 24h count reads a sparse series instead of a 24h range of every container's restart counter.
- The chart markers show each kill at the end of the chart step it fell in, and count the same kills as the 24h count.
