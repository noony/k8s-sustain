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
// from "this is a brand new kill". When the tuple differs the newer record
// wins unconditionally so the cache always reflects the latest known state.
func (c *Cache) Record(key Key, record OOMRecord) bool {
	c.mu.Lock()
	if existing, ok := c.entries[key]; ok {
		if existing.RestartCount == record.RestartCount &&
			existing.TerminatedAt.Equal(record.TerminatedAt) {
			c.mu.Unlock()
			return false
		}
	} else if len(c.entries) >= c.maxEntries {
		// Cap reached on insert of a NEW key — evict the entry with the
		// oldest ObservedAt. Updates of existing keys do not count against
		// the cap so a hot-looping workload cannot starve other workloads.
		c.evictOldestLocked()
	}
	c.entries[key] = record
	c.addIndexLocked(key)
	n := len(c.entries)
	obs := c.SizeObserver
	c.mu.Unlock()
	if obs != nil {
		obs(n)
	}
	return true
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
