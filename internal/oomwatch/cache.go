package oomwatch

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// defaultTTL is the fallback retention horizon when the caller passes a
// non-positive ttl to NewCache. Picked to comfortably outlast the recommender's
// typical reconcile interval while not retaining stale data for so long that
// memory growth becomes a concern on large clusters.
const defaultTTL = 30 * time.Minute

// minSweepInterval bounds the active-eviction tick so that very small TTLs
// (typical in tests) do not turn the sweeper into a hot loop in production.
const minSweepInterval = 30 * time.Second

// defaultMaxEntries caps the cache so a misbehaving fleet (thousands of pods
// OOMing into distinct workloads) cannot grow the map without bound. When the
// cap is hit, the oldest entry by ObservedAt is evicted to make room. Picked
// high enough that real clusters will never hit it.
const defaultMaxEntries = 5_000

// workloadKey is the (ns, kind, name) tuple used as the secondary-index key so
// RecentByWorkload is O(containers) instead of O(N).
type workloadKey struct {
	Namespace string
	OwnerKind string
	OwnerName string
}

func workloadKeyOf(k Key) workloadKey {
	return workloadKey{Namespace: k.Namespace, OwnerKind: k.OwnerKind, OwnerName: k.OwnerName}
}

// resolvedEventKey identifies one specific container termination on one
// specific pod — pod UID + container + restart count + terminated-at —
// independent of whatever workload (if any) it resolves to. See
// Cache.AlreadyResolved / Cache.MarkResolved and the Sink interface doc for
// why this cannot simply reuse Key.
type resolvedEventKey struct {
	PodUID       types.UID
	Container    string
	RestartCount int32
	TerminatedAt time.Time
}

// Cache is an in-memory store of OOM observations keyed by workload+container.
// It implements both Source (read API consumed by the recommender) and Sink
// (write API consumed by the watcher), letting both sides share a single
// dedup-aware map without leaking their concrete dependency on each other.
type Cache struct {
	ttl        time.Duration
	maxEntries int

	mu         sync.RWMutex
	entries    map[Key]OOMRecord
	byWorkload map[workloadKey]map[string]struct{}
	// resolved backs AlreadyResolved/MarkResolved: value is the time the
	// event was marked resolved, aged out under the same ttl as entries (see
	// sweep and evictOldestResolvedLocked). Guarded by the same mu as
	// entries/byWorkload rather than a separate lock — there is no hot path
	// here that needs them to be independent, and one lock is one less thing
	// to get wrong.
	resolved map[resolvedEventKey]time.Time

	// SizeObserver, if set, is invoked with the current entry count after
	// every mutation (Record, sweep). The controller wires it to the
	// k8s_sustain_oom_cache_entries gauge.
	//
	// The count is snapshotted under mu but the call is made after Unlock,
	// on purpose: this is a caller-supplied callback that ends in a
	// Prometheus gauge write (its own lock), and invoking it under mu would
	// both serialize every cache mutation behind it and create a
	// mu -> observer lock order that a callback touching the cache could
	// close into a deadlock. The cost is that two concurrent mutations may
	// reach the observer out of order, leaving the gauge off by the handful
	// of entries between them; it self-corrects on the next mutation and at
	// worst on the next sweep tick, which is well within what a size gauge
	// needs to be good for.
	SizeObserver func(int)

	runOnce sync.Once
}

// NewCache returns a Cache with the given retention TTL. A non-positive ttl is
// silently replaced by defaultTTL so that misconfigured callers still get a
// working cache instead of one that evicts on every read.
func NewCache(ttl time.Duration) *Cache {
	return NewCacheWithLimit(ttl, defaultMaxEntries)
}

// NewCacheWithLimit is NewCache with an explicit max-entries cap. A non-positive
// limit is replaced by defaultMaxEntries. Tests use this to force eviction with
// a tiny cap without waiting on TTL.
func NewCacheWithLimit(ttl time.Duration, maxEntries int) *Cache {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	return &Cache{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[Key]OOMRecord),
		byWorkload: make(map[workloadKey]map[string]struct{}),
		resolved:   make(map[resolvedEventKey]time.Time),
	}
}

