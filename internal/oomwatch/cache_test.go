package oomwatch

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
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
	fresh := c.RecentByWorkload("default", "Deployment", "app", time.Hour)
	if _, ok := fresh["a"]; ok {
		t.Errorf("oldest entry still present after eviction")
	}
	if _, ok := fresh["b"]; !ok {
		t.Errorf("middle entry missing after eviction")
	}
	if _, ok := fresh["c"]; !ok {
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

func TestRecentByWorkloadStaleByMaxAge(t *testing.T) {
	c := NewCache(time.Hour)
	key := makeKey("web")
	rec := makeRecord(1, time.Now())
	rec.ObservedAt = time.Now().Add(-5 * time.Minute)
	c.Record(key, rec)

	// maxAge < age -> filtered out, but ttl > age -> still present in map.
	if got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Minute); len(got) != 0 {
		t.Fatalf("RecentByWorkload returned stale entry: %+v", got)
	}
	if c.Size() != 1 {
		t.Fatalf("maxAge filter must not evict; size=%d", c.Size())
	}

	// maxAge > age -> returned.
	if got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, 10*time.Minute); len(got) == 0 {
		t.Fatalf("RecentByWorkload did not return fresh entry")
	}
}

func TestRecentByWorkloadReturnsCopy(t *testing.T) {
	c := NewCache(time.Minute)
	key := makeKey("web")
	c.Record(key, makeRecord(1, time.Now()))

	got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Minute)
	if got[key.Container] == nil {
		t.Fatalf("expected record")
	}
	got[key.Container].RestartCount = 999

	got2 := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Minute)
	if got2[key.Container].RestartCount == 999 {
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
	survivors := c.RecentByWorkload("default", "Deployment", "app", time.Minute)
	if _, ok := survivors["fresh"]; !ok {
		t.Fatalf("fresh entry should survive sweep")
	}
	if _, ok := survivors["stale"]; ok {
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

	// Second call must return immediately even with an independent,
	// never-cancelled context. Blocking here would leak a goroutine in any
	// caller that wires Run twice.
	secondDone := make(chan struct{})
	go func() {
		c.Run(t.Context())
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatalf("second Run did not return immediately when first is still running")
	}

	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatalf("first Run did not return after context cancel")
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := NewCache(time.Minute)
	const workers = 32
	const perWorker = 200

	var wg sync.WaitGroup

	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
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
		})
		wg.Go(func() {
			for i := range perWorker {
				_ = c.RecentByWorkload("ns", "Deployment", fmt.Sprintf("app-%d", i%4), time.Minute)
				_ = c.Size()
			}
		})
	}

	wg.Wait()

	// Across 4 workloads * 8 containers we expect at most 32 distinct keys.
	if got := c.Size(); got == 0 || got > 32 {
		t.Fatalf("unexpected size after concurrent run: %d", got)
	}
}

