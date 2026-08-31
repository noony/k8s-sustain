package webhook

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	k8sclient "github.com/noony/k8s-sustain/internal/k8s"
	"github.com/noony/k8s-sustain/internal/policymatch/policymatchtest"
	"github.com/noony/k8s-sustain/internal/workload"
)

// optInFixture builds a Deployment→ReplicaSet→Pod chain in an annotated
// namespace, with each level's annotations supplied by the caller.
func optInFixture(t *testing.T, tmplAnn, workloadAnn, nsAnn map[string]string, policies ...client.Object) (*Handler, *corev1.Pod) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Annotations: nsAnn}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "web", Annotations: workloadAnn,
	}}
	rs := deploymentReplicaSet("team-a", "web-abc", "web")
	pod := podWithRSOwner("team-a", "web-abc-1", "web-abc", "")
	pod.Annotations = tmplAnn

	// config.Scheme() is the full manager scheme (core, apps, batch, rollouts,
	// k8s.sustain.io). internal/webhook's stub_test.go already builds fake
	// clients this way; the ad-hoc scheme in newAdmitEnv does not register
	// corev1, which the Namespace read now needs.
	objs := append([]client.Object{ns, d, rs}, policies...)
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(objs...).Build()
	return &Handler{Client: c}, pod
}

// TestResolveOptIn_AnnotationLevels replays the shared contract table against
// the webhook's own wiring.
func TestResolveOptIn_AnnotationLevels(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	for _, tc := range policymatchtest.AnnotationCases() {
		t.Run(tc.Name, func(t *testing.T) {
			h, pod := optInFixture(t, tc.Template, tc.Workload, tc.Namespace, policy)
			gotName, gotLevel, owner, err := h.resolveOptIn(context.Background(), log.Log, pod)
			if err != nil {
				t.Fatalf("resolveOptIn: %v", err)
			}
			if gotName != tc.WantPolicy || gotLevel != tc.WantLevel {
				t.Errorf("resolveOptIn = (%q, %q), want (%q, %q)", gotName, gotLevel, tc.WantPolicy, tc.WantLevel)
			}
			if gotName != "" && (!owner.Resolved || owner.Kind != "Deployment" || owner.Name != "web") {
				t.Errorf("owner = %+v, want a resolved Deployment/web so admit() need not resolve it twice", owner)
			}
		})
	}
}

// The pre-gate is the whole cost story: a pod no Policy selector could ever
// claim must not trigger the owner Get.
func TestResolveOptIn_PreGateSkipsOwnerLookup(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	policy.Spec.Selector.Namespaces = []string{"some-other-namespace"}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}
	rs := deploymentReplicaSet("team-a", "web-abc", "web")
	pod := podWithRSOwner("team-a", "web-abc-1", "web-abc", "")

	var replicaSetGets int
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(ns, d, rs, policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok {
					replicaSetGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c}

	name, _, _, err := h.resolveOptIn(context.Background(), log.Log, pod)
	if err != nil {
		t.Fatalf("resolveOptIn: %v", err)
	}
	if name != "" {
		t.Errorf("expected no opt-in, got %q", name)
	}
	if replicaSetGets != 0 {
		t.Errorf("pre-gate must short-circuit before owner resolution, but issued %d ReplicaSet Gets", replicaSetGets)
	}
}

// An excluded namespace is a hard deny — the pre-gate must reject it even when
// a cluster-wide Policy would otherwise cover the pod.
func TestResolveOptIn_ExcludedNamespaceShortCircuits(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	h, pod := optInFixture(t,
		nil, nil,
		map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
		policy)
	h.ExcludedNamespaces = []string{"team-a"}

	name, _, _, err := h.resolveOptIn(context.Background(), log.Log, pod)
	if err != nil {
		t.Fatalf("resolveOptIn: %v", err)
	}
	if name != "" {
		t.Errorf("excluded namespace must never opt in, got %q", name)
	}
}