// Record upserts an observation. It returns false when the existing entry for
// the same Key has the same (RestartCount, TerminatedAt) tuple, which is how
// the watcher distinguishes "we already told the controller about this kill"
// from "this is a brand new kill"; any other tuple reports true.
//
// Storage is a PER-FIELD MERGE, not a winning record. A Key names a
// workload+container, not a pod, so every pod of the workload writes to the
// same slot and the watcher reconciles pods in parallel
// (maxConcurrentReconciles > 1) — and the slot carries three kinds of field
// whose merge rules differ:
//
//   - Identity and kill timestamps (PodName, PodUID, TerminatedAt,
//     RestartCount, PolicyName) come from the newest observation, ordered by
//     newerThan. Without an explicit ordering "newest" would silently degrade
//     to "whichever goroutine took the lock last".
//   - ObservedAt takes the LATER of the two, independently of which identity
//     won. It is the cache's freshness clock — sweep and RecentByWorkload
//     both age entries off it — not a kill-ordering field, so an observation
//     made now has to refresh the entry it just contributed to even when its
//     TerminatedAt is older. Taking it from the identity winner instead would
//     let an out-of-order kill be fanned out as new and then swept moments
//     later on the older stored entry's clock, dropping the memory-floor
//     evidence for a kill observed seconds earlier.
//   - OOMLimitBytes — the kernel-applied memory limit at the moment of the
//     kill, which the recommender anchors its OOM memory floor on — takes the
//     MAX of the incoming and stored values, because the anchor that matters
//     is the largest limit that still got OOM-killed. Recency is the wrong
//     rule here: a workload whose limit was bumped 128Mi -> 256Mi can have an
//     old un-resized pod (128Mi) OOM *after* an already-resized one (256Mi),
//     and anchoring on the newer 128Mi would bump the floor to a value 256Mi
//     just proved insufficient. internal/controller/recommendation_build.go
//     already max()es this record against the Prometheus anchor for exactly
//     that reason; keeping only the newest limit here discarded evidence the
//     consumer would have maxed anyway.
//
// The one accepted cost is on a deliberate downsize: after the limit is
// lowered on purpose, an older larger limit keeps anchoring the floor high
// until it ages out. That is bounded (the cache TTL, and RecentByWorkload's
// maxAge window on top of it) and self-healing, and it errs in the
// conservative direction — a brief over-provision rather than an under-bump
// into another OOM. It does not reintroduce the runaway fixed in 8c44b62:
// that one compounded because the anchor was read from our own spec output,
// whereas every value merged here is a limit the kernel actually applied.
//
// A distinct-but-older observation therefore contributes its limit but not its
// identity, and still reports true. That is deliberate: it is a real kill this
// cache had never seen, so the watcher should still fan it out and trigger an
// immediate reconcile — the same thing it did before ordering was enforced.
// Suppressing it would lose the signal outright whenever node clock skew makes
// a genuinely new kill on another pod look older. Reporting true costs at most
// one extra (idempotent) Policy reconcile, and the storm protection that
// actually matters lives one level up in AlreadyResolved/MarkResolved, which
// dedups per pod UID before Reconcile ever reaches this call.
func (c *Cache) Record(key Key, record OOMRecord) bool {
	c.mu.Lock()
	merged := record
	if existing, ok := c.entries[key]; ok {
		if existing.RestartCount == record.RestartCount &&
			existing.TerminatedAt.Equal(record.TerminatedAt) {
			// Same kill as the stored one: report the duplicate, but never
			// let it lower the anchor — the stored value may have been maxed
			// in from another pod, and a repeat carrying a larger limit is
			// still evidence that limit died.
			if record.OOMLimitBytes > existing.OOMLimitBytes {
				existing.OOMLimitBytes = record.OOMLimitBytes
				c.entries[key] = existing
			}
			c.mu.Unlock()
			return false
		}
		if !newerThan(record, existing) {
			// Distinct kill, but out of order: keep the newer entry's
			// identity and report the observation as new anyway.
			merged = existing
		}
		// Whichever identity won, the anchor is the largest killed limit —
		// so it is taken from both sides, not from the winner.
		merged.OOMLimitBytes = max(record.OOMLimitBytes, existing.OOMLimitBytes)
		// And the freshness clock is the later of the two: this observation
		// happened now, so the entry it just contributed to gets a full TTL
		// from here even when the kill it carries is the older one.
		merged.ObservedAt = laterOf(record.ObservedAt, existing.ObservedAt)
	} else if len(c.entries) >= c.maxEntries {
		// Cap reached on insert of a NEW key — evict the entry with the
		// oldest ObservedAt. Updates of existing keys do not count against
		// the cap so a hot-looping workload cannot starve other workloads.
		c.evictOldestLocked()
	}
	c.entries[key] = merged
	c.addIndexLocked(key)
	n := len(c.entries)
	obs := c.SizeObserver
	c.mu.Unlock()
	if obs != nil {
		obs(n)
	}
	return true
}

