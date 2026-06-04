package dashboard

import (
	"testing"
	"time"
)

func TestCacheGetMissReturnsFalse(t *testing.T) {
	c := NewCache(10, 1*time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected miss")
	}
}

func TestCacheGetHitWithinTTL(t *testing.T) {
	c := NewCache(10, 1*time.Second)
	c.Set("k", 42)
	v, ok := c.Get("k")
	if !ok || v.(int) != 42 {
		t.Fatalf("expected hit with 42, got ok=%v v=%v", ok, v)
	}
}

func TestCacheExpiresAfterTTL(t *testing.T) {
	// Drive expiry deterministically off an injectable clock instead of
	// real-wall-clock time.Sleep — avoids a 50ms-TTL test flaking on a busy CI.
	now := time.Now()
	c := NewCache(10, 50*time.Millisecond)
	c.now = func() time.Time { return now }
	c.Set("k", 1)
	now = now.Add(80 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected expiry")
	}
}

func TestCacheEvictsLRUWhenFull(t *testing.T) {
	c := NewCache(2, 1*time.Second)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Get("a")
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("expected b evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected a present")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("expected c present")
	}
}

func TestCacheSetUpdatesValueAndTTL(t *testing.T) {
	c := NewCache(2, 1*time.Second)
	c.Set("k", 1)
	c.Set("k", 2)
	v, ok := c.Get("k")
	if !ok || v.(int) != 2 {
		t.Fatalf("expected updated value 2, got ok=%v v=%v", ok, v)
	}
}

func TestCacheSetUpdatesMakesKeyMRU(t *testing.T) {
	c := NewCache(2, 1*time.Second)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 11) // 'a' becomes MRU; 'b' is now LRU
	c.Set("c", 3)  // should evict 'b'
	if _, ok := c.Get("b"); ok {
		t.Fatal("expected b evicted")
	}
	if v, ok := c.Get("a"); !ok || v.(int) != 11 {
		t.Fatalf("expected a=11 present, got ok=%v v=%v", ok, v)
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("expected c present")
	}
}

func TestCacheMaxOneEvictsOnSecondInsert(t *testing.T) {
	c := NewCache(1, 1*time.Second)
	c.Set("a", 1)
	c.Set("b", 2)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expected a evicted")
	}
	v, ok := c.Get("b")
	if !ok || v.(int) != 2 {
		t.Fatalf("expected b=2 present, got ok=%v v=%v", ok, v)
	}
}

func TestGetLastGoodWithinServesStaleButRecentEntry(t *testing.T) {
	// An entry whose TTL has lapsed is still served as last-good provided it
	// was stored within maxAge.
	now := time.Now()
	c := NewCache(10, 60*time.Second)
	c.now = func() time.Time { return now }
	c.Set("k", 7)
	now = now.Add(5 * time.Minute) // well past the 60s TTL, but within maxAge
	// Note: deliberately do not call Get here — Get evicts the expired entry,
	// which would defeat the last-good fallback. GetLastGoodWithin must see it.
	v, ok := c.GetLastGoodWithin("k", 10*time.Minute)
	if !ok || v.(int) != 7 {
		t.Fatalf("expected stale-but-recent hit 7, got ok=%v v=%v", ok, v)
	}
}

func TestGetLastGoodWithinRejectsTooOldEntry(t *testing.T) {
	now := time.Now()
	c := NewCache(10, 60*time.Second)
	c.now = func() time.Time { return now }
	c.Set("k", 7)
	now = now.Add(11 * time.Minute) // beyond maxAge
	if _, ok := c.GetLastGoodWithin("k", 10*time.Minute); ok {
		t.Fatal("expected miss for entry older than maxAge")
	}
}

func TestGetLastGoodWithinMissOnAbsentKey(t *testing.T) {
	c := NewCache(10, 60*time.Second)
	if _, ok := c.GetLastGoodWithin("nope", 10*time.Minute); ok {
		t.Fatal("expected miss for absent key")
	}
}

func TestNewCachePanicsOnInvalidMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on max=0")
		}
	}()
	NewCache(0, 1*time.Second)
}
