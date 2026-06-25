// Package oomwatch implements an active Pod-status watcher that detects
// OOM kills as they happen and exposes them as a fast in-memory signal source
// to the recommender. It complements the Prometheus-based 24h OOM rule path
// by closing the multi-minute latency window between an OOM kill and the next
// Policy reconcile.
package oomwatch

import (
	"context"
	"time"
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
	// PolicyName is the value of the k8s.sustain.io/policy annotation on the
	// source pod when the event was observed. Empty if the annotation was
	// missing (in which case the pod would not have been admitted to the
	// watcher in the first place; we keep the field for symmetry with the
	// EventHandler contract).
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
	Record(key Key, record OOMRecord) bool
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
