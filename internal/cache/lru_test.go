package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestLRUGetMissReturnsFalse(t *testing.T) {
	c := NewLRU[string, int](10, time.Second, nil)
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected miss")
	}
}

func TestLRUGetHitWithinTTL(t *testing.T) {
	c := NewLRU[string, int](10, time.Second, nil)
	c.Set("k", 42)
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("expected hit with 42, got ok=%v v=%v", ok, v)
	}
}

func TestLRUExpiresAfterTTL(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	c := NewLRU[string, int](10, 30*time.Second, clk)
	c.Set("k", 1)

	clk.SetTime(epoch.Add(30 * time.Second))
	if _, ok := c.Get("k"); !ok {
		t.Fatal("expected a hit at exactly the TTL")
	}
	clk.SetTime(epoch.Add(31 * time.Second))
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected a miss past the TTL")
	}
}

// A negative result is cached as the value's zero, and must be a hit like
// any other value.
func TestLRUStoredZeroValueIsAHit(t *testing.T) {
	c := NewLRU[string, map[string]string](10, time.Second, nil)
	c.Set("k", nil)
	if v, ok := c.Get("k"); !ok || v != nil {
		t.Fatalf("expected a hit with nil, got ok=%v v=%v", ok, v)
	}
}

func TestLRUEvictsLeastRecentlyUsedWhenFull(t *testing.T) {
	c := NewLRU[string, int](2, time.Second, nil)
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

func TestLRUSetUpdatesValueAndTTL(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	c := NewLRU[string, int](2, time.Minute, clk)
	c.Set("k", 1)
	clk.SetTime(epoch.Add(50 * time.Second))
	c.Set("k", 2)
	clk.SetTime(epoch.Add(100 * time.Second))
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("expected the overwrite's value 2, fresh from its own Set, got ok=%v v=%v", ok, v)
	}
}

func TestLRUSetMakesKeyMostRecentlyUsed(t *testing.T) {
	c := NewLRU[string, int](2, time.Second, nil)
	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("a", 11)
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("expected b evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 11 {
		t.Fatalf("expected a=11 present, got ok=%v v=%v", ok, v)
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("expected c present")
	}
}

func TestLRUMaxOneEvictsOnSecondInsert(t *testing.T) {
	c := NewLRU[string, int](1, time.Second, nil)
	c.Set("a", 1)
	c.Set("b", 2)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expected a evicted")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("expected b=2 present, got ok=%v v=%v", ok, v)
	}
}

func TestLRULastGoodWithinServesExpiredButRecentEntry(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	c := NewLRU[string, int](10, time.Minute, clk)
	c.Set("k", 7)
	clk.SetTime(epoch.Add(5 * time.Minute))
	if _, ok := c.Get("k"); ok {
		t.Fatal("expected Get to miss past the TTL")
	}
	if v, ok := c.LastGoodWithin("k", 10*time.Minute); !ok || v != 7 {
		t.Fatalf("expected stale-but-recent hit 7, got ok=%v v=%v", ok, v)
	}
}

func TestLRULastGoodWithinRejectsTooOldEntry(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	c := NewLRU[string, int](10, time.Minute, clk)
	c.Set("k", 7)
	clk.SetTime(epoch.Add(11 * time.Minute))
	if _, ok := c.LastGoodWithin("k", 10*time.Minute); ok {
		t.Fatal("expected miss for entry older than maxAge")
	}
}

func TestLRULastGoodWithinMissOnAbsentKey(t *testing.T) {
	c := NewLRU[string, int](10, time.Minute, nil)
	if _, ok := c.LastGoodWithin("nope", 10*time.Minute); ok {
		t.Fatal("expected miss for absent key")
	}
}