// laterOf returns the later of two wall-clock times. Used for ObservedAt,
// which tracks when the watcher last saw evidence for an entry rather than
// when the kill happened, so it advances on any observation.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// newerThan reports whether a is a strictly later observation than b, using
// (TerminatedAt, RestartCount) as a lexicographic order. RestartCount only
// breaks ties on an equal TerminatedAt, which TerminatedAt's metav1.Time
// origin makes common: it is truncated to whole seconds, so two kills inside
// one second are indistinguishable by time alone.
//
// This ordering governs the record's identity and timestamps ONLY — see
// Record, which merges OOMLimitBytes by max() instead of taking it from the
// winner. That split is what makes the tie-break harmless: restart counts from
// two different pods of one workload are unrelated counters, so on a cross-pod
// tie this picks arbitrarily between two equally-recent identity stamps, and
// nothing the recommender computes from depends on which one it picks.
func newerThan(a, b OOMRecord) bool {
	if a.TerminatedAt.Equal(b.TerminatedAt) {
		return a.RestartCount > b.RestartCount
	}
	return a.TerminatedAt.After(b.TerminatedAt)
}

// AlreadyResolved implements Sink. See the interface doc for the full
// contract; this is a plain TTL-bounded lookup guarded by the same lock as
// entries/byWorkload.
func (c *Cache) AlreadyResolved(podUID types.UID, container string, restartCount int32, terminatedAt time.Time) bool {
	return c.alreadyResolvedAt(time.Now(), podUID, container, restartCount, terminatedAt)
}

// alreadyResolvedAt is AlreadyResolved with an injectable "now", following
// the same explicit-parameter pattern sweep(now) already establishes for
// testing this type deterministically (Sink's interface signature is fixed,
// so the clock can't be a constructor argument the way ttlLRUCache's is in
// internal/webhook — it has to be threaded through like sweep's is).
func (c *Cache) alreadyResolvedAt(now time.Time, podUID types.UID, container string, restartCount int32, terminatedAt time.Time) bool {
	key := resolvedEventKey{PodUID: podUID, Container: container, RestartCount: restartCount, TerminatedAt: terminatedAt}
	c.mu.RLock()
	defer c.mu.RUnlock()
	at, ok := c.resolved[key]
	return ok && now.Sub(at) <= c.ttl
}

// MarkResolved implements Sink. Bounded the same way entries is: on insert
// of a genuinely new key past maxEntries, the oldest resolved mark is
// evicted to make room, so a fleet with many distinct chronically-restarting
// pods cannot grow this map without bound between TTL sweeps.
func (c *Cache) MarkResolved(podUID types.UID, container string, restartCount int32, terminatedAt time.Time) {
	c.markResolvedAt(time.Now(), podUID, container, restartCount, terminatedAt)
}

// markResolvedAt is MarkResolved with an injectable "now" — see
// alreadyResolvedAt's doc for why this mirrors sweep(now) instead of a
// struct-level clock field.
func (c *Cache) markResolvedAt(now time.Time, podUID types.UID, container string, restartCount int32, terminatedAt time.Time) {
	key := resolvedEventKey{PodUID: podUID, Container: container, RestartCount: restartCount, TerminatedAt: terminatedAt}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.resolved[key]; !exists && len(c.resolved) >= c.maxEntries {
		c.evictOldestResolvedLocked()
	}
	c.resolved[key] = now
}

// RecentByWorkload returns a per-container map of fresh records for the given
// workload identity. The map is always non-nil so callers can range over it
// without a nil check; an empty map means "no fresh observations".
//
// Reads run under RLock with the help of a secondary index keyed by workload,
// so the recommender does not serialize against concurrent Record calls. Any
// stale entries discovered along the way are deleted via a brief write lock
// after the read pass completes.
func (c *Cache) RecentByWorkload(ns, kind, name string, maxAge time.Duration) map[string]*OOMRecord {
	out := make(map[string]*OOMRecord)
	now := time.Now()
	wk := workloadKey{Namespace: ns, OwnerKind: kind, OwnerName: name}

	var stale []Key
	c.mu.RLock()
	for container := range c.byWorkload[wk] {
		key := Key{Namespace: ns, OwnerKind: kind, OwnerName: name, Container: container}
		entry, ok := c.entries[key]
		if !ok {
			continue
		}
		if now.Sub(entry.ObservedAt) > c.ttl {
			stale = append(stale, key)
			continue
		}
		if now.Sub(entry.ObservedAt) > maxAge {
			continue
		}
		rec := entry
		out[container] = &rec
	}
	c.mu.RUnlock()

	if len(stale) > 0 {
		c.mu.Lock()
		for _, key := range stale {
			c.deleteIfStaleLocked(key, now)
		}
		c.mu.Unlock()
	}
	return out
}

