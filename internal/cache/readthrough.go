package cache

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// ReadThrough serves reads from an LRU and fills a miss by fetching, at most
// one fetch per key at a time: a caller that misses while a fetch of its key
// runs waits for that fetch instead of starting its own.
//
// The fetch runs on a context detached from every caller and bounded by its
// own deadline. A caller that gives up returns its ctx.Err() while the fetch
// goes on for the others, and still fills the LRU for the next caller. A fetch
// error reaches every caller waiting on it and is never cached. A value
// fetched without error is cached, a nil or zero one included: a fetch caches
// a negative result, such as NotFound, by returning it as a value.
type ReadThrough[K comparable, V any] struct {
	entries  *LRU[K, V]
	deadline time.Duration
	fetch    func(context.Context, K) (V, error)
	onPanic  func(context.Context, K, any)

	mu      sync.Mutex
	flights map[K]*flight[V]

	// joined, if set, is called with the key each time a caller joins a fetch.
	// Test-only: a barrier that holds a fetch open until every caller waits.
	joined func(K)
}

type flight[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// PanicError is what every caller waiting on a fetch that panicked gets
// instead of a value.
type PanicError struct {
	Key   any
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("panic reading %v: %v", e.Key, e.Value)
}

// NewReadThrough returns a ReadThrough over entries whose fetch runs for at
// most deadline. onPanic, when set, is called with a recovered fetch panic,
// on the fetch's goroutine and before its callers get a *PanicError.
func NewReadThrough[K comparable, V any](
	entries *LRU[K, V],
	deadline time.Duration,
	fetch func(context.Context, K) (V, error),
	onPanic func(context.Context, K, any),
) *ReadThrough[K, V] {
	return &ReadThrough[K, V]{
		entries:  entries,
		deadline: deadline,
		fetch:    fetch,
		onPanic:  onPanic,
		flights:  map[K]*flight[V]{},
	}
}

// Get returns the fresh value cached at key, or else the result of the fetch
// running for key, starting one when none is.
func (r *ReadThrough[K, V]) Get(ctx context.Context, key K) (V, error) {
	if v, ok := r.entries.Get(key); ok {
		return v, nil
	}
	f := r.join(ctx, key)
	if r.joined != nil {
		r.joined(key)
	}
	select {
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	case <-f.done:
		return f.val, f.err
	}
}

func (r *ReadThrough[K, V]) join(ctx context.Context, key K) *flight[V] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.flights[key]; ok {
		return f
	}
	f := &flight[V]{done: make(chan struct{})}
	r.flights[key] = f
	go r.lead(context.WithoutCancel(ctx), key, f)
	return f
}

// lead caches before it retires the flight, so a caller arriving in between
// either hits the cache or joins the flight, and never fetches again.
func (r *ReadThrough[K, V]) lead(ctx context.Context, key K, f *flight[V]) {
	val, err := r.fetchRecovered(ctx, key)
	if err == nil {
		r.entries.Set(key, val)
		f.val = val
	}
	f.err = err
	r.mu.Lock()
	delete(r.flights, key)
	r.mu.Unlock()
	close(f.done)
}

// fetchRecovered contains a fetch panic: the fetch runs on a goroutine of its
// own, where nothing above it recovers, so a panic would abort the process.
func (r *ReadThrough[K, V]) fetchRecovered(ctx context.Context, key K) (val V, err error) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		if r.onPanic != nil {
			r.onPanic(ctx, key, rec)
		}
		var zero V
		val, err = zero, &PanicError{Key: key, Value: rec, Stack: debug.Stack()}
	}()
	fetchCtx, cancel := context.WithTimeout(ctx, r.deadline)
	defer cancel()
	return r.fetch(fetchCtx, key)
}