// An expired entry is kept for LastGoodWithin only until LRU pressure
// reclaims it.
func TestLRUExpiredEntryIsReclaimedByLRUPressure(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	c := NewLRU[string, int](1, time.Minute, clk)
	c.Set("old", 1)
	clk.SetTime(epoch.Add(2 * time.Minute))
	c.Set("new", 2)
	if _, ok := c.LastGoodWithin("old", 10*time.Minute); ok {
		t.Fatal("expected the expired entry evicted by the insert that overflowed the cache")
	}
}

func TestLRUSetIfAbsentStoresOnlyWithoutAFreshEntry(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	c := NewLRU[string, int](10, 30*time.Second, clk)
	if !c.SetIfAbsent("k", 1) {
		t.Fatal("expected a store on an absent key")
	}
	clk.SetTime(epoch.Add(30 * time.Second))
	if c.SetIfAbsent("k", 2) {
		t.Fatal("expected no store over a fresh entry")
	}
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("expected the first value 1 kept, got ok=%v v=%v", ok, v)
	}
	clk.SetTime(epoch.Add(31 * time.Second))
	if !c.SetIfAbsent("k", 3) {
		t.Fatal("expected a store over an expired entry")
	}
	if v, ok := c.Get("k"); !ok || v != 3 {
		t.Fatalf("expected 3, fresh from its own store, got ok=%v v=%v", ok, v)
	}
}

func TestLRUSetIfAbsentStoresForExactlyOneOfRacingCallers(t *testing.T) {
	const n = 50
	c := NewLRU[string, int](10, time.Minute, nil)
	var stored atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if c.SetIfAbsent("k", i) {
				stored.Add(1)
			}
		})
	}
	wg.Wait()
	if got := stored.Load(); got != 1 {
		t.Errorf("%d of %d racing callers stored, want exactly 1", got, n)
	}
}

// A refused store is a use: the entry it found outlives colder ones under LRU
// pressure.
func TestLRUSetIfAbsentRefusalMakesKeyMostRecentlyUsed(t *testing.T) {
	c := NewLRU[string, int](2, time.Minute, nil)
	c.Set("a", 1)
	c.Set("b", 2)
	c.SetIfAbsent("a", 11)
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Fatal("expected b evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("expected a=1 present, got ok=%v v=%v", ok, v)
	}
}

func TestLRUDeleteIfRemovesOnlyAMatchingValue(t *testing.T) {
	c := NewLRU[string, int](10, time.Minute, nil)
	c.Set("k", 2)
	if c.DeleteIf("k", func(v int) bool { return v == 1 }) {
		t.Fatal("expected no delete of a value that does not match")
	}
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("expected k=2 kept, got ok=%v v=%v", ok, v)
	}
	if !c.DeleteIf("k", func(v int) bool { return v == 2 }) {
		t.Fatal("expected the matching value deleted")
	}
	if _, ok := c.LastGoodWithin("k", time.Hour); ok {
		t.Fatal("expected the entry gone, not merely expired")
	}
	if !c.SetIfAbsent("k", 3) {
		t.Fatal("expected a store once the entry is deleted")
	}
}

func TestLRUDeleteIfRemovesAnExpiredMatch(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	c := NewLRU[string, int](10, time.Minute, clk)
	c.Set("k", 1)
	clk.SetTime(epoch.Add(2 * time.Minute))
	if !c.DeleteIf("k", func(v int) bool { return v == 1 }) {
		t.Fatal("expected an expired matching entry deleted")
	}
	if _, ok := c.LastGoodWithin("k", time.Hour); ok {
		t.Fatal("expected the entry gone")
	}
}

func TestLRUDeleteIfMissOnAbsentKey(t *testing.T) {
	c := NewLRU[string, int](10, time.Minute, nil)
	if c.DeleteIf("nope", func(int) bool { return true }) {
		t.Fatal("expected no delete of an absent key")
	}
}

func TestNewLRUPanicsOnInvalidMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on maxEntries=0")
		}
	}()
	NewLRU[string, int](0, time.Second, nil)
}
