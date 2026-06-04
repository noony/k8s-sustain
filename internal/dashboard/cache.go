package dashboard

import (
	"container/list"
	"sync"
	"time"
)

// Cache is a thread-safe LRU with per-entry TTL.
// Use one Cache per logical endpoint family (e.g. /api/summary).
type Cache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	ll    *list.List
	items map[string]*list.Element
	now   func() time.Time // injectable for tests; defaults to time.Now
}

type cacheEntry struct {
	key       string
	value     any
	expiresAt time.Time
}

// NewCache returns a Cache with the given maximum size and TTL.
// Panics if max < 1 — a zero-or-negative size silently swallows every Set.
func NewCache(max int, ttl time.Duration) *Cache {
	if max < 1 {
		panic("dashboard.NewCache: max must be >= 1")
	}
	return &Cache{max: max, ttl: ttl, ll: list.New(), items: map[string]*list.Element{}, now: time.Now}
}

// Get returns the cached value if present and not expired.
//
// A TTL-expired entry reports a miss but is intentionally retained (not evicted)
// so GetLastGoodWithin can still serve it as a bounded stale-but-good fallback.
// Retained-but-expired entries are reclaimed naturally: overwritten by the next
// Set for the same key, or evicted by LRU pressure when the cache is full.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return entry.value, true
}

// GetLastGoodWithin returns the cached value for the key if an entry exists and
// was stored no longer than maxAge ago, ignoring normal TTL expiry. It serves a
// stale-but-recent value when a fresh recompute partially fails, while still
// bounding how stale that fallback may be so a sustained outage cannot pin an
// arbitrarily-old snapshot in front of clients. Unlike Get it does not evict
// expired entries and does not bump LRU recency.
//
// The entry's store time is derived from its expiry minus the cache TTL
// (expiresAt == storedAt + ttl), so no extra timestamp field is needed.
func (c *Cache) GetLastGoodWithin(key string, maxAge time.Duration) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*cacheEntry)
	storedAt := entry.expiresAt.Add(-c.ttl)
	if c.now().Sub(storedAt) > maxAge {
		return nil, false
	}
	return entry.value, true
}

// Set stores a value at the key, evicting the least-recently-used entry if full.
func (c *Cache) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*cacheEntry)
		entry.value = value
		entry.expiresAt = c.now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}
	entry := &cacheEntry{key: key, value: value, expiresAt: c.now().Add(c.ttl)}
	el := c.ll.PushFront(entry)
	c.items[key] = el
	if c.ll.Len() > c.max {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*cacheEntry).key)
		}
	}
}