// TestResolvedMarkTTLDeterministic drives AlreadyResolved/MarkResolved's TTL
// window via the injectable-now variants (alreadyResolvedAt/markResolvedAt)
// instead of racing a real clock: a mark inside the ttl window must still
// suppress, and the same mark queried past the window must not.
func TestResolvedMarkTTLDeterministic(t *testing.T) {
	c := NewCache(time.Minute)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	podUID := types.UID("pod-1")

	// Nothing marked yet: never suppresses.
	if c.alreadyResolvedAt(base, podUID, "web", 1, base) {
		t.Fatalf("AlreadyResolved reported true before any MarkResolved call")
	}

	c.markResolvedAt(base, podUID, "web", 1, base)

	// Just inside the window: still suppresses.
	if !c.alreadyResolvedAt(base.Add(59*time.Second), podUID, "web", 1, base) {
		t.Fatalf("AlreadyResolved reported false inside the ttl window")
	}

	// Past the window: must no longer suppress.
	if c.alreadyResolvedAt(base.Add(61*time.Second), podUID, "web", 1, base) {
		t.Fatalf("AlreadyResolved reported true past the ttl window")
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

// TestRecordLatestWins pins the ordering guarantee Record makes for a single
// Key's identity and timestamps: those always come from the newest observation
// by (TerminatedAt, RestartCount), no matter which order the writes arrive in.
// Different pods of the same workload are genuinely parallel reconcile keys
// (maxConcurrentReconciles > 1), so without this the stored identity would
// degrade to whichever goroutine took the lock last. OOMLimitBytes follows a
// different rule (max, not recency) — see TestRecordMergesLargestOOMLimit; the
// cases here all happen to have the newest record carrying the larger limit,
// so both rules agree on the expectation.
func TestRecordLatestWins(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	rec := func(restart int32, terminated time.Time, limit int64) OOMRecord {
		r := makeRecord(restart, terminated)
		r.OOMLimitBytes = limit
		return r
	}

	const (
		small = 128 * 1024 * 1024
		big   = 512 * 1024 * 1024
	)

	cases := []struct {
		name  string
		first OOMRecord
		write OOMRecord
		want  OOMRecord
	}{
		{
			name:  "newer then older keeps the newer entry",
			first: rec(5, t1, big),
			write: rec(2, t0, small),
			want:  rec(5, t1, big),
		},
		{
			name:  "older then newer takes the newer entry",
			first: rec(2, t0, small),
			write: rec(5, t1, big),
			want:  rec(5, t1, big),
		},
		{
			name:  "same TerminatedAt higher RestartCount wins",
			first: rec(5, t1, small),
			write: rec(6, t1, big),
			want:  rec(6, t1, big),
		},
		{
			name:  "same TerminatedAt lower RestartCount loses",
			first: rec(6, t1, big),
			write: rec(5, t1, small),
			want:  rec(6, t1, big),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(time.Hour)
			key := makeKey("web")
			c.Record(key, tc.first)
			c.Record(key, tc.write)

			got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Hour)[key.Container]
			if got == nil {
				t.Fatalf("RecentByWorkload lost the entry entirely")
			}
			if got.RestartCount != tc.want.RestartCount {
				t.Errorf("RestartCount = %d, want %d", got.RestartCount, tc.want.RestartCount)
			}
			if !got.TerminatedAt.Equal(tc.want.TerminatedAt) {
				t.Errorf("TerminatedAt = %v, want %v", got.TerminatedAt, tc.want.TerminatedAt)
			}
			if got.OOMLimitBytes != tc.want.OOMLimitBytes {
				t.Errorf("OOMLimitBytes = %d, want %d", got.OOMLimitBytes, tc.want.OOMLimitBytes)
			}
			if c.Size() != 1 {
				t.Errorf("Size() = %d, want 1", c.Size())
			}
		})
	}
}

// TestRecordReturnValueOnOlderRecord pins the bool contract for the branch
// TestRecordLatestWins added: an out-of-order observation is still a distinct
// kill this cache had never seen, so it reports true (and fans out to the
// handler) even though it does not displace the stored entry. Only an exact
// (RestartCount, TerminatedAt) repeat reports false.
func TestRecordReturnValueOnOlderRecord(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	c := NewCache(time.Hour)
	key := makeKey("web")

	if got := c.Record(key, makeRecord(5, t1)); !got {
		t.Fatalf("first Record() = false, want true")
	}
	// Distinct but older: new to the cache, so it still triggers.
	if got := c.Record(key, makeRecord(2, t0)); !got {
		t.Errorf("Record(older) = false, want true (distinct kill, still reported)")
	}
	// Repeat of the winning entry: pure duplicate, must not re-trigger.
	if got := c.Record(key, makeRecord(5, t1)); got {
		t.Errorf("Record(duplicate of stored) = true, want false")
	}
}

// TestRecordConcurrentLatestWins is the race-detector variant of
// TestRecordLatestWins: whichever goroutine writes last, the newest
// observation must be the one left in the cache.
func TestRecordConcurrentLatestWins(t *testing.T) {
	c := NewCache(time.Hour)
	key := makeKey("web")
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			r := makeRecord(int32(i), base.Add(time.Duration(i)*time.Minute))
			r.OOMLimitBytes = int64(i+1) * 1024 * 1024
			c.Record(key, r)
		})
	}
	wg.Wait()

	got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Hour)[key.Container]
	if got == nil {
		t.Fatalf("RecentByWorkload lost the entry entirely")
	}
	if want := int32(writers - 1); got.RestartCount != want {
		t.Errorf("RestartCount = %d, want %d (newest write must win)", got.RestartCount, want)
	}
}