// Size returns the current number of cached entries, including any that may
// be stale but not yet evicted. Intended for the metrics gauge; callers that
// need an "only fresh" count should iterate via RecentByWorkload instead.
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Run starts the active sweeper that drops entries older than ttl. It blocks
// until ctx is canceled. Subsequent calls return immediately so a misuse (two
// callers wiring Run as a manager Runnable) does not silently leak a goroutine
// blocked on its own ctx — the first call owns the lifetime.
func (c *Cache) Run(ctx context.Context) {
	started := false
	c.runOnce.Do(func() { started = true })
	if !started {
		return
	}

	interval := max(c.ttl/2, minSweepInterval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c.sweep(now)
		}
	}
}

// sweep drops every entry older than ttl, in both entries and resolved.
// Exposed (unexported) so tests can drive eviction deterministically without
// waiting on a real ticker.
func (c *Cache) sweep(now time.Time) {
	c.mu.Lock()
	for key := range c.entries {
		c.deleteIfStaleLocked(key, now)
	}
	for key, at := range c.resolved {
		if now.Sub(at) > c.ttl {
			delete(c.resolved, key)
		}
	}
	n := len(c.entries)
	obs := c.SizeObserver
	c.mu.Unlock()
	if obs != nil {
		obs(n)
	}
}

func (c *Cache) addIndexLocked(key Key) {
	wk := workloadKeyOf(key)
	set, ok := c.byWorkload[wk]
	if !ok {
		set = make(map[string]struct{})
		c.byWorkload[wk] = set
	}
	set[key.Container] = struct{}{}
}

func (c *Cache) removeIndexLocked(key Key) {
	wk := workloadKeyOf(key)
	set, ok := c.byWorkload[wk]
	if !ok {
		return
	}
	delete(set, key.Container)
	if len(set) == 0 {
		delete(c.byWorkload, wk)
	}
}

// evictOldestLocked drops the entry with the smallest ObservedAt. Caller must
// hold mu in write mode. O(N) — at the default cap (a few thousand entries) a
// full scan is on the order of microseconds; a min-heap would shave it to
// O(log N) but at the cost of an auxiliary structure to keep in sync with
// entries on every mutation.
func (c *Cache) evictOldestLocked() {
	var oldestKey Key
	var oldestAt time.Time
	first := true
	for key, entry := range c.entries {
		if first || entry.ObservedAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = entry.ObservedAt
			first = false
		}
	}
	if !first {
		c.deleteEntryLocked(oldestKey)
	}
}

// evictOldestResolvedLocked drops the resolved mark with the smallest
// mark-time. Caller must hold mu in write mode. Same O(N) full-scan trade-off
// as evictOldestLocked, and for the same reason: cheap enough at this cap not
// to warrant an auxiliary heap.
func (c *Cache) evictOldestResolvedLocked() {
	var oldestKey resolvedEventKey
	var oldestAt time.Time
	first := true
	for key, at := range c.resolved {
		if first || at.Before(oldestAt) {
			oldestKey = key
			oldestAt = at
			first = false
		}
	}
	if !first {
		delete(c.resolved, oldestKey)
	}
}

// deleteEntryLocked removes a single entry and keeps the secondary index in
// sync. The single chokepoint guarantees byWorkload never holds keys whose
// entries row has been deleted.
func (c *Cache) deleteEntryLocked(key Key) {
	delete(c.entries, key)
	c.removeIndexLocked(key)
}

// deleteIfStaleLocked is the shared lazy-eviction primitive used by
// RecentByWorkload and sweep. The re-check under the write lock handles the
// TOCTOU window where a concurrent Record refreshed the entry between the
// initial RLock read and the Lock upgrade.
func (c *Cache) deleteIfStaleLocked(key Key, now time.Time) {
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	if now.Sub(entry.ObservedAt) > c.ttl {
		c.deleteEntryLocked(key)
	}
}
