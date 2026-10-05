# 4. Governance is decided on the pod template

- **Status:** Accepted
- **Date:** 2026-10-05

## Context

Four readers need to know which Policy governs a pod or a pod template: the inventory (and through it the controller and the dashboard), the webhook and the OOM watcher. Each composed the same predicates itself: three walks of the opt-in levels (one reading the Namespace even when the workload level had already decided), four compositions of a Policy's consent, and the owner-name override applied in four places. The obligation to apply both the opt-in and the consent was a comment, and they disagreed.

- The inventory matched a Policy's label selector against the workload object's own `metadata.labels`; the webhook matched it against the pod's labels. With labels on a Deployment but not on its pod template, the controller governed the workload and the webhook never injected: under `Ongoing` without in-place resize every pass evicted pods that came back on template resources, and under `OnCreate` the Recommendation was never applied, so the workload was Drifted forever. With labels on the pod template only, the webhook created a cold-start stub that the controller never governed, swept after its grace period, and the next pod recreated.
- The OOM watcher applied no consent at all. It re-triggered the Policy a pod opted into even when that Policy did not accept the pod, and counted `k8s_sustain_oom_observed_total` for pods no Policy governed.

## Decision

**One module, `internal/governance`, decides governance for every reader, on the pod template.**

- One entry point, `governance.Decide(ctx, subject, reader)`. The subject is a workload's pod template, a pod, or a Departed identity (its kind and namespace and the Policy its `WorkloadRecommendation` names). The answer is the identity the subject reports into (the owner-name override applied there and only there), the Policy it opts into and at which level, and either the governing Policy or the refusal: no owner, not opted in, opted out, Policy missing, namespace excluded or not selected, selector invalid, labels not selected, kind not managed, or no Policy selecting it at all.
- The reader supplies what the subject does not carry: every Policy, a pod's owner, a workload's own annotations, a Namespace's annotations. `Decide` reads a level only when every more specific level is silent, reads a pod's owner only when the walk needs it, and checks that some Policy could select the subject before its first read. Each reader adapts its own reads: memoised listings in the inventory, the TTL caches in the webhook, direct reads in the OOM watcher.
- A Policy's label selector matches the pod template's labels everywhere: a CronJob's job template, a bare pod's own labels, the labels of the pod being admitted or OOM-killed. A Departed identity has no labels; only its namespace is checked.
- The OOM watcher records a kill and re-triggers a Policy only for a pod that Policy governs.

## Consequences

- The controller, the dashboard, the webhook and the OOM watcher cannot disagree on who governs a pod: there is one decision and one contract table (`internal/governance/governance_test.go`) for it.
- Breaking: a `labelSelector` that matched labels present only on a workload object's own metadata stops matching. Its labels belong on the pod template.
- Labels the workload controller adds to pods (`pod-template-hash`, `controller-uid`, `batch.kubernetes.io/job-name`, `statefulset.kubernetes.io/pod-name`, …) are not on the pod template, so the inventory never sees them while a pod reader may. A selector must not use them.
- The webhook lists Policies from its informer cache on every admission that reads one, instead of getting one by name.
- A webhook owner read that fails after the Policy accepted a pod now fails open like every other failed read, without the owner-name label mirror.
- The OOM watcher no longer records a kill under a ReplicaSet when its owner lookup fails: no Policy governs a ReplicaSet. The termination stays unresolved, so the pod's next status event retries the lookup.