// TestRecordMergesLargestOOMLimit pins the per-field merge: recency governs
// identity/timestamps, but OOMLimitBytes is the max across every pod that
// wrote to the slot. The failing case needs no timestamp tie — a Deployment
// whose limit was bumped 128Mi -> 256Mi can have an old un-resized pod OOM at
// t1 (128Mi) after a resized pod OOM'd at t0 (256Mi). The newer record wins
// the identity, but anchoring the memory floor on 128Mi would bump into a
// limit 256Mi already proved insufficient.
func TestRecordMergesLargestOOMLimit(t *testing.T) {
	now := time.Now()
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	const (
		small = 128 * 1024 * 1024
		big   = 256 * 1024 * 1024
	)

	// podA: newer kill on the stale 128Mi spec. podB: older kill on the
	// already-resized 256Mi spec.
	podA := OOMRecord{
		Container: "web", PolicyName: "policy-a", ObservedAt: now,
		TerminatedAt: t1, RestartCount: 3,
		PodName: "pod-a", PodUID: "uid-a", OOMLimitBytes: small,
	}
	podB := OOMRecord{
		Container: "web", PolicyName: "policy-a", ObservedAt: now,
		TerminatedAt: t0, RestartCount: 9,
		PodName: "pod-b", PodUID: "uid-b", OOMLimitBytes: big,
	}

	cases := []struct {
		name  string
		order []OOMRecord
	}{
		{"older-larger first", []OOMRecord{podB, podA}},
		{"newer-smaller first", []OOMRecord{podA, podB}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(time.Hour)
			key := makeKey("web")
			for _, r := range tc.order {
				c.Record(key, r)
			}

			got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Hour)[key.Container]
			if got == nil {
				t.Fatalf("RecentByWorkload lost the entry entirely")
			}
			// Identity and timestamps come from the newest observation.
			if !got.TerminatedAt.Equal(podA.TerminatedAt) {
				t.Errorf("TerminatedAt = %v, want %v (newest observation)", got.TerminatedAt, podA.TerminatedAt)
			}
			if got.PodUID != podA.PodUID {
				t.Errorf("PodUID = %q, want %q (newest observation)", got.PodUID, podA.PodUID)
			}
			if got.RestartCount != podA.RestartCount {
				t.Errorf("RestartCount = %d, want %d (newest observation)", got.RestartCount, podA.RestartCount)
			}
			// The anchor is the largest limit that still got OOM-killed.
			if got.OOMLimitBytes != big {
				t.Errorf("OOMLimitBytes = %d, want %d (largest killed limit)", got.OOMLimitBytes, int64(big))
			}
		})
	}
}

// TestRecordMergesOOMLimitOnEqualTerminatedAt is the same merge on the
// equal-timestamp path: metav1.Time truncates to whole seconds, so two pods of
// one workload can OOM "at the same time" and the only tie-break left is a
// restart counter that means nothing across pods. Whichever identity wins, the
// anchor must still be the larger killed limit.
func TestRecordMergesOOMLimitOnEqualTerminatedAt(t *testing.T) {
	now := time.Now()
	at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	const (
		small = 128 * 1024 * 1024
		big   = 256 * 1024 * 1024
	)

	podA := OOMRecord{
		Container: "web", PolicyName: "policy-a", ObservedAt: now,
		TerminatedAt: at, RestartCount: 7,
		PodName: "pod-a", PodUID: "uid-a", OOMLimitBytes: small,
	}
	podB := OOMRecord{
		Container: "web", PolicyName: "policy-a", ObservedAt: now,
		TerminatedAt: at, RestartCount: 1,
		PodName: "pod-b", PodUID: "uid-b", OOMLimitBytes: big,
	}

	cases := []struct {
		name  string
		order []OOMRecord
	}{
		{"high restart count first", []OOMRecord{podA, podB}},
		{"low restart count first", []OOMRecord{podB, podA}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(time.Hour)
			key := makeKey("web")
			for _, r := range tc.order {
				c.Record(key, r)
			}

			got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Hour)[key.Container]
			if got == nil {
				t.Fatalf("RecentByWorkload lost the entry entirely")
			}
			if got.OOMLimitBytes != big {
				t.Errorf("OOMLimitBytes = %d, want %d (largest killed limit)", got.OOMLimitBytes, int64(big))
			}
		})
	}
}

