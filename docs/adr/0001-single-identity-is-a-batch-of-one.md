# 1. A single identity is a batch of one

- **Status:** Accepted
- **Date:** 2026-10-04

## Context

Recommendation inputs (CPU and memory percentiles, the 24h OOM signal) were read two ways. The controller packed a policy's identities into sharded PromQL queries; the dashboard, identities with no observed-resources snapshot to size a shard by, and the fallback for a failed shard each queried one workload at a time through a second set of queries with exact-match selectors.

Both sets had to return identical values, so every query change was made twice, and comments guarded the duplication. The fallback ran through the very path that had not failed: a shard Prometheus rejected (typically over `--query.max-samples`) was re-read with different queries, and callers had to know which path had produced an identity's inputs, or that none had.

## Decision

There is one query path, the sharded one. A single identity is a shard with one name: a batch of one.

- Recommendation inputs are read through one port, `recommender.InputsFetcher`. In: a `ResourcesConfigs` and the identities to fetch, each with a container-count size hint that may be unknown. Out: one result per identity, its inputs or the error that made them unavailable.
- The Prometheus adapter, `recommender.PromInputs`, plans shards from the sample budget, retries a failed shard once, then re-queries its identities as one-identity shards. An identity of unknown size gets a one-identity shard from the start. CPU and memory are required; OOM is best-effort.
- The controller and the dashboard both fetch through the port.

## Consequences

- One query construction per signal. A failed shard degrades to the same queries narrowed to one name, so the fallback cannot drift from the batch.
- Callers know nothing of shards, retries or fallback. Tests substitute an in-memory adapter, `recommendertest.StaticInputs`, instead of serving PromQL.
- A one-identity shard selects with an alternation of one escaped name (`owner_name=~"…"`) rather than an exact match; the selected series are the same.
- The dashboard has no shard-budget flag; it batches with the controller's default budget.