// TestResolveOptIn_CachesReplicaSetGetAcrossPodsBehindSameOwner pins the fix
// for a review finding: resolveOptIn's owner-chain walk
// (workload.ResolvePodOwner, now resolveCachedPodOwner) issued an uncached
// ReplicaSet Get, even though the second Get one level further up (the
// resolved Deployment, via h.ownerAnnCache) was already cached. With the
// Quick Start Policy's empty selector — which is what makes anyPolicyCovers
// return true unconditionally here — every pod CREATE in the cluster paid
// that Get; a rolling restart of an N-replica Deployment is N pods behind the
// SAME ReplicaSet, so this must collapse to one Get, not N.
func TestResolveOptIn_CachesReplicaSetGetAcrossPodsBehindSameOwner(t *testing.T) {
	policy := &sustainv1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "p"}} // empty selector: covers every pod
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "team-a",
		Name:        "web",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	rs := deploymentReplicaSet("team-a", "web-abc", "web")

	var rsGets int
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(ns, d, rs, policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok {
					rsGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c}

	for i := range 50 {
		pod := podWithRSOwner("team-a", fmt.Sprintf("web-abc-%d", i), "web-abc", "")
		name, _, owner, err := h.resolveOptIn(context.Background(), log.Log, pod)
		if err != nil {
			t.Fatalf("resolveOptIn: %v", err)
		}
		if name != "p" {
			t.Fatalf("resolveOptIn = %q, want %q", name, "p")
		}
		if !owner.Resolved || owner.Kind != "Deployment" || owner.Name != "web" {
			t.Fatalf("owner = %+v, want a resolved Deployment/web", owner)
		}
	}
	if rsGets != 1 {
		t.Errorf("expected 1 ReplicaSet Get for 50 pods behind the same ReplicaSet, got %d", rsGets)
	}
}

// TestResolveCachedPodOwner_KeyIncludesUID pins the fix for a review finding:
// the cache key used to be namespace/kind/name alone, so a ReplicaSet or Job
// name reused under a DIFFERENT top-level owner within the TTL would resolve
// to the stale owner's cached result. This simulates that exact race: a
// ReplicaSet "web-abc" owned by Deployment "web" is deleted and replaced,
// under the SAME name, by a ReplicaSet "web-abc" owned by a different
// Deployment "web2" — the only thing that changes for a pod behind the new
// one is the ownerRef UID.
func TestResolveCachedPodOwner_KeyIncludesUID(t *testing.T) {
	ctrl := true
	rs1 := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Controller: &ctrl,
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(rs1).Build()
	h := &Handler{Client: c}

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc-1",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-abc", UID: "uid-1", Controller: &ctrl,
			}},
		},
	}
	kind, name, err := h.resolveCachedPodOwner(context.Background(), pod1)
	if err != nil {
		t.Fatalf("resolveCachedPodOwner (pod1): %v", err)
	}
	if kind != "Deployment" || name != "web" {
		t.Fatalf("pod1: got (%q, %q), want (Deployment, web)", kind, name)
	}

	// Simulate the ReplicaSet being replaced under the same name by one owned
	// by a different Deployment — the pod behind it carries a different
	// ownerRef UID even though kind/name/namespace are identical.
	if err := c.Delete(context.Background(), rs1); err != nil {
		t.Fatalf("delete rs1: %v", err)
	}
	rs2 := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "web2", Controller: &ctrl,
			}},
		},
	}
	if err := c.Create(context.Background(), rs2); err != nil {
		t.Fatalf("create rs2: %v", err)
	}

	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "web-abc-2",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-abc", UID: "uid-2", Controller: &ctrl,
			}},
		},
	}
	kind, name, err = h.resolveCachedPodOwner(context.Background(), pod2)
	if err != nil {
		t.Fatalf("resolveCachedPodOwner (pod2): %v", err)
	}
	if kind != "Deployment" || name != "web2" {
		t.Fatalf("stale cache hit: got (%q, %q), want (Deployment, web2) — a different ownerRef UID must not "+
			"share pod1's cache entry", kind, name)
	}
}

