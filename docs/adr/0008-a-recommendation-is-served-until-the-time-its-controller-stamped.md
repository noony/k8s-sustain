# 8. A Recommendation is served until the time its controller stamped

- **Status:** Accepted
- **Date:** 2026-10-31
- **Refines:** [ADR 0003](0003-a-recommendation-is-served-only-to-its-policy.md) — besides being served only to its Policy's pods, a Recommendation is served only until the time the controller stamped when storing it

## Context

The webhook decided whether a stored Recommendation was still worth injecting from its age, against two bounds it held itself: a fixed 30-minute staleness budget for a live identity (`wlrcache.DefaultStaleness`, overridable only by a `Handler` field no flag set), and its own `--recommendation-retention` for a Departed or Conflicted identity, whose `observedAt` is frozen by design. The controller held the other half of each bound: its `--reconcile-interval` decides how often `observedAt` is refreshed, and its own `--recommendation-retention` decides how long the sweep keeps a Departed or Conflicted identity's object. Each pair had to agree, and nothing made them:

- The two retention flags were rendered from one Helm value, but agreed only by chart convention, and zero meant opposite things on either side: the sweep kept nothing, the webhook fell back to 168h. With zero retention and a controller that could not list its workloads, so skipped its sweep, the webhook served Departed and Conflicted numbers for a week.
- The staleness budget did not follow the reconcile interval at all. With `--reconcile-interval` at 30m or more, which the CLI reference suggested, every Recommendation went stale between two passes and the webhook stopped injecting.

The sweep had a defect of its own: its grace period, meant to protect an object created after the reconcile's snapshot, was counted from the sweep's own clock. A pass that waited on evictions could sweep more than ten minutes after its snapshot, and deleted a webhook stub created in between as belonging to no Policy.

## Decision

**A Recommendation is served until the time its controller stamped when storing it, `status.servableUntil`.**

- `wlrcache` computes the stamp from one formula over the status it writes, with every status write of `Record` and `Ensure`, so it is always consistent with what the object holds:
  - for a live identity, `observedAt` + the staleness budget, the larger of 30 minutes and three reconcile intervals;
  - for a Departed or Conflicted identity, its last computation + `--recommendation-retention`.
- Only a `Computed` outcome moves `observedAt`, so `NoData`, `TooYoung`, `FetchFailed` and `Conflicted` never extend the stamp. Nor do they, or a claim, give an object without one its first: only a `Computed` record does, since any other write stamping it would extend what it is served for. A stable `Computed` Recommendation is rewritten every refresh interval (10 minutes), which keeps it servable through a pass that runs one interval late, whatever the interval.
- The webhook serves a Recommendation only while the admission time is before `servableUntil`, and holds no freshness setting: it loses `--recommendation-retention` and the staleness override. An object without the stamp is served nothing until its controller records a `Computed` outcome for it.
- The stamp is an absolute time, so a wedged controller still stops injection, as the read-time bounds did.
- `--recommendation-retention` and `--reconcile-interval` must be greater than zero: the controller refuses to start otherwise. Retention has one meaning: the sweep keeps a Departed or Conflicted identity's object for exactly the window its Recommendation is stamped servable for.
- `wlrcache` owns every delete, the sweep, the release of a deleted Policy's objects and the reap of objects naming a Policy that no longer exists, each at the revision it judged. The controller supplies where each identity stands in its snapshot and the snapshot's instant, from which the sweep counts the grace period.

## Consequences

- Raising `--reconcile-interval` raises the staleness budget with it, so any interval keeps Recommendations servable between passes. A longer interval also lets a stuck controller's last Recommendations be served for longer: three intervals.
- The webhook and the controller cannot disagree on retention or staleness: only the controller has the settings, and the object carries their result.
- An object written before `status.servableUntil` existed is stale until its controller next records a `Computed` outcome for it: the next reconcile for a live identity with samples. One whose passes record nothing else, a Conflicted identity or a Departed one whose samples aged out, stays stale until it is computed again or swept, and its pods start on template resources. A Departed identity without an observed-resources snapshot is recorded by no pass, so its pods start on template resources until it runs again or is swept.
- Changing `--reconcile-interval` or `--recommendation-retention` restamps each object at its next status write: a live one within one refresh interval, a Departed or Conflicted one when its next record finds the stamp different.
- A webhook stub created during a long pass survives its sweep.
