package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
)

func newReadThrough[V any](fetch func(context.Context, string) (V, error)) *ReadThrough[string, V] {
	return NewReadThrough(NewLRU[string, V](16, 30*time.Second, nil), 2*time.Second, fetch, nil)
}

// barrier returns a join hook that closes the returned channel once n callers
// have joined, so a fetch held on it runs only with every caller waiting.
func barrier(n int32) (func(string), <-chan struct{}) {
	var joined atomic.Int32
	all := make(chan struct{})
	return func(string) {
		if joined.Add(1) == n {
			close(all)
		}
	}, all
}

func getConcurrently[V any](r *ReadThrough[string, V], n int, key string) ([]V, []error) {
	vals := make([]V, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { vals[i], errs[i] = r.Get(context.Background(), key) })
	}
	wg.Wait()
	return vals, errs
}

// The cache alone bounds only the steady state, since an entry lands after a
// completed fetch: a cold burst of N misses on one key must cost one fetch.
func TestReadThroughConcurrentMissesCollapseToOneFetch(t *testing.T) {
	const n = 50
	join, allJoined := barrier(n)
	var fetches atomic.Int32
	r := newReadThrough(func(context.Context, string) (string, error) {
		fetches.Add(1)
		<-allJoined
		return "v", nil
	})
	r.joined = join

	vals, errs := getConcurrently(r, n, "k")

	if got := fetches.Load(); got != 1 {
		t.Errorf("expected exactly 1 fetch for %d concurrent misses on one key, got %d", n, got)
	}
	for i := range n {
		if errs[i] != nil || vals[i] != "v" {
			t.Fatalf("call %d: got (%q, %v), want (v, nil)", i, vals[i], errs[i])
		}
	}
}

func TestReadThroughCachesAcrossCalls(t *testing.T) {
	var fetches int
	r := newReadThrough(func(context.Context, string) (string, error) {
		fetches++
		return "v", nil
	})
	for range 50 {
		if v, err := r.Get(context.Background(), "k"); err != nil || v != "v" {
			t.Fatalf("Get: got (%q, %v), want (v, nil)", v, err)
		}
	}
	if fetches != 1 {
		t.Errorf("expected 1 fetch for 50 calls on one key, got %d", fetches)
	}
}

// A fetch caches a negative result by returning it as a value, so the common
// unmanaged case is not re-read on every call.
func TestReadThroughCachesANegativeResultReturnedAsAValue(t *testing.T) {
	var fetches int
	r := newReadThrough(func(context.Context, string) (map[string]string, error) {
		fetches++
		return nil, nil
	})
	for range 10 {
		if v, err := r.Get(context.Background(), "gone"); err != nil || v != nil {
			t.Fatalf("Get: got (%v, %v), want (nil, nil)", v, err)
		}
	}
	if fetches != 1 {
		t.Errorf("expected 1 fetch for 10 calls returning a negative result, got %d", fetches)
	}
}

