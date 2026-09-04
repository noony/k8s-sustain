package oomwatch

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// defaultTTL comfortably outlasts the recommender's typical reconcile interval
// without retaining stale data long enough to matter for memory on big clusters.
const defaultTTL = 30 * time.Minute

// minSweepInterval bounds the active-eviction tick so that very small TTLs
// (typical in tests) do not turn the sweeper into a hot loop in production.
const minSweepInterval = 30 * time.Second

// defaultMaxEntries caps the cache so a misbehaving fleet cannot grow the map
// without bound; on overflow the oldest entry by ObservedAt is evicted. High
// enough that real clusters will never hit it.
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
// It implements both Source (read by the recommender) and Sink (written by the
// watcher) so both sides share one dedup-aware map.
type Cache struct {
	ttl        time.Duration
	maxEntries int

	mu         sync.RWMutex
	entries    map[Key]OOMRecord
	byWorkload map[workloadKey]map[string]struct{}
	// resolved backs AlreadyResolved/MarkResolved: value is the mark time,
	// aged out under the same ttl as entries and guarded by the same mu.
	resolved map[resolvedEventKey]time.Time

	// SizeObserver, if set, is invoked with the current entry count after
	// every mutation. The controller wires it to the
	// k8s_sustain_oom_cache_entries gauge.
	//
	// The count is snapshotted under mu but called after Unlock: this
	// callback takes its own lock, so calling it under mu would serialize
	// every mutation behind it and open a mu -> observer deadlock order.
	// Concurrent mutations may therefore reach it out of order, which
	// self-corrects on the next mutation or sweep.
	SizeObserver func(int)

	runOnce sync.Once
}

// NewCache returns a Cache with the given retention TTL. A non-positive ttl
// falls back to defaultTTL rather than evicting on every read.
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

// Record upserts an observation, returning false only when the stored entry
// has the same (RestartCount, TerminatedAt) tuple — how the watcher tells an
// already-reported kill from a new one.
//
// A Key names a workload+container, not a pod, so every pod of the workload
// writes the same slot from parallel reconciles. Storage is therefore a
// per-field merge, not a winning record:
//
//   - Identity and kill timestamps come from the newest observation by
//     newerThan; without an explicit order "newest" degrades to "whichever
//     goroutine took the lock last".
//   - ObservedAt takes the later of the two regardless of which identity won.
//     It is the freshness clock sweep and RecentByWorkload age entries off,
//     so an out-of-order kill must still refresh the entry — otherwise it is
//     fanned out as new and then swept moments later on the older clock.
//   - OOMLimitBytes takes the max, because the useful memory-floor anchor is
//     the largest limit that still got OOM-killed. A workload bumped
//     128Mi -> 256Mi can have a stale 128Mi pod OOM after a resized 256Mi
//     one, and anchoring on the newer 128Mi would bump the floor to a value
//     256Mi just disproved.
//
// The accepted cost is a deliberate downsize: an older larger limit anchors
// the floor high until it ages out. That is bounded by the TTL, self-healing,
// and conservative. It cannot reintroduce the runaway fixed in 8c44b62, which
// compounded because the anchor came from our own spec output; every value
// merged here is a limit the kernel actually applied.
//
// A distinct-but-older observation contributes its limit but not its identity
// and still reports true: it is a real kill nothing has seen, and suppressing
// it would lose the signal whenever clock skew makes a new kill look older.
// Per-pod storm protection lives in AlreadyResolved/MarkResolved instead.
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
// This governs identity and timestamps only; Record merges OOMLimitBytes by
// max(). That split makes the tie-break harmless: restart counts from two pods
// of one workload are unrelated counters, so a cross-pod tie picks arbitrarily
// between two equally-recent identity stamps.
func newerThan(a, b OOMRecord) bool {
	if a.TerminatedAt.Equal(b.TerminatedAt) {
		return a.RestartCount > b.RestartCount
	}
	return a.TerminatedAt.After(b.TerminatedAt)
}

// AlreadyResolved implements Sink. See the interface doc for the contract.
func (c *Cache) AlreadyResolved(podUID types.UID, container string, restartCount int32, terminatedAt time.Time) bool {
	return c.alreadyResolvedAt(time.Now(), podUID, container, restartCount, terminatedAt)
}

// alreadyResolvedAt is AlreadyResolved with an injectable "now". Sink's
// signature is fixed, so the clock is threaded through like sweep(now)'s
// rather than held as a constructor argument.
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
// alreadyResolvedAt.
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

// Size returns the number of cached entries, including stale-but-not-yet-swept
// ones. For a fresh-only count, use RecentByWorkload.
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

// sweep drops every entry older than ttl, in both entries and resolved. now is
// a parameter so tests can drive eviction without a real ticker.
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
// hold mu in write mode. The O(N) scan is microseconds at the default cap; a
// min-heap would only add a structure to keep in sync on every mutation.
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

// evictOldestResolvedLocked drops the oldest resolved mark. Caller must hold
// mu in write mode. Same O(N) trade-off as evictOldestLocked.
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
