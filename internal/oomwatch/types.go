// Package oomwatch implements an active Pod-status watcher that detects
// OOM kills as they happen and exposes them as a fast in-memory signal source
// to the recommender. It complements the Prometheus-based 24h OOM rule path
// by closing the multi-minute latency window between an OOM kill and the next
// Policy reconcile.
package oomwatch

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// DefaultRecentMaxAge is the freshness window the recommender uses when
// LiveOOMConfig.MaxAge is zero. It matches defaultTTL so a record that the
// recommender would still consider live cannot have been swept by the cache.
const DefaultRecentMaxAge = defaultTTL

// Key identifies an OOM record by workload + container.
type Key struct {
	Namespace string
	OwnerKind string
	OwnerName string
	Container string
}

// OOMRecord is the in-memory representation of an observed OOM kill.
type OOMRecord struct {
	// Container is the container name within the pod that was OOM-killed.
	Container string
	// PolicyName is the name of the Policy the source pod's workload opted
	// into, resolved across all three annotation levels — pod template,
	// owning workload, Namespace (see policymatch.ResolvePolicy) — not
	// necessarily the pod's own annotation. Never empty on a recorded event:
	// Reconcile returns before recording when resolution yields no policy at
	// any level.
	PolicyName string
	// ObservedAt is the wall-clock time at which the watcher first saw the
	// kill. Distinct from TerminatedAt because the watcher may lag a few
	// scrape intervals behind the actual kill.
	ObservedAt time.Time
	// TerminatedAt is pod.Status.ContainerStatuses[*].LastTerminationState
	// .Terminated.FinishedAt — the kernel-reported time the container exited.
	TerminatedAt time.Time
	// RestartCount is the container restart count at the moment we observed
	// the OOM. Used together with TerminatedAt as a dedup key so we only emit
	// one record per distinct kill.
	RestartCount int32
	// PodName / PodUID identify the most-recent source pod. The watcher only
	// keeps the latest pod that contributed to this record.
	PodName string
	PodUID  string
	// OOMLimitBytes is the container's memory limit at the time of the kill,
	// read from container.Resources.Limits.Memory() on the source pod spec.
	// Zero when the container had no memory limit (uncommon for OOMKilled but
	// possible if the kill was triggered by the node-level cgroup).
	OOMLimitBytes int64
}

// Source is the read-only API consumed by the recommender. It returns recent
// OOM observations for a given workload or container, filtered by maxAge.
type Source interface {
	// RecentByWorkload returns all per-container records for a workload that
	// are younger than maxAge. Returns an empty map when there is nothing.
	RecentByWorkload(ns, kind, name string, maxAge time.Duration) map[string]*OOMRecord
}

// Sink is the write-only API used by the watcher to persist observations.
// The Cache type implements both Sink and Source; keeping them separate lets
// the watcher be unit-tested against a fake sink without importing the cache.
type Sink interface {
	// Record stores an OOM observation. Returns true when the record is new
	// (i.e. distinct from any existing entry for the same Key by
	// RestartCount + TerminatedAt). Returns false for duplicates so the
	// watcher can skip notifying downstream subscribers.
	//
	// A Key names a workload+container, so concurrent reconciles of several
	// pods of one workload write to it. What the Key retains is the newest
	// observation by (TerminatedAt, RestartCount), never simply the
	// last-written one; an out-of-order observation is still reported as new
	// (it is a real kill nothing has seen) but does not displace the newer
	// entry. See Cache.Record for the full rationale.
	Record(key Key, record OOMRecord) bool

	// AlreadyResolved reports whether this exact container termination —
	// identified by pod UID + container name + restart count + terminated-at,
	// all readable straight off the Pod object with zero apiserver calls —
	// has already been fully resolved by an earlier Reconcile pass, whether
	// or not that pass found a managing Policy.
	//
	// This exists so Reconcile can skip the owner/Namespace walk on a repeat
	// status event for a kill it has already dealt with: Pod status updates
	// fire on every unrelated field change (readiness, IP, ...), but
	// LastTerminationState only changes on an actual new container restart,
	// so RestartCount+TerminatedAt staying put means nothing new happened.
	// It cannot reuse Record's own Key-based dedup because that Key names
	// the resolved workload — the very thing the owner walk this check
	// exists to skip would have to produce first.
	//
	// Never marks anything; MarkResolved does that. Keeping the check and
	// the mark as two calls (rather than one Record-style check-and-mark)
	// matters for correctness: Reconcile only calls MarkResolved after the
	// owner/Namespace reads have actually succeeded, so a transient read
	// failure — which returns an error and lets controller-runtime retry
	// with backoff — gets retried for real instead of being permanently
	// suppressed by a mark made before resolution was known to succeed.
	AlreadyResolved(podUID types.UID, container string, restartCount int32, terminatedAt time.Time) bool

	// MarkResolved records that this exact container termination (see
	// AlreadyResolved) has now been resolved. Called once per fresh
	// termination, only after the owner and Namespace reads that resolution
	// needed have returned without error — see AlreadyResolved's doc for why
	// that ordering is load-bearing.
	MarkResolved(podUID types.UID, container string, restartCount int32, terminatedAt time.Time)
}

// EventHandler is invoked by the watcher whenever a new (deduped) OOM record
// is added to the cache. The controller wires this to a source.Channel that
// enqueues the owning Policy for immediate reconciliation.
//
// Implementations must not block; the watcher calls this synchronously from
// its reconcile path.
type EventHandler interface {
	OnOOMDetected(ctx context.Context, key Key, record OOMRecord)
}

// EventHandlerFunc adapts an ordinary function to EventHandler.
type EventHandlerFunc func(ctx context.Context, key Key, record OOMRecord)

// OnOOMDetected implements EventHandler.
func (f EventHandlerFunc) OnOOMDetected(ctx context.Context, key Key, record OOMRecord) {
	f(ctx, key, record)
}
