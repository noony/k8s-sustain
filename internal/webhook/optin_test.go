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

	"github.com/prometheus/client_golang/prometheus/testutil"
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

	// The ad-hoc scheme in newAdmitEnv does not register corev1, which the
	// Namespace read needs; config.Scheme() is the full manager scheme.
	objs := append([]client.Object{ns, d, rs}, policies...)
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(objs...).Build()
	return &Handler{Client: c}, pod
}

// Replays the shared contract table against the webhook's own wiring.
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

// Under a selector-less Policy, anyPolicyCovers is unconditionally true, so
// every pod CREATE reaches the owner-chain walk. A rolling restart is N pods
// behind the SAME ReplicaSet and must collapse to one Get, not N.
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

// A ReplicaSet or Job name reused under a DIFFERENT top-level owner within the
// TTL must not resolve to the stale owner: the ownerRef UID is the only thing
// that changes for a pod behind the replacement, so it has to be in the key.
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

// ownerAnnCache alone bounds only steady-state cost — entries populate after a
// completed Get — so a cold-start burst could still issue N simultaneous Gets.
// sfJoinHook holds the Get open until all N callers have joined, so this
// exercises real concurrency rather than passing on timing luck.
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

// The ownerRefCache/ownerRefSF counterpart: N pods behind the same ReplicaSet,
// admitted concurrently on a cold cache, must collapse to one Get.
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

// singleflight re-raises a leader panic with a bare `go panic(e)` once a DoChan
// caller has joined, with no recover on its stack — so the owner Get running
// off the request goroutine escapes httpx.WithRecovery and would abort the
// process, blocking every Pod CREATE under failurePolicy: Fail.
//
// Without sfPanicSafe this test does not fail, it CRASHES the test binary.
func TestOwnerAnnotations_LeaderPanicFailsOpenForEveryWaiter(t *testing.T) {
	const n = 2 // the leader, plus one caller parked on the shared channel
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "web"}}

	// The Get is held until BOTH callers have joined, so the panic happens
	// with a waiter genuinely parked on the DoChan channel — the only shape in
	// which singleflight escalates it to an unrecoverable re-panic.
	var joined int32
	allJoined := make(chan struct{})
	joinHook := func(string) {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}

	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(d).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					<-allJoined
					panic("owner Get exploded")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c, sfJoinHook: joinHook}

	before := testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelOwnerAnnotations))

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

	for i := range n {
		switch {
		case errs[i] == nil:
			t.Errorf("call %d: got a nil error, want the recovered panic surfaced as an ordinary error to fail open on", i)
		case !strings.Contains(errs[i].Error(), "panic"):
			t.Errorf("call %d: error = %v, want it to name the recovered panic", i, errs[i])
		}
		if results[i] != nil {
			t.Errorf("call %d: annotations = %v, want nil", i, results[i])
		}
	}
	if _, ok := h.ownerAnnCache.get("team-a/Deployment/web"); ok {
		t.Error("a panicking resolution must not populate ownerAnnCache")
	}
	if delta := testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelOwnerAnnotations)) - before; delta != 1 {
		t.Errorf("PanicTotal[%s] delta = %v, want 1", panicLabelOwnerAnnotations, delta)
	}
}

// The ownerRefSF counterpart: the pod→owner walk's Get runs on the same
// singleflight goroutine and must contain a panic the same way.
func TestResolveCachedPodOwner_LeaderPanicFailsOpenForEveryWaiter(t *testing.T) {
	const n = 2
	rs := deploymentReplicaSet("team-a", "web-abc", "web")

	var joined int32
	allJoined := make(chan struct{})
	joinHook := func(string) {
		if atomic.AddInt32(&joined, 1) == n {
			close(allJoined)
		}
	}

	c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(rs).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok {
					<-allJoined
					panic("ownerRef walk exploded")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	h := &Handler{Client: c, sfJoinHook: joinHook}

	before := testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelOwnerRef))

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

	for i := range n {
		switch {
		case errs[i] == nil:
			t.Errorf("call %d: got a nil error, want the recovered panic surfaced as an ordinary error to fail open on", i)
		case !strings.Contains(errs[i].Error(), "panic"):
			t.Errorf("call %d: error = %v, want it to name the recovered panic", i, errs[i])
		}
		if kinds[i] != "" || names[i] != "" {
			t.Errorf("call %d: got (%q, %q), want no resolved owner", i, kinds[i], names[i])
		}
	}
	if _, ok := h.ownerRefCache.get("team-a/ReplicaSet/web-abc/"); ok {
		t.Error("a panicking resolution must not populate ownerRefCache")
	}
	if delta := testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelOwnerRef)) - before; delta != 1 {
		t.Errorf("PanicTotal[%s] delta = %v, want 1", panicLabelOwnerRef, delta)
	}
}

// A follower must not block past its OWN context deadline when the leader's Get
// is slow: the opt-in budget is per-admission, not per-leader.
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

	// The leader must be JOINED before this test returns, not merely released:
	// a straggler inside ownerAnnotations runs concurrently with whatever test
	// -shuffle puts next and reads Handler.sfJoinHook while it is in there.
	// Releasing here rather than only on the happy path also keeps an early
	// t.Fatal from stranding it and tripping the package's goleak check.
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

// N calls for one owner must cost one Get. Unlike ownerRefCache (see
// TestResolveCachedPodOwner_KeyIncludesUID), this key needs no UID: it names
// the already-resolved workload identity, not a ReplicaSet/Job ref that a
// delete-and-recreate could reuse under a different owner.
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

// An unmanaged owner (no annotations, or NotFound) must be cached too:
// unmanaged workloads are the common case, so a selector-less Policy would
// otherwise re-Get every one of them on every admission.
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

// An entry past its TTL must be re-fetched, so a workload that gains an
// annotation is picked up rather than staying stale forever.
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

// Caching the owner's entire annotations map would balloon the footprint far
// past what ownerAnnotationsCacheMaxEntries suggests — a kubectl-apply-managed
// owner carries last-applied-configuration, up to Kubernetes' 256KB ceiling.
// Only PolicyAnnotation and OptOutAnnotation are ever read back out, so those
// are the only keys cacheableOwnerAnnotations may retain.
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

// Every kind ownerAnnotations can Get must appear in k8s.OwnerChainDisableFor,
// or the first pod CREATE reaching that kind stands up a cluster-wide informer
// instead of costing one Get — its LIST is charged against optInTimeout, so on
// a large cluster it silently times out and fails open. The two lists live in
// different packages with nothing else forcing them to agree.
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
