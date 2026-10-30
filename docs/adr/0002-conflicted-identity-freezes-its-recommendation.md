# 2. A Conflicted identity freezes its recommendation

- **Status:** Accepted
- **Date:** 2026-10-04
- **Refined by:** [ADR 0003](0003-a-recommendation-is-served-only-to-its-policy.md) — the webhook also requires the Policy that computed the numbers to be the pod's

## Context

An identity can span several objects: owner-name groups (`api-blue` and `api-green` both reporting as `Deployment/api`) and bare-pod groups. Each object opts into a Policy on its own, so members of one identity can name different Policies. The identity still has one Prometheus series and one `WorkloadRecommendation`.

Nothing modelled that case. Each Policy's reconcile treated the identity as its own: `wlrcache.EnsureExists` rewrote the object's `spec.policy` and policy label to whichever Policy ran last, so the object flipped between them every cycle and each recomputed it under its own configuration. The webhook never compared the object's Policy with the pod's, so every member received whichever Policy's numbers had won last. The dashboard listed the identity under both Policies, the controller counted it in both, and bare-pod grouping had a third rule that silently dropped the minority pods.

Objects were also turned into identities twice, by the controller and by the dashboard, with different rules for containers, age, finished Jobs and governance.

## Decision

Identities are built in one place, `internal/inventory`, and the controller's rules are canonical. A member is governed by the Policy it opts into when that Policy exists, manages its kind and selects it. An identity is governed by its members' Policy when they agree, and is **Conflicted** when they name different Policies. A member that opts out, or whose Policy does not accept it, is no party to a conflict.

**No Policy governs a Conflicted identity.**

- The controller does not recompute it, does not apply anything to its pods, and does not rewrite its `WorkloadRecommendation`'s `spec.policy` or label. Only the governing Policy calls `EnsureExists`, which ends the flip.
- Its stored Recommendation is frozen. Each Policy party to the conflict records `status.outcome: Conflicted`, and nothing else.
- The webhook injects only when the pod's resolved Policy equals the object's `spec.policy`. Members still opting into the last governing Policy keep the frozen numbers; members of the other Policy are admitted unmutated (`other-policy`), as the webhook always fails open.
- Like a departed one, a frozen recommendation is exempt from the staleness gate and bounded by `--recommendation-retention`.
- The identity counts for no Policy, has no controller health series, and is shown once by the dashboard, in the Conflicted Risk state, which outranks every other.

`status.source` is replaced by `status.outcome` (`Computed`, `NoData`, `TooYoung`, `FetchFailed`, `Conflicted`). Every outcome but `Computed` keeps the last Recommendation.

## Consequences

- A configuration error stops changing pods instead of thrashing them between two Policies' numbers. The identity resumes under one Policy as soon as its members agree; that Policy's next reconcile adopts the object.
- Pods of the losing side start on their template resources until the conflict is fixed. Pods of the side the object names keep getting the last Recommendation, which ages: past the retention window they start on template resources too.
- The webhook now also withholds numbers from an identity moving between Policies until the new Policy's next reconcile adopts its object, since those numbers were computed under the old one.
- The Overview's KPIs and attention lists are built from Prometheus health signals alone and do not show Conflicted identities yet; the Workloads list and detail pages do.