func TestReadThroughRefetchesPastTheTTL(t *testing.T) {
	clk := clocktesting.NewFakePassiveClock(epoch)
	var fetches int
	r := NewReadThrough(NewLRU[string, int](16, 30*time.Second, clk), time.Second,
		func(context.Context, string) (int, error) {
			fetches++
			return fetches, nil
		}, nil)

	if _, err := r.Get(context.Background(), "k"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	clk.SetTime(epoch.Add(29 * time.Second))
	if v, _ := r.Get(context.Background(), "k"); v != 1 {
		t.Fatalf("within the TTL: got the value of fetch %d, want the cached first", v)
	}
	clk.SetTime(epoch.Add(31 * time.Second))
	if v, _ := r.Get(context.Background(), "k"); v != 2 {
		t.Errorf("past the TTL: got the value of fetch %d, want a second fetch", v)
	}
}

func TestReadThroughNeverCachesAnError(t *testing.T) {
	boom := errors.New("apiserver unreachable")
	var fetches int
	r := newReadThrough(func(context.Context, string) (string, error) {
		fetches++
		if fetches == 1 {
			return "partial", boom
		}
		return "v", nil
	})

	v, err := r.Get(context.Background(), "k")
	if !errors.Is(err, boom) || v != "" {
		t.Fatalf("first Get: got (%q, %v), want the zero value and the fetch's error", v, err)
	}
	if v, err := r.Get(context.Background(), "k"); err != nil || v != "v" {
		t.Fatalf("second Get: got (%q, %v), want a fresh fetch's (v, nil)", v, err)
	}
	if fetches != 2 {
		t.Errorf("expected the failed fetch to be retried, got %d fetches", fetches)
	}
}

// A fetch runs off every caller's goroutine, where an unrecovered panic aborts
// the process. It must reach every waiter as an error, be reported once, and
// leave nothing cached.
func TestReadThroughFetchPanicFailsEveryWaiter(t *testing.T) {
	const n = 2
	join, allJoined := barrier(n)
	type report struct {
		key string
		rec any
	}
	var reports []report
	var fetches atomic.Int32
	r := NewReadThrough(NewLRU[string, string](16, 30*time.Second, nil), time.Second,
		func(context.Context, string) (string, error) {
			if fetches.Add(1) == 1 {
				<-allJoined
				panic("fetch exploded")
			}
			return "v", nil
		},
		func(_ context.Context, key string, rec any) { reports = append(reports, report{key, rec}) })
	r.joined = join

	vals, errs := getConcurrently(r, n, "k")

	for i := range n {
		var pe *PanicError
		if !errors.As(errs[i], &pe) || pe.Value != "fetch exploded" {
			t.Errorf("call %d: error = %v, want a *PanicError carrying the recovered value", i, errs[i])
		}
		if vals[i] != "" {
			t.Errorf("call %d: value = %q, want the zero value", i, vals[i])
		}
	}
	if len(reports) != 1 || reports[0] != (report{"k", "fetch exploded"}) {
		t.Errorf("onPanic reports = %v, want exactly one for key k", reports)
	}
	if v, err := r.Get(context.Background(), "k"); err != nil || v != "v" {
		t.Errorf("Get after the panic: got (%q, %v), want a fresh fetch's (v, nil)", v, err)
	}
}

// startHeldFetch starts the first caller of key on a fetch held until the
// returned release is called, and returns once the fetch is running. Cleanup
// releases it and waits for the flight to retire, so no goroutine outlives the
// test.
func startHeldFetch(t *testing.T, r *ReadThrough[string, string], started <-chan struct{}, release func()) {
	t.Helper()
	leaderDone := make(chan struct{})
	t.Cleanup(func() {
		release()
		select {
		case <-leaderDone:
		case <-time.After(2 * time.Second):
			t.Error("first caller still waiting after the test finished")
		}
		_, _ = r.Get(context.Background(), "k")
	})
	go func() {
		defer close(leaderDone)
		_, _ = r.Get(context.Background(), "k")
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("fetch never started")
	}
}

func heldFetch() (fetch func(context.Context, string) (string, error), started <-chan struct{}, release func(), fetches *atomic.Int32) {
	startedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	fetches = &atomic.Int32{}
	fetch = func(ctx context.Context, _ string) (string, error) {
		fetches.Add(1)
		startOnce.Do(func() { close(startedCh) })
		select {
		case <-releaseCh:
			return "v", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return fetch, startedCh, func() { releaseOnce.Do(func() { close(releaseCh) }) }, fetches
}

// A waiter must not block past its own deadline on a slow fetch: a caller's
// budget is its own, not the first caller's.
func TestReadThroughWaiterReturnsOnItsOwnDeadline(t *testing.T) {
	fetch, started, release, _ := heldFetch()
	r := newReadThrough(fetch)
	startHeldFetch(t, r, started, release)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	begin := time.Now()
	_, err := r.Get(ctx, "k")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the waiter's own context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("waiter blocked for %v past its own 20ms deadline", elapsed)
	}
}

// Every caller giving up does not abort the fetch: it runs on, and its value
// serves the next caller without another fetch.
func TestReadThroughFetchOutlivesCallersThatGaveUpAndFillsTheCache(t *testing.T) {
	fetch, started, release, fetches := heldFetch()
	r := newReadThrough(fetch)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := r.Get(ctx, "k")
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the caller's own context.Canceled", err)
	}

	release()
	for range 2 {
		if v, err := r.Get(context.Background(), "k"); err != nil || v != "v" {
			t.Fatalf("Get after the fetch was released: got (%q, %v), want (v, nil)", v, err)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("expected the abandoned fetch to serve the next caller, got %d fetches", got)
	}
}

// The fetch is bounded by its own deadline, not by any caller's, so a wedged
// read cannot pin it even when every caller waits forever.
func TestReadThroughFetchIsBoundedByItsOwnDeadline(t *testing.T) {
	r := NewReadThrough(NewLRU[string, string](16, 30*time.Second, nil), 20*time.Millisecond,
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, nil)

	begin := time.Now()
	_, err := r.Get(context.Background(), "k")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the fetch's own context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("fetch ran for %v past its 20ms deadline", elapsed)
	}
}
