package webhook

import (
	"container/list"
	"sync"
	"time"
)

// ownerAnnotationsCacheTTL bounds how long a cached owner-annotations lookup
// (see Handler.ownerAnnotations) is trusted. 30s is sized against the
// read-amplification this cache exists to fix — a rolling restart creates N
// pods behind the same owner in a burst — and is deliberately far shorter than
// CacheStaleness: this is load-shedding for a hot read, not a source of truth.
//
// The cost is that a workload gaining or losing the policy annotation may take
// up to this TTL to be seen by admission. Acceptable because the controller
// reconciles independently and resizes such a pod once it catches up.
const ownerAnnotationsCacheTTL = 30 * time.Second

// ownerAnnotationsCacheMaxEntries bounds the cache's footprint independent of
// TTL, so many distinct owners cannot grow it without bound between expiries.
// Entries stay small only because cacheableOwnerAnnotations copies out just the
// two keys ResolvePolicy reads — an owner's raw annotations can carry
// last-applied-configuration, up to Kubernetes' 256KB per-object ceiling.
const ownerAnnotationsCacheMaxEntries = 4096

// ownerRefCacheTTL and ownerRefCacheMaxEntries govern Handler.ownerRefCache
// (resolveCachedPodOwner in optin.go). Same values and reasoning as the
// ownerAnnotations pair — a rolling restart is the pattern both bound — but
// named separately so either can be retuned on its own.
const (
	ownerRefCacheTTL        = ownerAnnotationsCacheTTL
	ownerRefCacheMaxEntries = ownerAnnotationsCacheMaxEntries
)

// ttlLRUCache is a small thread-safe LRU-with-TTL backing both owner caches in
// this package. It does not reuse internal/dashboard.Cache: importing the
// dashboard from the admission hot path would be a backwards dependency.
//
// TTL and max-entries are passed per call rather than stored, so the zero value
// is usable — Handler embeds these by value and is built as a plain struct
// literal throughout the tests. Expiry is lazy, never a background sweeper, so
// there is nothing to Stop and nothing for the goroutine-leak test to catch.
type ttlLRUCache[V any] struct {
	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

type ttlLRUCacheEntry[V any] struct {
	key       string
	value     V
	expiresAt time.Time
}

// get returns the cached value for key if present and not expired. A stored
// zero value is still a hit. The returned value is the instance the entry
// holds, shared across concurrent admissions — callers must not mutate it.
func (c *ttlLRUCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	entry := el.Value.(*ttlLRUCacheEntry[V])
	if c.nowFn().After(entry.expiresAt) {
		return zero, false
	}
	c.ll.MoveToFront(el)
	return entry.value, true
}

// set stores value at key with the given ttl, evicting the least-recently-used
// entry when the cache is over maxEntries. A zero value caches a negative
// result. value is not copied — the caller must not mutate or alias it after.
func (c *ttlLRUCache[V]) set(key string, value V, ttl time.Duration, maxEntries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[string]*list.Element)
		c.ll = list.New()
	}
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*ttlLRUCacheEntry[V])
		entry.value = value
		entry.expiresAt = c.nowFn().Add(ttl)
		c.ll.MoveToFront(el)
		return
	}
	entry := &ttlLRUCacheEntry[V]{key: key, value: value, expiresAt: c.nowFn().Add(ttl)}
	el := c.ll.PushFront(entry)
	c.items[key] = el
	if c.ll.Len() > maxEntries {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*ttlLRUCacheEntry[V]).key)
		}
	}
}

func (c *ttlLRUCache[V]) nowFn() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// ownerAnnotationsCache is Handler.ownerAnnCache's concrete type; see
// cacheableOwnerAnnotations for why a cached value is always small.
type ownerAnnotationsCache = ttlLRUCache[map[string]string]

// resolvedOwnerRef is the (kind, name) pair workload.ResolveControllerOwner
// resolved for one controller ownerRef, as cached by Handler.ownerRefCache.
// Orphan pods never reach the cache — see resolveCachedPodOwner.
type resolvedOwnerRef struct {
	Kind, Name string
}
