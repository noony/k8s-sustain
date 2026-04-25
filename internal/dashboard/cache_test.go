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
	c := NewCache(10, 50*time.Millisecond)
	c.Set("k", 1)
	time.Sleep(80 * time.Millisecond)
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

func TestNewCachePanicsOnInvalidMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on max=0")
		}
	}()
	NewCache(0, 1*time.Second)
}