// TestOwnerAnnotations_ConcurrentMissesCollapseToOneGet pins the fix for a
// review finding: ownerAnnCache only bounded the STEADY-STATE cost, since
// every cache entry is populated only after a completed Get. A genuinely
// concurrent cold-start burst — a rolling restart, exactly the case this
// cache exists for — could still let every one of N simultaneous misses
// issue its own Get before any of them had populated the cache.
//
// Handler.sfJoinHook lets this test hold the Get open until all N callers
// have joined the same in-flight resolution, so it genuinely exercises
// concurrency rather than passing by timing luck: if the fix were reverted
// (no singleflight), every one of the N goroutines would reach the
// interceptor's Get independently and this test would report N Gets, not 1.
func TestOwnerAnnotations_ConcurrentMissesCollapseToOneGet(t *testing.T) {
	const n = 50
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "team-a",
		Name:        "web",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}

	var joined int32
	allJoined := make(chan struct{})
	joinHook := func(string) {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}

	var gets int32
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					atomic.AddInt32(&gets, 1)
					<-allJoined // held open until every one of the N callers has joined
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c, sfJoinHook: joinHook}

	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]map[string]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = h.ownerAnnotations(context.Background(), "team-a", "Deployment", "web")
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&gets); got != 1 {
		t.Errorf("expected exactly 1 apiserver Get for %d concurrent misses on the same key, got %d", n, got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: ownerAnnotations: %v", i, err)
		}
		if results[i][sustainv1alpha1.PolicyAnnotation] != "p" {
			t.Fatalf("call %d: annotations = %v, want policy=p", i, results[i])
		}
	}
}

// TestResolveCachedPodOwner_ConcurrentMissesCollapseToOneGet is
// TestOwnerAnnotations_ConcurrentMissesCollapseToOneGet's counterpart for
// ownerRefCache/ownerRefSF: N pods behind the same ReplicaSet, admitted
// concurrently on a genuinely cold cache, must collapse to one ReplicaSet
// Get, not N.
func TestResolveCachedPodOwner_ConcurrentMissesCollapseToOneGet(t *testing.T) {
	const n = 50
	rs := deploymentReplicaSet("team-a", "web-abc", "web")

	var joined int32
	allJoined := make(chan struct{})
	joinHook := func(string) {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}

	var rsGets int32
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(rs).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok {
					atomic.AddInt32(&rsGets, 1)
					<-allJoined
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c, sfJoinHook: joinHook}

	var wg sync.WaitGroup
	kinds := make([]string, n)
	names := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		pod := podWithRSOwner("team-a", fmt.Sprintf("web-abc-%d", i), "web-abc", "")
		wg.Add(1)
		go func(i int, pod *corev1.Pod) {
			defer wg.Done()
			kinds[i], names[i], errs[i] = h.resolveCachedPodOwner(context.Background(), pod)
		}(i, pod)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&rsGets); got != 1 {
		t.Errorf("expected exactly 1 ReplicaSet Get for %d concurrent misses behind the same owner, got %d", n, got)
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("call %d: resolveCachedPodOwner: %v", i, errs[i])
		}
		if kinds[i] != "Deployment" || names[i] != "web" {
			t.Fatalf("call %d: got (%q, %q), want (Deployment, web)", i, kinds[i], names[i])
		}
	}
}

// TestOwnerAnnotations_WaiterRespectsOwnContextDeadline pins the other half
// of the singleflight contract: a caller that joins an in-flight resolution
// as a follower must not block past its OWN context deadline just because
// the leader's Get is slow. Admission's opt-in budget is per-admission, not
// per-leader, so a slow leader must not borrow budget from every follower
// waiting on it.
func TestOwnerAnnotations_WaiterRespectsOwnContextDeadline(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}

	leaderStarted := make(chan struct{})
	leaderCanProceed := make(chan struct{})
	var startedOnce sync.Once
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					startedOnce.Do(func() { close(leaderStarted) })
					<-leaderCanProceed
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c}

	// The leader goroutine must be JOINED before this test returns, not
	// merely released: a goroutine still inside ownerAnnotations after its
	// own test has finished runs concurrently with whatever test -shuffle
	// puts next, and while it is in there it reads Handler.sfJoinHook. Back
	// when that hook was a package-level var, this straggler read it while
	// the next test was writing it — a data race, and one that also fired
	// that test's barrier counter, releasing its barrier a caller early so a
	// late caller opened a second singleflight round and broke its
	// "exactly one Get" assertion. Releasing the leader is also done here
	// rather than only on the happy path, so an early t.Fatal above cannot
	// strand it forever and trip the package's goleak check.
	leaderDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseLeader := func() { releaseOnce.Do(func() { close(leaderCanProceed) }) }
	defer func() {
		releaseLeader()
		select {
		case <-leaderDone:
		case <-time.After(2 * time.Second):
			t.Error("leader goroutine still running after the test finished")
		}
	}()
	go func() {
		defer close(leaderDone)
		_, _ = h.ownerAnnotations(context.Background(), "team-a", "Deployment", "web")
	}()

	select {
	case <-leaderStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("leader's Get never started")
	}

	followerCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.ownerAnnotations(followerCtx, "team-a", "Deployment", "web")
	elapsed := time.Since(start)
	releaseLeader() // let the leader's Get finish; the deferred join waits for it

	if err == nil {
		t.Fatal("expected the follower to return an error on its own context deadline")
	}
	if elapsed > time.Second {
		t.Fatalf("follower blocked for %v past its own 20ms deadline; a slow leader must not borrow another caller's budget", elapsed)
	}
}

