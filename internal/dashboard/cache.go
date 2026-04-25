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
	return &Cache{max: max, ttl: ttl, ll: list.New(), items: map[string]*list.Element{}}
}

// Get returns the cached value if present and not expired.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*cacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.ll.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return entry.value, true
}

// Set stores a value at the key, evicting the least-recently-used entry if full.
func (c *Cache) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*cacheEntry)
		entry.value = value
		entry.expiresAt = time.Now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}
	entry := &cacheEntry{key: key, value: value, expiresAt: time.Now().Add(c.ttl)}
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
