# 3. A Recommendation is served only to its Policy

- **Status:** Accepted
- **Date:** 2026-10-05
- **Refines:** [ADR 0002](0002-conflicted-identity-freezes-its-recommendation.md)

## Context

[ADR 0002](0002-conflicted-identity-freezes-its-recommendation.md) made the webhook inject only when the pod's resolved Policy is the one the `WorkloadRecommendation` names in `spec.policy`, so that a pod only gets numbers computed under its own Policy. But `spec.policy` says which Policy governs the identity now, not which one computed the numbers the object holds.

When another Policy starts governing an identity (a workload re-annotated, a Conflicted identity whose members come to agree on the other Policy, a GitOps rename), its first reconcile adopts the object: it rewrites `spec.policy` and the policy label, and keeps the stored Recommendation, as every outcome but `Computed` does. If that Policy's first pass decided anything else — too young under its own minimum age, no data under its own window, a failed fetch — the webhook served the previous Policy's numbers to the new Policy's pods as a fresh hit, for up to the 30-minute staleness budget. A Conflicted identity resolving to the Policy whose pods had been left unmutated was worse: its frozen numbers were served as retained, and if every member stayed in retry backoff, nothing recorded anything until the retention window ran out.

The same adoption broke the Policy-deletion cleanup. It deleted the deleted Policy's objects on their UID alone, reasoning that "this object belongs to the Policy being deleted" could not change. Adoption changes it: when a rename creates the new Policy before deleting the old one, the new Policy can adopt an object between the old one's finalizer listing it and deleting it, and the delete destroyed the adopted object.

## Decision

**A Recommendation is served only to pods of the Policy that computed it.**

- `status.computedBy` records the Policy that computed `status.containers`. It is written with every `Computed` outcome, from the Policy the object names, and kept by every other outcome, like the Recommendation itself.
- The webhook injects only when both `spec.policy` and `status.computedBy` are the pod's resolved Policy; otherwise the Recommendation is withheld (`other-policy`).
- Adopting an object keeps the previous Policy's numbers in place, attributed to it. They are withheld from every pod until the adopting Policy records a Recommendation of its own.
- A Conflicted identity keeps working as ADR 0002 describes: neither `spec.policy` nor `status.computedBy` changes while its members disagree, so pods of the last governing Policy keep the frozen numbers.
- Every cleanup path deletes a `WorkloadRecommendation` only at the revision it judged (UID and `resourceVersion`). On the Policy-deletion path a conflict is returned, so the finalizer stays until a retry re-lists the Policy's objects; one another Policy adopted meanwhile is no longer listed and survives.

## Consequences

- An identity moving between Policies starts its pods on template resources from the move until the new Policy's first Recommendation, instead of on numbers computed under the old Policy's window, percentile, headroom and limits. For a live identity with history that is the new Policy's first reconcile.
- An object written before `status.computedBy` existed carries none and is withheld until its Policy records a Recommendation. A live identity with samples gets one on its next reconcile; a departed identity whose samples have aged out never does, and starts on template resources until it runs again.
- The Policy-deletion cleanup can take more than one reconcile when an object is rewritten while it runs.