// TestRecordReturnValueWithMergedLimit pins that merging the anchor did not
// move the bool contract: a distinct-but-older observation still reports true
// (it is a real kill nothing has seen, so it must still fan out), and an exact
// repeat of the stored identity still reports false — including when the stored
// entry carries a larger anchor merged in from a third pod, which the repeat
// must neither drop nor turn into a permanent "new kill".
func TestRecordReturnValueWithMergedLimit(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	const (
		small = 128 * 1024 * 1024
		big   = 256 * 1024 * 1024
	)

	rec := func(pod string, restart int32, terminated time.Time, limit int64) OOMRecord {
		return OOMRecord{
			Container: "web", PolicyName: "policy-a", ObservedAt: time.Now(),
			TerminatedAt: terminated, RestartCount: restart,
			PodName: pod, PodUID: pod, OOMLimitBytes: limit,
		}
	}

	c := NewCache(time.Hour)
	key := makeKey("web")

	if got := c.Record(key, rec("uid-a", 3, t1, small)); !got {
		t.Fatalf("first Record() = false, want true")
	}
	// Distinct but older, and carrying the bigger killed limit.
	if got := c.Record(key, rec("uid-b", 9, t0, big)); !got {
		t.Errorf("Record(older) = false, want true (distinct kill, still reported)")
	}
	// Exact repeat of the stored identity, while the slot holds another
	// pod's larger anchor: still a duplicate, and the anchor survives.
	for range 2 {
		if got := c.Record(key, rec("uid-a", 3, t1, small)); got {
			t.Errorf("Record(duplicate of stored) = true, want false")
		}
	}
	got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, time.Hour)[key.Container]
	if got == nil {
		t.Fatalf("RecentByWorkload lost the entry entirely")
	}
	if got.OOMLimitBytes != big {
		t.Errorf("OOMLimitBytes = %d, want %d (duplicate must not drop the merged anchor)", got.OOMLimitBytes, int64(big))
	}
	if got.PodUID != "uid-a" {
		t.Errorf("PodUID = %q, want %q (newest observation)", got.PodUID, "uid-a")
	}
}

// TestRecordOutOfOrderKillRefreshesObservedAt pins the freshness half of the
// per-field merge. An out-of-order observation still reports true — the
// watcher fans it out and triggers an immediate reconcile — so it must also
// keep the entry it contributed to alive: ObservedAt is what sweep and
// RecentByWorkload age entries off, not TerminatedAt. Taking ObservedAt from
// the identity winner let a kill observed seconds ago be swept on a stored
// entry's much older clock, losing the memory-floor evidence right after
// announcing it.
func TestRecordOutOfOrderKillRefreshesObservedAt(t *testing.T) {
	now := time.Now()
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	const (
		small = 128 * 1024 * 1024
		big   = 256 * 1024 * 1024
	)

	c := NewCache(time.Hour)
	key := makeKey("web")

	// Seen long ago: the newer kill, on the stale 128Mi spec.
	c.Record(key, OOMRecord{
		Container: "web", PolicyName: "policy-a", ObservedAt: now.Add(-50 * time.Minute),
		TerminatedAt: t1, RestartCount: 3,
		PodName: "pod-a", PodUID: "uid-a", OOMLimitBytes: small,
	})
	// Seen just now: an older kill on another pod, still running the 256Mi
	// spec — node clock skew, or a status update the watcher only got to now.
	if isNew := c.Record(key, OOMRecord{
		Container: "web", PolicyName: "policy-a", ObservedAt: now,
		TerminatedAt: t0, RestartCount: 9,
		PodName: "pod-b", PodUID: "uid-b", OOMLimitBytes: big,
	}); !isNew {
		t.Fatal("Record reported a distinct kill as a duplicate")
	}

	// A 5m window is far inside the 50m-old stored clock: the entry is only
	// visible here if the second observation advanced ObservedAt.
	got := c.RecentByWorkload(key.Namespace, key.OwnerKind, key.OwnerName, 5*time.Minute)[key.Container]
	if got == nil {
		t.Fatal("entry aged out of a 5m window although it was observed just now")
	}
	if got.ObservedAt.Before(now) {
		t.Errorf("ObservedAt = %v, want >= %v (the later of the two observations)", got.ObservedAt, now)
	}
	// The rest of the merge is unchanged: newest identity, largest limit.
	if got.PodUID != "uid-a" {
		t.Errorf("PodUID = %q, want %q (newest observation)", got.PodUID, "uid-a")
	}
	if got.OOMLimitBytes != big {
		t.Errorf("OOMLimitBytes = %d, want %d (largest killed limit)", got.OOMLimitBytes, int64(big))
	}
}