// TestOwnerAnnotations_CachesAcrossCallsBehindSameOwner pins the fix for a
// review finding: on a cluster where the pre-gate can't bound cost (a Policy
// with no namespace/label selector covers every pod), every unannotated pod
// CREATE was issuing an uncached owner Get. A rolling restart creates N pods
// behind the SAME owner, so the fix is a small TTL cache keyed by
// (namespace, kind, name) of the resolved top-level object — unlike
// ownerRefCache (see TestResolveCachedPodOwner_KeyIncludesUID above), this
// key needs no UID: it is keyed by the already-resolved workload identity
// itself, not by a ReplicaSet/Job ref that a deletion-and-recreate could
// reuse under a different owner. N calls for one owner must cost one Get.
func TestOwnerAnnotations_CachesAcrossCallsBehindSameOwner(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "team-a",
		Name:        "web",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"},
	}}
	var gets int
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					gets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c}

	for range 50 {
		ann, err := h.ownerAnnotations(context.Background(), "team-a", "Deployment", "web")
		if err != nil {
			t.Fatalf("ownerAnnotations: %v", err)
		}
		if ann[sustainv1alpha1.PolicyAnnotation] != "p" {
			t.Fatalf("annotations = %v, want policy=p", ann)
		}
	}
	if gets != 1 {
		t.Errorf("expected 1 apiserver Get for 50 calls behind the same owner, got %d", gets)
	}
}

// TestOwnerAnnotations_CachesNegativeResult pins that an unmanaged owner (no
// annotations, or the owner not existing at all — NotFound) is cached too:
// unmanaged workloads are the common case, so a Policy with no selector would
// otherwise re-Get every unmanaged owner on every one of its pods' admissions.
func TestOwnerAnnotations_CachesNegativeResult(t *testing.T) {
	var gets int
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					gets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c}

	for range 10 {
		ann, err := h.ownerAnnotations(context.Background(), "team-a", "Deployment", "gone")
		if err != nil {
			t.Fatalf("ownerAnnotations: %v", err)
		}
		if len(ann) != 0 {
			t.Fatalf("expected no annotations for a missing owner, got %v", ann)
		}
	}
	if gets != 1 {
		t.Errorf("expected 1 apiserver Get for 10 calls against a missing (NotFound) owner, got %d", gets)
	}
}

// TestOwnerAnnotations_TTLExpiryRefetches pins the other half of the cache
// contract: an entry past its TTL must be re-fetched, so a workload that
// gains an annotation is picked up within the TTL rather than staying stale
// forever.
func TestOwnerAnnotations_TTLExpiryRefetches(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}
	var gets int
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					gets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.ownerAnnCache.now = func() time.Time { return now }

	if _, err := h.ownerAnnotations(context.Background(), "team-a", "Deployment", "web"); err != nil {
		t.Fatalf("ownerAnnotations: %v", err)
	}
	// Still within TTL: no second Get.
	now = now.Add(ownerAnnotationsCacheTTL - time.Second)
	if _, err := h.ownerAnnotations(context.Background(), "team-a", "Deployment", "web"); err != nil {
		t.Fatalf("ownerAnnotations: %v", err)
	}
	if gets != 1 {
		t.Fatalf("expected 1 Get within the TTL window, got %d", gets)
	}
	// Past TTL: must re-fetch.
	now = now.Add(2 * time.Second)
	if _, err := h.ownerAnnotations(context.Background(), "team-a", "Deployment", "web"); err != nil {
		t.Fatalf("ownerAnnotations: %v", err)
	}
	if gets != 2 {
		t.Errorf("expected a second Get after the TTL expired, got %d", gets)
	}
}

