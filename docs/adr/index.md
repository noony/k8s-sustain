# Architecture Decisions

Decisions that shape how the code is structured, recorded when they were made. A record keeps its original reasoning; a later decision that changes it supersedes it rather than rewriting it.

| ADR | Decision | Status |
|-----|----------|--------|
| [0001](0001-single-identity-is-a-batch-of-one.md) | A single identity is a batch of one | Accepted |
| [0002](0002-conflicted-identity-freezes-its-recommendation.md) | A Conflicted identity freezes its recommendation | Accepted, refined by 0003 and 0007 |
| [0003](0003-a-recommendation-is-served-only-to-its-policy.md) | A Recommendation is served only to its Policy | Accepted |
| [0004](0004-governance-is-decided-on-the-pod-template.md) | Governance is decided on the pod template | Accepted, refined by 0006 |
| [0005](0005-a-limit-never-falls-below-its-request.md) | A limit never falls below its request | Accepted |
| [0006](0006-owner-name-is-read-from-the-annotation.md) | The owner-name override is read from the annotation | Accepted |
| [0007](0007-the-overview-is-built-from-the-identity-inventory.md) | The Overview is built from the identity inventory | Accepted |
