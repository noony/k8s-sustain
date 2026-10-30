# 7. The Overview is built from the identity inventory

- **Status:** Accepted
- **Date:** 2026-10-31
- **Refines:** [ADR 0002](0002-conflicted-identity-freezes-its-recommendation.md) — the Overview counts and lists Conflicted identities, from the inventory the Workloads list reads

## Context

[ADR 0002](0002-conflicted-identity-freezes-its-recommendation.md) built identities in one place, `internal/inventory`, and made Conflicted a Risk state that outranks every other. It left one view out: the Overview's KPIs and attention lists were built from Prometheus health signals alone, so they did not show Conflicted identities.

The Risk state was classified in one place, `riskStateOf`, but its input was assembled three ways. The workload lists read the inventory, joined the identities' health and set Conflicted from the inventory; the detail page did the same for one identity; the Overview queried Prometheus cluster-wide for unhealthy identities and read no inventory at all. The Overview therefore disagreed with the lists it links to:

- A Conflicted identity never appeared on it: no KPI, no attention list.
- A Conflicted identity with a recent OOM kill counted as At risk in the KPI, and showed as Conflicted in the Workloads list.
- An identity the inventory does not hold, such as one left with neither a member nor a `WorkloadRecommendation`, counted on the Overview for as long as it had a health series, and was listed nowhere, so a KPI could not be matched with the filtered list it opens.
- The Policy rollup's Blocked counts were read from a fourth source. The Coordinated KPI was computed in the SPA from the first page of `/api/workloads`, and each attention header showed how many rows it displayed, at most ten, not how many identities were in that state.

## Decision

**Every dashboard view — the Overview, both workload lists and the detail page — is a view of one board, built from the identity inventory.**

- `Server.board` takes inventory identities, reads their health in one batch through the `HealthSignals` port, sets Conflicted from the inventory, and classifies each identity with `riskStateOf`, which nothing else calls.
- The Overview reads the board of exactly the identities the Workloads list shows (`Server.clusterIdentities`: every kind, `--excluded-namespaces` applied). Each Risk-state KPI is the total of that state's attention list, Conflicted included; each attention list shows the worst ten identities of its state next to the state's true total; the Coordinated KPI and the per-Policy Blocked counts come from the same board.
- Each Overview count equals the Workloads list filtered by the same Risk state, and a test holds the two endpoints to it.
- The SPA renders the counts and Risk states the backend returns; it derives none of them.

## Consequences

- A Conflicted identity appears on the Overview, once, as Conflicted, whatever else is wrong with it. ADR 0002's last consequence no longer holds.
- A Prometheus series for an identity the inventory does not hold counts nowhere. A Departed identity is held, so it still counts, as it is still listed.
- Each Overview recompute reads the inventory across every kind. The read happens inside the `/api/summary` recompute, so it is cached and single-flighted with the rest of the summary.
- The Policies pages still read their Blocked counts from the controller's `k8s_sustain_policy_blocked_count`, not from the board, so nothing holds them equal to the Overview's per-Policy Blocked counts.
