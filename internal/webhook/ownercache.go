package webhook

import (
	"container/list"
	"sync"
	"time"
)

// ownerAnnotationsCacheTTL bounds how long a cached owner-annotations lookup
// (see Handler.ownerAnnotations) is trusted before it is re-fetched.
//
// 30s is chosen against the read-amplification pattern this cache exists to
// fix: a rolling restart creates N pods behind the SAME owner in a burst, so
// even a short TTL collapses that burst from ~N Gets to ~1-2. It is
// deliberately much shorter than CacheStaleness (WorkloadRecommendation
// freshness) or RecommendationRetention — this cache is a load-shedding
// measure for a hot read, not a source of truth.
//
// Staleness window, stated honestly: a workload whose OWN metadata.annotations
// gains (or loses) the policy annotation may take up to this TTL to be seen
// by admission, because admission is reading a cached copy of that object's
// annotations rather than the live one. This is acceptable because the
// controller reconciles independently of admission and will pick up the
// change on its own schedule regardless of what the webhook does; a pod
// admitted with template resources during the staleness window is resized
// (in place, or via eviction) once the controller catches up, and the webhook
// injects correctly for every pod created after the TTL elapses.
const ownerAnnotationsCacheTTL = 30 * time.Second

// ownerAnnotationsCacheMaxEntries bounds the cache's memory footprint
// independent of TTL, so a cluster with many distinct owners (or a slow
// leak of never-reused keys) cannot grow this without bound between
// expiries. Entries are small NOT because a shallow copy of the owner's
// annotations map is small — an owner's full metadata.annotations can be
// arbitrarily large (a kubectl-apply-managed object carries
// kubectl.kubernetes.io/last-applied-configuration, the whole serialized
// object, routinely a few KB and up to Kubernetes' 256KB per-object
// annotation ceiling) — but because the populate site (optin.go's
// ownerAnnotations, via cacheableOwnerAnnotations) copies out only the two
// keys ResolvePolicy ever reads, sustainv1alpha1.PolicyAnnotation and
// OptOutAnnotation, before a value ever reaches set. A cached entry never
// holds more than those two short strings, so this ceiling is genuinely
// bounded rather than merely generous.
const ownerAnnotationsCacheMaxEntries = 4096

// ownerRefCacheTTL and ownerRefCacheMaxEntries govern Handler.ownerRefCache
// (see resolveCachedPodOwner in optin.go), the cache over the pod→owner
// ownerRef walk (workload.ResolveControllerOwner). Same values and the same
// reasoning as ownerAnnotationsCacheTTL/MaxEntries: a rolling restart is the
// read-amplification pattern both caches exist to bound, and there is no
// reason for the two to drift apart. Named separately (rather than reusing
// the ownerAnnotations* constants directly) so either can be retuned on its
// own later without implying the other must move too.
const (
	ownerRefCacheTTL        = ownerAnnotationsCacheTTL
	ownerRefCacheMaxEntries = ownerAnnotationsCacheMaxEntries
)

// ttlLRUCache is a small thread-safe LRU-with-TTL, generic over the cached
// value shape. It backs two independent caches in this package —
// Handler.ownerAnnCache (map[string]string) and Handler.ownerRefCache
// (resolvedOwnerRef) — that would otherwise be near-identical copies of the
// same container/list + map + one mutex, lazy-expiry-on-read pattern.
//
// It is not sigs.k8s.io or internal/dashboard.Cache verbatim: importing
// internal/dashboard from internal/webhook would be a backwards dependency
// (the dashboard is a read-only consumer, not something the admission hot
// path should depend on).
//
// Safe for concurrent use: admission handlers run concurrently, and every
// method takes the mutex. Bounded: the maxEntries passed to set on first use
// per instance evicts the least-recently-used entry on overflow — see get/set
// below; each instance is parameterised by its own TTL/max-entries constants
// at the call site rather than storing them on the struct, keeping the zero
// value simple. No goroutines: eviction of expired entries is lazy (on the
// next Get/Set that touches that slot, or via LRU pressure), never a
// background sweeper — nothing here needs a Stop or a context, and the
// package's goroutine-leak test has nothing to catch.
//
// The zero value is usable (get/set lazily initialise the map/list), because
// Handler — which embeds these by value — is built as a plain struct literal
// throughout the test suite and must keep working that way.
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
// zero value (e.g. nil map, or a struct zero value used as a legitimate
// negative result by the caller) is still a hit — ok is still true.
//
// The returned value is the same instance held by the cache entry, shared
// across every concurrent admission that hits this key — callers must treat
// it as read-only and never mutate it in place.
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
// entry if the cache is at or over maxEntries. value may be a nil map (or any
// other zero value) to cache a negative result. set does not copy value: the
// caller owns whatever it passes in and must not mutate or alias it elsewhere
// afterwards.
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

// ownerAnnotationsCache is Handler.ownerAnnCache's concrete type: see
// ownerAnnotations in optin.go for the populate site and
// cacheableOwnerAnnotations for why a cached value is always a small,
// private-to-the-cache map holding at most the two keys ResolvePolicy reads.
type ownerAnnotationsCache = ttlLRUCache[map[string]string]

// singleflightTestHook, if non-nil, is invoked with the singleflight key
// immediately after optin.go's DoChan call registers a caller (the leader or
// a follower, indistinguishably) with Handler.ownerAnnSF or Handler.ownerRefSF.
// DoChan registers the caller synchronously, under the group's own mutex,
// before it returns, so calling the hook only after DoChan returns is what
// makes "N concurrent callers have all joined the same in-flight resolution"
// an actually true statement by the time the hook fires — calling it before
// DoChan would let a caller be preempted between the hook and DoChan, so a
// barrier built on it could release while that caller had not joined yet. It
// exists purely so tests can deterministically detect that join without
// relying on sleeps or scheduler luck — see the burst-collapse tests in
// optin_test.go. Always nil outside tests.
var singleflightTestHook func(key string)

// resolvedOwnerRef is the value type cached by Handler.ownerRefCache: the
// (kind, name) pair workload.ResolveControllerOwner resolved for one
// controller ownerRef. A zero value ({"", ""}) is itself a legitimate
// negative result — the "no controller owner" case ResolveControllerOwner
// itself never actually returns (it always has a ref to work from, by
// construction of the caller), and orphan pods (no controller ownerRef at
// all) never reach the cache in the first place — see
// resolveCachedPodOwner's short-circuit in optin.go.
type resolvedOwnerRef struct {
	Kind, Name string
}
