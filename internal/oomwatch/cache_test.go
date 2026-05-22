package oomwatch

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func makeKey(container string) Key {
	return Key{
		Namespace: "default",
		OwnerKind: "Deployment",
		OwnerName: "app",
		Container: container,
	}
}

func makeRecord(restart int32, terminated time.Time) OOMRecord {
	return OOMRecord{
		Container:     "web",
		PolicyName:    "policy-a",
		ObservedAt:    time.Now(),
		TerminatedAt:  terminated,
		RestartCount:  restart,
		PodName:       "pod-1",
		PodUID:        "uid-1",
		OOMLimitBytes: 256 * 1024 * 1024,
	}
}

func TestNewCacheDefaultsTTL(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero ttl falls back to default", 0, defaultTTL},
		{"negative ttl falls back to default", -5 * time.Minute, defaultTTL},
		{"positive ttl is kept", 2 * time.Minute, 2 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewCache(tc.in).ttl; got != tc.want {
				t.Errorf("ttl = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecordDedup(t *testing.T) {
	t0 := time.Now()
	cases := []struct {
		name     string
		seed     []OOMRecord
		write    OOMRecord
		wantNew  bool
		wantSize int
	}{
		{
			name:     "first write is new",
			write:    makeRecord(1, t0),
			wantNew:  true,
			wantSize: 1,
		},
		{
			name:     "exact duplicate dedups",
			seed:     []OOMRecord{makeRecord(1, t0)},
			write:    makeRecord(1, t0),
			wantNew:  false,
			wantSize: 1,
		},
		{
			name:     "differing RestartCount is new",
			seed:     []OOMRecord{makeRecord(1, t0)},
			write:    makeRecord(2, t0),
			wantNew:  true,
			wantSize: 1,
		},
		{
			name:     "differing TerminatedAt is new",
			seed:     []OOMRecord{makeRecord(2, t0)},
			write:    makeRecord(2, t0.Add(time.Second)),
			wantNew:  true,
			wantSize: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(time.Minute)
			key := makeKey("web")
			for _, s := range tc.seed {
				c.Record(key, s)
			}
			got := c.Record(key, tc.write)
			if got != tc.wantNew {
				t.Errorf("Record() = %v, want %v", got, tc.wantNew)
			}
			if c.Size() != tc.wantSize {
				t.Errorf("Size() = %d, want %d", c.Size(), tc.wantSize)
			}
		})
	}
}

func TestRecordEvictsOldestAtCap(t *testing.T) {
	c := NewCacheWithLimit(time.Hour, 2)
	now := time.Now()

	k1 := makeKey("a")
	rec1 := makeRecord(1, now)
	rec1.ObservedAt = now.Add(-5 * time.Minute)
	c.Record(k1, rec1)

	k2 := makeKey("b")
	rec2 := makeRecord(1, now)
	rec2.ObservedAt = now.Add(-1 * time.Minute)
	c.Record(k2, rec2)

	if c.Size() != 2 {
		t.Fatalf("Size() = %d, want 2", c.Size())
	}

	// Inserting a third key triggers eviction of the oldest (rec1, container "a").
	k3 := makeKey("c")
	rec3 := makeRecord(1, now)
	rec3.ObservedAt = now
	c.Record(k3, rec3)

	if c.Size() != 2 {
		t.Fatalf("Size() after cap eviction = %d, want 2", c.Size())
	}
	if got := c.Recent("default", "Deployment", "app", "a", time.Hour); got != nil {
		t.Errorf("oldest entry still present after eviction: %+v", got)
	}
	if got := c.Recent("default", "Deployment", "app", "b", time.Hour); got == nil {
		t.Errorf("middle entry missing after eviction")
	}
	if got := c.Recent("default", "Deployment", "app", "c", time.Hour); got == nil {
		t.Errorf("newest entry missing after eviction")
	}
}

func TestRecordUpdateDoesNotCountAgainstCap(t *testing.T) {
	c := NewCacheWithLimit(time.Hour, 1)
	key := makeKey("web")
	c.Record(key, makeRecord(1, time.Now()))
	// Update of the same key must not evict; only NEW keys trigger eviction.
	c.Record(key, makeRecord(2, time.Now().Add(time.Second)))
	if c.Size() != 1 {
		t.Fatalf("Size() = %d, want 1 after update of same key", c.Size())
	}
}

func TestSecondaryIndexCleanupOnEviction(t *testing.T) {
	c := NewCacheWithLimit(50*time.Millisecond, 100)
	key := makeKey("web")
	rec := makeRecord(1, time.Now())
	rec.ObservedAt = time.Now().Add(-time.Hour) // already stale
	c.Record(key, rec)

	// RecentByWorkload must not see the stale entry and must drop it.
	got := c.RecentByWorkload("default", "Deployment", "app", time.Minute)
	if len(got) != 0 {
		t.Errorf("RecentByWorkload returned %d entries, want 0", len(got))
	}
	if c.Size() != 0 {
		t.Errorf("Size() = %d, want 0 after lazy eviction", c.Size())
	}
	// Index for that workload should also be gone, so a second call still
	// returns empty without scanning a stale set.
	wk := workloadKey{Namespace: "default", OwnerKind: "Deployment", OwnerName: "app"}
	c.mu.RLock()
	_, present := c.byWorkload[wk]
	c.mu.RUnlock()
	if present {
		t.Errorf("byWorkload index still holds an empty set for evicted workload")
	}
}

func TestRecentUnknownKey(t *testing.T) {
	c := NewCache(time.Minute)
	if got := c.Recent("ns", "Deployment", "app", "web", time.Minute); got != nil {
		t.Fatalf("Recent on empty cache returned %+v, want nil", got)
	}
}

func TestRecentStaleByMaxAge(t *testing.T) {
	c := NewCache(time.Hour)
	key := makeKey("web")
	rec := makeRecord(1, time.Now())
	rec.ObservedAt = time.Now().Add(-5 * time.Minute)
	c.Record(key, rec)

	// maxAge < age -> filtered out, but ttl > age -> still present in map.
	if got := c.Recent(key.Namespace, key.OwnerKind, key.OwnerName, key.Container, time.Minute); got != nil {
		t.Fatalf("Recent returned stale entry: %+v", got)
	}
	if c.Size() != 1 {
		t.Fatalf("maxAge filter must not evict; size=%d", c.Size())
	}

	// maxAge > age -> returned.
	if got := c.Recent(key.Namespace, key.OwnerKind, key.OwnerName, key.Container, 10*time.Minute); got == nil {
		t.Fatalf("Recent did not return fresh entry")
	}
}

func TestRecentLazyEvictionOnTTL(t *testing.T) {
	c := NewCache(time.Second)
	key := makeKey("web")
	rec := makeRecord(1, time.Now())
	rec.ObservedAt = time.Now().Add(-time.Hour)
	c.Record(key, rec)

	if got := c.Recent(key.Namespace, key.OwnerKind, key.OwnerName, key.Container, time.Hour); got != nil {
		t.Fatalf("stale entry should be evicted, got %+v", got)
	}
	if c.Size() != 0 {
		t.Fatalf("lazy eviction did not run; size=%d", c.Size())
	}
}

func TestRecentReturnsCopy(t *testing.T) {
	c := NewCache(time.Minute)
	key := makeKey("web")
	c.Record(key, makeRecord(1, time.Now()))

	got := c.Recent(key.Namespace, key.OwnerKind, key.OwnerName, key.Container, time.Minute)
	if got == nil {
		t.Fatalf("expected record")
	}
	got.RestartCount = 999

	got2 := c.Recent(key.Namespace, key.OwnerKind, key.OwnerName, key.Container, time.Minute)
	if got2.RestartCount == 999 {
		t.Fatalf("mutation through returned pointer leaked into cache")
	}
}

func TestRecentByWorkload(t *testing.T) {
	c := NewCache(time.Hour)
	now := time.Now()

	c.Record(Key{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "app", Container: "web"},
		OOMRecord{Container: "web", ObservedAt: now, RestartCount: 1, TerminatedAt: now})
	c.Record(Key{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "app", Container: "sidecar"},
		OOMRecord{Container: "sidecar", ObservedAt: now, RestartCount: 1, TerminatedAt: now})
	// Stale for the maxAge filter we'll use.
	c.Record(Key{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "app", Container: "old"},
		OOMRecord{Container: "old", ObservedAt: now.Add(-30 * time.Minute), RestartCount: 1, TerminatedAt: now})
	// Different workload entirely.
	c.Record(Key{Namespace: "ns", OwnerKind: "Deployment", OwnerName: "other", Container: "web"},
		OOMRecord{Container: "web", ObservedAt: now, RestartCount: 1, TerminatedAt: now})

	got := c.RecentByWorkload("ns", "Deployment", "app", 5*time.Minute)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2; got=%+v", len(got), got)
	}
	if _, ok := got["web"]; !ok {
		t.Errorf("missing web container")
	}
	if _, ok := got["sidecar"]; !ok {
		t.Errorf("missing sidecar container")
	}
	if _, ok := got["old"]; ok {
		t.Errorf("stale entry should be filtered out")
	}

	// Empty result is non-nil per contract.
	empty := c.RecentByWorkload("ns", "Deployment", "nonexistent", time.Minute)
	if empty == nil {
		t.Fatalf("RecentByWorkload returned nil for no matches; want empty map")
	}
	if len(empty) != 0 {
		t.Fatalf("len(empty)=%d, want 0", len(empty))
	}
}

func TestSweepEviction(t *testing.T) {
	c := NewCache(100 * time.Millisecond)
	now := time.Now()

	c.Record(makeKey("fresh"), OOMRecord{ObservedAt: now, RestartCount: 1, TerminatedAt: now})
	c.Record(makeKey("stale"), OOMRecord{ObservedAt: now.Add(-time.Hour), RestartCount: 1, TerminatedAt: now})
	if c.Size() != 2 {
		t.Fatalf("size=%d, want 2", c.Size())
	}

	c.sweep(now)

	if c.Size() != 1 {
		t.Fatalf("after sweep size=%d, want 1", c.Size())
	}
	if got := c.Recent("default", "Deployment", "app", "fresh", time.Minute); got == nil {
		t.Fatalf("fresh entry should survive sweep")
	}
	if got := c.Recent("default", "Deployment", "app", "stale", time.Minute); got != nil {
		t.Fatalf("stale entry should be evicted")
	}
}

func TestRunCancelsOnContext(t *testing.T) {
	c := NewCache(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after context cancel")
	}
}

func TestRunSecondCallIsNoOp(t *testing.T) {
	c := NewCache(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstDone := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(firstDone)
	}()
	time.Sleep(50 * time.Millisecond)

	secondDone := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(secondDone)
	}()

	cancel()
	<-firstDone
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatalf("second Run did not return after context cancel")
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := NewCache(time.Minute)
	const workers = 32
	const perWorker = 200

	var wg sync.WaitGroup
	wg.Add(workers * 2)

	for w := 0; w < workers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := Key{
					Namespace: "ns",
					OwnerKind: "Deployment",
					OwnerName: fmt.Sprintf("app-%d", w%4),
					Container: fmt.Sprintf("c-%d", i%8),
				}
				c.Record(key, OOMRecord{
					Container:    key.Container,
					ObservedAt:   time.Now(),
					TerminatedAt: time.Now(),
					RestartCount: int32(i),
				})
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_ = c.Recent("ns", "Deployment", fmt.Sprintf("app-%d", i%4), fmt.Sprintf("c-%d", i%8), time.Minute)
				_ = c.RecentByWorkload("ns", "Deployment", fmt.Sprintf("app-%d", i%4), time.Minute)
				_ = c.Size()
			}
		}()
	}

	wg.Wait()

	// Across 4 workloads * 8 containers we expect at most 32 distinct keys.
	if got := c.Size(); got == 0 || got > 32 {
		t.Fatalf("unexpected size after concurrent run: %d", got)
	}
}

func TestSize(t *testing.T) {
	c := NewCache(time.Minute)
	if c.Size() != 0 {
		t.Fatalf("empty Size=%d, want 0", c.Size())
	}
	c.Record(makeKey("a"), makeRecord(1, time.Now()))
	c.Record(makeKey("b"), makeRecord(1, time.Now()))
	if c.Size() != 2 {
		t.Fatalf("Size=%d, want 2", c.Size())
	}
	// Duplicate write must not grow size.
	c.Record(makeKey("a"), makeRecord(1, time.Now()))
	if c.Size() != 2 {
		t.Fatalf("Size after dedup=%d, want 2", c.Size())
	}
}