// TestOwnerAnnotations_CacheRetainsOnlyPolicyKeys pins the fix for a review
// finding: the cache used to store the owner's ENTIRE annotations map, so a
// kubectl-apply-managed owner — which carries
// kubectl.kubernetes.io/last-applied-configuration, the whole serialized
// object, up to Kubernetes' 256KB per-object annotation ceiling — could
// balloon the cache's real memory footprint far past what its bounded entry
// count (ownerAnnotationsCacheMaxEntries) suggests. Only
// sustainv1alpha1.PolicyAnnotation and OptOutAnnotation are ever read back
// out of a cached entry (policymatch.ResolvePolicy's decidesAt), so those
// are the only keys the fix (cacheableOwnerAnnotations) may retain. This
// test seeds an owner with a large unrelated annotation plus an unrelated
// short one alongside the policy annotation, and would fail if
// ownerAnnotations ever went back to caching obj.GetAnnotations() directly.
func TestOwnerAnnotations_CacheRetainsOnlyPolicyKeys(t *testing.T) {
	large := strings.Repeat("x", 8192)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a",
		Name:      "web",
		Annotations: map[string]string{
			sustainv1alpha1.PolicyAnnotation:                   "p",
			"kubectl.kubernetes.io/last-applied-configuration": large,
			"some.other/unrelated-annotation":                  "unrelated",
		},
	}}
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(d).Build()
	h := &Handler{Client: c}

	ann, err := h.ownerAnnotations(context.Background(), "team-a", "Deployment", "web")
	if err != nil {
		t.Fatalf("ownerAnnotations: %v", err)
	}
	if ann[sustainv1alpha1.PolicyAnnotation] != "p" {
		t.Fatalf("annotations = %v, want policy=p", ann)
	}

	cached, ok := h.ownerAnnCache.get("team-a/Deployment/web")
	if !ok {
		t.Fatalf("expected the entry to be cached")
	}
	if len(cached) > 2 {
		t.Fatalf("cached entry retains %d keys (%v), want at most the 2 keys ResolvePolicy ever reads — "+
			"this fails if the whole annotations map is cached", len(cached), cached)
	}
	if _, ok := cached["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Fatalf("cached entry retains the large unrelated last-applied-configuration annotation: keys=%v", mapKeys(cached))
	}
	if _, ok := cached["some.other/unrelated-annotation"]; ok {
		t.Fatalf("cached entry retains an unrelated annotation: keys=%v", mapKeys(cached))
	}
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestDisableForCoversOwnerAnnotationKinds locks the invariant flagged in
// review: every kind ownerAnnotations can Get (via workload.ObjectForKind)
// must appear in NewCached's DisableFor (k8s.OwnerChainDisableFor), or the
// webhook's first pod CREATE that reaches that kind after a restart stands up
// a cluster-wide informer over it instead of costing a single apiserver Get —
// the informer's LIST is charged against optInTimeout, so on a large cluster
// it can silently time out and fail open, and it leaves the process holding a
// watch over every object of that kind for reads that happen at most once per
// pod CREATE. workload.OwnerChainKinds and OwnerChainDisableFor are
// maintained in two different packages with nothing else forcing them to
// agree; this test is that force.
func TestDisableForCoversOwnerAnnotationKinds(t *testing.T) {
	disabled := map[reflect.Type]bool{}
	for _, obj := range k8sclient.OwnerChainDisableFor() {
		disabled[reflect.TypeOf(obj)] = true
	}
	for _, kind := range workload.OwnerChainKinds() {
		typ := reflect.TypeOf(workload.ObjectForKind(kind))
		if !disabled[typ] {
			t.Errorf("ownerAnnotations can Get kind %q (%s) but k8s.OwnerChainDisableFor does not list it; "+
				"its first Get would stand up a cluster-wide informer on the admission hot path", kind, typ)
		}
	}
}
