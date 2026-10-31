// Package cache holds the in-process caches the webhook and the dashboard
// share: an LRU whose entries expire, and a read-through that fills it with
// one fetch per key at a time.
package cache

import (
	"container/list"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// LRU is a thread-safe least-recently-used cache whose entries expire ttl
// after they were stored. Expiry is lazy, never a background sweeper, so there
// is nothing to stop. An expired entry is a miss for Get but stays until a Set
// overwrites it or LRU pressure evicts it, which is what lets LastGoodWithin
// still serve it.
type LRU[K comparable, V any] struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	clock      clock.PassiveClock
	order      *list.List
	entries    map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key      K
	value    V
	storedAt time.Time
}

// NewLRU returns an LRU of at most maxEntries entries, each fresh for ttl as
// read on clk; a nil clk is the wall clock. It panics when maxEntries < 1,
// which would silently swallow every Set.
func NewLRU[K comparable, V any](maxEntries int, ttl time.Duration, clk clock.PassiveClock) *LRU[K, V] {
	if maxEntries < 1 {
		panic("cache.NewLRU: maxEntries must be >= 1")
	}
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &LRU[K, V]{
		maxEntries: maxEntries,
		ttl:        ttl,
		clock:      clk,
		order:      list.New(),
		entries:    map[K]*list.Element{},
	}
}

// Get returns the value stored at key while it is fresh, and makes the entry
// the most recently used. A stored zero value is a hit. The value is shared
// with every other caller: never mutate it.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.clock.Since(e.storedAt) > c.ttl {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

// LastGoodWithin returns the value stored at key if it was stored no longer
// than maxAge ago, fresh or expired, without touching its recency. It serves a
// stale-but-recent value when a fresh read fails, while bounding how stale
// that fallback may be so a sustained outage cannot pin an arbitrarily old
// value in front of clients.
func (c *LRU[K, V]) LastGoodWithin(key K, maxAge time.Duration) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.clock.Since(e.storedAt) > maxAge {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Set stores value at key, fresh for the TTL from now, and evicts the least
// recently used entry when the cache is over its size. The value is not
// copied: the caller must not mutate it afterwards.
func (c *LRU[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*entry[K, V])
		e.value, e.storedAt = value, now
		c.order.MoveToFront(el)
		return
	}
	c.entries[key] = c.order.PushFront(&entry[K, V]{key: key, value: value, storedAt: now})
	if c.order.Len() > c.maxEntries {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*entry[K, V]).key)
	}
}
