// Package k8s holds small helpers shared by every binary that needs a
// controller-runtime client without taking on the full manager machinery.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"time"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// New builds a controller-runtime client against the in-cluster (or kubeconfig)
// REST config, using the supplied scheme. Used by the webhook and the
// dashboard binaries, which both run outside the controller-manager and
// therefore can't reuse manager.GetClient().
//
// Every Get/List issued through this client is a direct, uncached round trip
// to the apiserver. That is the right choice for the dashboard (low request
// volume, read-your-writes freshness matters more than load) but is exactly
// what NewCached below exists to avoid on the webhook's admission hot path.
func New(scheme *runtime.Scheme) (client.Client, error) {
	restCfg := ctrl.GetConfigOrDie()
	return client.New(restCfg, client.Options{Scheme: scheme})
}

// NewCached builds a controller-runtime client backed by an informer cache:
// Get/List calls are served from a local, watch-populated store instead of
// issuing a direct apiserver round trip every time. This is the client the
// webhook should use once Prometheus is out of the admission path and the
// apiserver becomes the only remaining source of per-pod latency -- at
// thousands of pods, an uncached Get for the Policy, the owner chain, and the
// WorkloadRecommendation on every single Pod CREATE is a per-pod apiserver
// round trip the cluster does not need.
//
// TWO contexts, because the startup phase and the serving phase want opposite
// lifetimes and conflating them loses one of the two.
//
//   - runCtx is the cache's run context: the informers' list-watch loops live
//     for as long as it is not Done. The caller keeps it alive past its own
//     shutdown signal on purpose, so in-flight admissions still read a cache
//     that is being updated while the server drains (see cmd/webhook/serve.go).
//     runCtx is the CALLER's to own and to cancel, and that is the whole
//     release mechanism on the success path: the returned client keeps a
//     goroutine and one informer per cached kind alive until runCtx is Done,
//     so a caller that never cancels it — context.Background(), say — holds
//     them, and their memory, for the life of the process. Failure returns
//     need no such unwinding: NewCached stops whatever it started before
//     returning an error, so a caller that retries construction or gives up on
//     it has nothing left to clean up and no need to cancel a context it may
//     still want for the next attempt.
//   - startCtx bounds the BLOCKING part of this call — the CRD wait and the
//     cache sync below — and is where the caller passes its shutdown signal.
//     Nothing answers /healthz while this call blocks, and the wait alone runs
//     to crdWaitTimeout, so a startCtx that ignores SIGTERM makes the process
//     ignore SIGTERM for that whole window: no probe to fail, no listener to
//     close, and removal only by the SIGKILL at the end of
//     terminationGracePeriodSeconds. Cancelling startCtx after this returns is
//     harmless — nothing here outlives the call.
//
// Passing the same context for both is valid and means "shutdown stops the
// cache immediately too"; the webhook deliberately does not want that.
//
// Three kinds are cached: Policy, WorkloadRecommendation and Namespace. All
// three are read on every admission (Namespace only on the multi-level
// opt-in path, see internal/webhook/optin.go) and all three are small even
// at cluster scale. The owner-chain kinds (ReplicaSet, Job, Deployment,
// StatefulSet, DaemonSet, CronJob and Rollout — see OwnerChainDisableFor) are
// deliberately left UNcached via DisableFor -- each is read at most once per
// pod CREATE, and an informer over every object of one of these kinds in the
// cluster (default revisionHistoryLimit alone keeps ~10 ReplicaSets per
// Deployment) would cost far more memory than the per-pod Get it saves.
//
// NewCached pre-warms those three informers and blocks until they have synced.
// Both halves matter, and the pre-warm is the part that is easy to get wrong:
// cache.New creates NO informers up front, so calling WaitForCacheSync on a
// fresh cache waits on an empty set and returns true immediately -- a no-op
// that looks like a guarantee. The informer for a kind is instead created
// lazily on its first Get, which blocks on a full cluster-wide LIST inline.
// On the admission path that LIST is charged against apiCallTimeout (2s), so
// on a large cluster the first admissions after every webhook restart would
// time out and fail open, admitting pods with template resources. That is a
// silent, self-healing bug -- it affects only the seconds after a restart,
// produces no error a human would notice, and disappears on its own -- which
// is exactly the kind that is very hard to diagnose after the fact.
// Registering the informers here moves that LIST into startup, before the
// webhook is wired into the apiserver and receiving traffic.
//
// Memory cost: for the WorkloadRecommendation informer expect roughly
// 20-50 MB resident at ~10k workloads (one WLR per workload, each holding a
// handful of quantity-valued fields per container); the Policy informer is
// negligible. This is a real number to account for in the webhook
// Deployment's memory requests/limits, not a rounding error -- it trades
// apiserver query load for webhook pod memory.
func NewCached(runCtx, startCtx context.Context, scheme *runtime.Scheme) (_ client.Client, err error) {
	restCfg := ctrl.GetConfigOrDie()

	c, err := cache.New(restCfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building informer cache: %w", err)
	}

	// Register the informers BEFORE starting the cache. cache.New creates none
	// on its own, so without this the WaitForCacheSync below would wait on an
	// empty set and return true instantly -- see the doc comment above.
	//
	// Doing it before Start rather than after is what keeps this loop's error
	// returns free of cleanup: GetInformer on a cache that has not been started
	// only creates the informer, it neither runs nor blocks on it (see
	// Informers.Get, which skips the wait-for-sync when started is false), so
	// the CRD-establishment failure below -- much the likeliest way this
	// function fails -- happens with no run goroutine in existence. The sync
	// itself is not skipped, only moved: Start below runs all three informers
	// at once and WaitForCacheSync waits for the set, instead of each
	// GetInformer blocking on its own kind in turn.
	//
	// ONE deadline for the whole loop, not one per kind. Every cached kind
	// comes from the same chart and is established within the same second or
	// two, so a per-kind budget would only ever multiply the worst case by the
	// number of kinds -- and the container's startup budget in the chart has to
	// be sized against whatever this can block for. A single number is the only
	// one that can be stated, and honoured, on both sides.
	crdDeadline := time.Now().Add(crdWaitTimeout)
	for _, obj := range cachedKinds() {
		if err := registerInformerWithRetry(startCtx, c, obj, crdDeadline, crdWaitInterval); err != nil {
			return nil, fmt.Errorf("waiting up to %s for custom resource definitions to be served: %w",
				crdWaitTimeout, err)
		}
	}

	// Past this point the cache is running, so every error return has to stop
	// it. It cannot be left to the caller: runCtx is the caller's and outlives
	// this call by design (see the doc comment), so on a failed construction
	// there is no client to justify a live cache, and a caller that passed
	// context.Background() has no cancel to call at all -- the goroutine and
	// its informers would simply stay. cacheCtx is the child this function
	// owns; the guard cancels it on failure only, leaving the success path's
	// cache bound to runCtx exactly as before.
	cacheCtx, stopCache := context.WithCancel(runCtx)
	defer func() {
		if err != nil {
			stopCache()
		}
	}()

	// Cache.Start blocks for as long as cacheCtx is alive (it's the informers'
	// run loop), so it has to run in its own goroutine. Its return value is
	// always nil in practice (Start only returns non-nil if called twice on
	// the same cache, which cannot happen here since we own the only
	// reference); nothing actionable to do with it once the context ends and
	// the process is already shutting down.
	go func() {
		_ = c.Start(cacheCtx)
	}()

	// Now meaningful: blocks until Start has actually begun (avoiding the race
	// where a caller reads before the cache flips into "started") and until
	// every informer registered above reports HasSynced.
	if !c.WaitForCacheSync(startCtx) {
		// WaitForCacheSync also returns false when the cache never started or an
		// informer's HasSynced never flipped — not only on context cancellation.
		// In those cases startCtx.Err() is nil and %w would render the
		// operator-facing startup failure as "...: %!w(<nil>)".
		if err := startCtx.Err(); err != nil {
			return nil, fmt.Errorf("informer cache failed to sync: %w", err)
		}
		return nil, errors.New("informer cache failed to sync")
	}

	// Assigned rather than returned directly so the guard above sees this
	// error too: the cache is already running by now, and a client that could
	// not be built is one more caller-less cache to stop.
	cached, err := client.New(restCfg, client.Options{
		Scheme: scheme,
		Cache: &client.CacheOptions{
			Reader: c,
			// Read straight from the apiserver instead of standing up an
			// informer over every object of these kinds. See
			// OwnerChainDisableFor's doc comment for why each one is here.
			DisableFor: OwnerChainDisableFor(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("building cache-backed client: %w", err)
	}
	return cached, nil
}

// OwnerChainDisableFor lists the kinds NewCached serves straight from the
// apiserver instead of standing up an informer for, because every Get of one
// of these kinds happens at most once per pod CREATE:
//
//   - ReplicaSet and Job are read while walking a pod's controller ownerRef
//     chain to its top-level workload kind (internal/workload.ResolvePodOwner).
//   - Deployment, StatefulSet, DaemonSet, CronJob and Rollout are read one
//     level beyond that, by the webhook's multi-level opt-in resolution
//     (internal/webhook/optin.go, ownerAnnotations), to fetch the resolved
//     owner OBJECT's own metadata.annotations — the "workload" level of
//     ResolvePolicy that a pod does not inherit.
//
// An informer over any of these would watch every object of that kind
// cluster-wide for a read that happens once per pod CREATE — the same
// memory-for-a-single-Get trade the ReplicaSet/Job split already avoids (see
// the package doc above). Exported so
// TestDisableForCoversOwnerAnnotationKinds (internal/webhook/optin_test.go)
// can assert this list actually covers every kind ownerAnnotations can Get:
// the two evolve in different packages, so nothing else stops a kind added to
// one from being silently missing from the other, which would reintroduce
// exactly the startup-stall / unbounded-memory bug this function exists to
// prevent.
func OwnerChainDisableFor() []client.Object {
	return []client.Object{
		&appsv1.ReplicaSet{},
		&batchv1.Job{},
		&appsv1.Deployment{},
		&appsv1.StatefulSet{},
		&appsv1.DaemonSet{},
		&batchv1.CronJob{},
		&rolloutsv1alpha1.Rollout{},
	}
}

// crdWaitTimeout bounds how long NewCached waits for the Policy and
// WorkloadRecommendation CRDs to become servable before giving up.
//
// GetInformer needs the apiserver to already serve the kind. On a fresh
// install that is racy: the chart renders CRDs as ordinary templates, and
// while Helm's kind sorter applies them before the Deployment, ESTABLISHMENT
// (the apiserver serving the type, plus a discovery-cache refresh) lags
// creation. installCRDs=false is also supported, so a cluster managing CRDs
// separately can start the webhook first outright.
//
// Failing immediately turns that race into CrashLoopBackOff, whose backoff
// climbs to five minutes — far longer than the establishment it is waiting on.
// At the default failurePolicy=Ignore that is a startup papercut (pods start
// unmutated); at failurePolicy=Fail a crash-looping webhook BLOCKS pod
// creation across its scope, and a fresh install is exactly when that is most
// likely. Waiting in-process instead turns minutes of backoff into seconds.
//
// Bounded, not infinite: a CRD that is genuinely absent — never installed,
// wrong release, RBAC that cannot read it — must still surface as a failed
// pod rather than one that sits Ready-less forever with no explanation.
//
// # This number is half of a contract with the Helm chart
//
// NewCached runs BEFORE the webhook's HTTP listener starts, so nothing answers
// /healthz for as long as this wait lasts. The chart's webhook liveness and
// readiness probes alone would kill the container at
// initialDelaySeconds + periodSeconds × failureThreshold ≈ 40s — well inside
// this budget — so the pod would be restarted on exactly the fresh-install
// race this wait was written for, and still end in CrashLoopBackOff.
//
// charts/k8s-sustain/values.yaml therefore gives the webhook a startupProbe
// whose budget (initialDelaySeconds + periodSeconds × failureThreshold)
// EXCEEDS this constant; liveness and readiness do not begin until it succeeds.
// Raising this constant without raising that budget silently reintroduces the
// crash loop. TestCRDWaitFitsInsideChartStartupProbeBudget pins the
// relationship, and values.yaml carries the other half of this comment.
const crdWaitTimeout = 2 * time.Minute

// crdWaitInterval is the poll gap while waiting for establishment. Short
// relative to crdWaitTimeout: establishment normally completes in seconds, and
// the cost of a retry is one discovery lookup.
const crdWaitInterval = 2 * time.Second

// registerInformerWithRetry registers obj's informer, tolerating a CRD that is
// not servable yet — see crdWaitTimeout.
//
// deadline is an absolute instant rather than a per-call duration so that every
// cached kind shares one startup budget; see the loop in NewCached.
//
// Only "no matches for kind" (meta.NoKindMatchError) is retried. Every other
// failure — RBAC denial, an unreachable apiserver, a scheme gap — is returned
// at once: none of those resolve by waiting, and retrying them would convert a
// clear startup error into a two-minute silence.
func registerInformerWithRetry(
	ctx context.Context, c cache.Cache, obj client.Object, deadline time.Time, interval time.Duration,
) error {
	logger := log.FromContext(ctx)
	for attempt := 1; ; attempt++ {
		_, err := c.GetInformer(ctx, obj)
		if err == nil {
			return nil
		}
		if !meta.IsNoMatchError(err) {
			return fmt.Errorf("registering informer for %T: %w", obj, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registering informer for %T: custom resource definition still not "+
				"served (is the CRD installed?): %w", obj, err)
		}
		logger.Info("custom resource definition not served yet; waiting for it to be established",
			"type", fmt.Sprintf("%T", obj), "attempt", attempt, "retryIn", interval)
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return fmt.Errorf("registering informer for %T: %w", obj, ctx.Err())
		}
	}
}

// cachedKinds lists the types NewCached keeps informers for. Kept as a
// function so the pre-warm loop and any future assertion cannot drift from
// each other.
func cachedKinds() []client.Object {
	return []client.Object{
		&sustainv1alpha1.Policy{},
		&sustainv1alpha1.WorkloadRecommendation{},
		// Namespaces are read on the admission hot path to resolve
		// namespace-level policy opt-in. There are few of them even in large
		// clusters, so the informer's memory cost is trivial next to keeping
		// that read off the apiserver for every pod create.
		&corev1.Namespace{},
	}
}
