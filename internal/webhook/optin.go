package webhook

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
	"github.com/noony/k8s-sustain/internal/workload"
)

// optInTimeout is one shared budget for the whole opt-in chain — the pre-gate
// list, the Namespace read and the owner resolution plus its object Get.
//
// A shared budget rather than a per-call one because this chain runs only for
// pods that carry no annotation of their own, i.e. potentially EVERY pod
// create in the cluster. Stacking per-call deadlines here would let one slow
// path consume the admission budget that the Policy and
// WorkloadRecommendation reads still need. admissionTimeout remains the outer
// ceiling for all of it; this bound just stops the cheapest-to-skip work from
// eating the most.
const optInTimeout = 2 * time.Second

// podOwner carries a resolved pod owner so admit() does not resolve it twice.
// Resolved distinguishes "resolved to nothing" (an orphan pod) from "not
// resolved yet".
type podOwner struct {
	Kind, Name string
	Resolved   bool
}

// resolveOptIn finds the Policy a pod opts into when the pod itself carries no
// policy annotation, by reading the levels pods do not inherit: the owning
// workload's own metadata, and the Namespace's.
//
// Only ONE step here short-circuits: the pre-gate. If no Policy's selector
// could claim a pod here at all, this returns immediately with zero further
// reads — that is what keeps the feature from taxing every unmanaged pod
// create in the cluster, which is the overwhelmingly common case.
//
// Past the pre-gate, the remaining two reads always run TOGETHER, not as a
// short-circuiting chain, even though resolution is most-specific-first
// (pod template, then workload, then Namespace): the Namespace is fetched
// first here purely because it is the cheaper of the two (served from the
// informer cache, see k8s.cachedKinds — no apiserver round-trip), but knowing
// the Namespace's annotations can never tell this function whether the more
// specific workload level would have decided first, so the owner walk plus
// its Get (resolveCachedPodOwner, then h.ownerAnnotations — both go straight
// to the apiserver on a cache miss, see k8s.OwnerChainDisableFor) always runs
// regardless of what the Namespace said. Ordering these two by cost rather
// than by specificity is safe only because ResolvePolicy is applied once,
// at the end, over all three levels together — it is not safe to skip the
// owner read based on the Namespace alone. See ownerAnnotationsCache and
// Handler.ownerRefCache for how both Gets' cost is actually bounded on a
// cluster where the pre-gate can't do it (a Policy with no namespace/label
// selector covers every pod).
//
// Every error is returned for the caller to log and fail open on; none of them
// may deny a pod.
func (h *Handler) resolveOptIn(ctx context.Context, logger logr.Logger, pod *corev1.Pod) (string, policymatch.Level, podOwner, error) {
	var owner podOwner

	covered, err := h.anyPolicyCovers(ctx, pod.Namespace, pod.Labels)
	if err != nil {
		return "", policymatch.LevelNone, owner, err
	}
	if !covered {
		logger.V(1).Info("no policy selector covers this pod; skipping multi-level opt-in resolution")
		return "", policymatch.LevelNone, owner, nil
	}

	var ns corev1.Namespace
	if err := h.Client.Get(ctx, types.NamespacedName{Name: pod.Namespace}, &ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", policymatch.LevelNone, owner, fmt.Errorf("reading namespace %s: %w", pod.Namespace, err)
		}
		// A pod being admitted into a namespace the apiserver says does not
		// exist is a race we simply resolve as "no namespace-level opt-in".
	}

	ownerKind, ownerName, err := h.resolveCachedPodOwner(ctx, pod)
	if err != nil {
		return "", policymatch.LevelNone, owner, err
	}
	owner = podOwner{Kind: ownerKind, Name: ownerName, Resolved: true}

	workloadAnnotations, err := h.ownerAnnotations(ctx, pod.Namespace, ownerKind, ownerName)
	if err != nil {
		return "", policymatch.LevelNone, owner, err
	}

	// The pod's own annotations are still passed as the most specific level.
	// This is defensive rather than load-bearing today: admit() (handler.go)
	// already returns on a pod-level opt-out before resolveOptIn is ever
	// reached, so by the time execution gets here the pod's annotations
	// cannot carry one. Passing them anyway costs nothing, keeps this
	// function correct on its own terms rather than relying on a caller
	// invariant holding forever, and matches ResolvePolicy's normal call
	// shape everywhere else in the codebase.
	name, level := policymatch.ResolvePolicy(pod.Annotations, workloadAnnotations, ns.Annotations)
	return name, level, owner, nil
}

// resolveCachedPodOwner is workload.ResolvePodOwner with the ReplicaSet/Job
// Get bounded by Handler.ownerRefCache, for the same reason ownerAnnotations
// is bounded by h.ownerAnnCache: anyPolicyCovers cannot narrow this down on a
// cluster where some Policy has no namespace/label selector, so without a
// cache every unannotated pod CREATE pays this Get uncached — and with the
// Quick Start Policy's empty selector, that is every pod CREATE in the
// cluster. A rolling restart of an N-replica Deployment creates N pods behind
// the SAME ReplicaSet, so keying the cache by the pod's own immediate
// controller ownerRef (namespace/kind/name/UID — before any Get, not the
// resolved Deployment/CronJob workload.ResolveControllerOwner may walk up to)
// collapses that burst from N Gets to one. UID is part of the key so a
// ReplicaSet/Job name reused under a different top-level owner within the TTL
// can never resolve to a stale owner's cached result.
//
// A pod with no controller ownerRef at all (metav1.GetControllerOf returns
// nil — a bare, standalone pod) never reaches the cache: there is no ref to
// key by, and ResolvePodOwner already costs zero Gets for that case, so there
// is nothing to save. An owner whose Get succeeds but which itself has no
// controller owner (an orphaned ReplicaSet/Job, not attached to any
// Deployment/CronJob) IS cached: workload.ResolveControllerOwner's terminal
// ("ReplicaSet", ref.Name) / ("Job", ref.Name) result is stored under that
// same ownerRef key like any other outcome, so N pods behind one orphaned
// ReplicaSet collapse the same way N pods behind an owned one do.
func (h *Handler) resolveCachedPodOwner(ctx context.Context, pod *corev1.Pod) (kind, name string, err error) {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return "", "", nil
	}
	// UID is part of the key, not just namespace/kind/name: a ReplicaSet or
	// Job name reused under a different top-level owner within the TTL must
	// not resolve to the stale owner's cached result. The UID is already on
	// the OwnerReference in hand, so this costs nothing.
	key := pod.Namespace + "/" + ref.Kind + "/" + ref.Name + "/" + string(ref.UID)
	if v, ok := h.ownerRefCache.get(key); ok {
		return v.Kind, v.Name, nil
	}

	namespace := pod.Namespace
	refCopy := *ref
	ch := h.ownerRefSF.DoChan(key, sfPanicSafe(log.FromContext(ctx), panicLabelOwnerRef, key, func() (any, error) {
		return h.fetchAndCacheOwnerRef(namespace, refCopy, key)
	}))
	if h.sfJoinHook != nil {
		h.sfJoinHook(key)
	}
	select {
	case <-ctx.Done():
		// This caller's own deadline (the shared optInTimeout budget) expired
		// while waiting on another caller's in-flight Get — do not block past
		// it just because the leader is slow. The leader keeps running for
		// whichever other waiters still have budget left; only this call
		// returns early, and it returns an error like any other failed
		// resolution, so admit() fails open on it as usual.
		return "", "", ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return "", "", res.Err
		}
		v := res.Val.(resolvedOwnerRef)
		return v.Kind, v.Name, nil
	}
}

// sfPanicSafe wraps a singleflight leader function so a panic inside it can
// never escape onto the group's own goroutine, turning it instead into an
// ordinary error every caller already fails open on.
//
// This is not defensive noise. Both leader functions here used to run inline
// on the request goroutine, where httpx.WithRecovery (internal/httpx) turned
// any panic into a 500 — which for a webhook IS failing open. Behind DoChan
// they run on singleflight's internal doCall goroutine instead, and
// golang.org/x/sync/singleflight deliberately makes a leader panic
// UNRECOVERABLE as soon as at least one caller has joined via DoChan: rather
// than deliver it as an error, it re-raises it on a bare `go panic(e)`
// goroutine with no recover anywhere on its stack. A nil entry in the scheme,
// a nil deref in the RESTMapper, an informer bug — anything that panics under
// the owner Get would abort the ENTIRE webhook process, killing every
// in-flight admission with it; under failurePolicy: Fail that blocks every
// Pod CREATE in the cluster until the pod is back. That is the exact opposite
// of the fail-open contract the rest of this package upholds.
//
// Recovering here puts the panic back on a path the callers already handle:
// the error reaches the leader and every parked waiter alike, nothing is
// cached (an error is never a legitimate result), and admit() admits the pod
// unmutated exactly as it would on a failed Get. The panic is not swallowed —
// it is logged with its stack and counted on PanicTotal, the same counter the
// httpx middleware feeds, so it stays as visible as it was when the middleware
// was the one catching it.
//
// op must be a bounded constant (see the panicLabel* constants in metrics.go),
// never the key: the key is caller-controlled and would blow up the counter's
// label cardinality. It is logged instead, where cardinality does not matter.
func sfPanicSafe(logger logr.Logger, op, key string, fn func() (any, error)) func() (any, error) {
	return func() (val any, err error) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logger.Error(nil, "recovered panic in singleflight owner resolution",
				"operation", op,
				"key", key,
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			PanicTotal.WithLabelValues(op).Inc()
			// A nil val is safe: both call sites type-assert res.Val only
			// when res.Err is nil.
			val, err = nil, fmt.Errorf("panic resolving %s for %s: %v", op, key, rec)
		}()
		return fn()
	}
}

// fetchAndCacheOwnerRef performs the single Get that backs a whole
// singleflight-collapsed burst for resolveCachedPodOwner, then populates
// ownerRefCache. It runs on a context detached from any individual admission
// (a fixed apiCallTimeout budget instead of a caller's ctx): this function
// keeps running in the background for as long as OTHER waiters may still be
// blocked on it even after the particular caller that happened to become
// singleflight's leader has itself timed out and returned — tying its
// lifetime to that one caller's context would abort the shared work for
// every other admission still waiting on it.
//
// It is invoked through sfPanicSafe, never raw: a panic on singleflight's own
// goroutine is unrecoverable by design, so it must be turned into an ordinary
// error here rather than taking the process down with it.
func (h *Handler) fetchAndCacheOwnerRef(namespace string, ref metav1.OwnerReference, key string) (any, error) {
	getCtx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	kind, name, err := workload.ResolveControllerOwner(getCtx, h.Client, namespace, ref)
	if err != nil {
		// Not cached: an error is not a legitimate result, matching the
		// pre-singleflight behaviour of never caching a failed Get.
		return resolvedOwnerRef{}, err
	}
	v := resolvedOwnerRef{Kind: kind, Name: name}
	h.ownerRefCache.set(key, v, ownerRefCacheTTL, ownerRefCacheMaxEntries)
	return v, nil
}

// anyPolicyCovers reports whether at least one Policy's selector could claim a
// pod in this namespace with these labels. Served from the Policy informer
// cache, so it is a local list and some selector compiles — no apiserver
// round-trip.
//
// A Policy with an unparseable label selector is skipped rather than treated as
// covering: that matches admit()'s fail-open handling of the same condition,
// where a broken selector never causes an injection.
func (h *Handler) anyPolicyCovers(ctx context.Context, namespace string, podLabels map[string]string) (bool, error) {
	var list sustainv1alpha1.PolicyList
	if err := h.Client.List(ctx, &list); err != nil {
		return false, fmt.Errorf("listing policies: %w", err)
	}
	for i := range list.Items {
		p := &list.Items[i]
		sel, err := policymatch.SelectorOK(p.Spec.Selector.LabelSelector)
		if err != nil {
			continue
		}
		if policymatch.MatchesSelector(p, namespace, podLabels, h.ExcludedNamespaces, sel) {
			return true, nil
		}
	}
	return false, nil
}

// ownerAnnotations reads the workload object's own metadata.annotations — the
// level pods do not inherit. An unknown kind (a custom controller) and a
// missing object both resolve to nil rather than an error: neither is a
// failure, both simply mean there is no workload level to read.
//
// The kind→object table lives in workload.ObjectForKind rather than here so
// the OOM watcher (internal/oomwatch) can read the same workload level
// without duplicating it. TestDisableForCoversOwnerAnnotationKinds still
// cross-checks that shared table against k8s.OwnerChainDisableFor: every kind
// read here must be there, or its first Get in the process stands up a
// cluster-wide informer on the admission hot path instead of costing one Get
// (see NewCached's doc comment for why that is a silent, self-healing failure
// that is very hard to diagnose after the fact).
//
// This Get is served from h.ownerAnnCache when possible. It exists at all
// because the pre-gate (anyPolicyCovers) does not bound this cost on a
// cluster where every Policy covers every namespace: with no selector to
// narrow it down, every unannotated pod CREATE would otherwise pay for this
// Get, uncached, on the admission hot path — see ownerAnnotationsCache's doc
// for the TTL, the staleness trade-off it accepts, and why a rolling restart
// (N pods behind one owner) is exactly the case it collapses.
func (h *Handler) ownerAnnotations(ctx context.Context, namespace, kind, name string) (map[string]string, error) {
	if workload.ObjectForKind(kind) == nil {
		return nil, nil
	}
	key := namespace + "/" + kind + "/" + name
	if v, ok := h.ownerAnnCache.get(key); ok {
		return v, nil
	}

	ch := h.ownerAnnSF.DoChan(key, sfPanicSafe(log.FromContext(ctx), panicLabelOwnerAnnotations, key, func() (any, error) {
		return h.fetchAndCacheOwnerAnnotations(namespace, kind, name, key)
	}))
	if h.sfJoinHook != nil {
		h.sfJoinHook(key)
	}
	select {
	case <-ctx.Done():
		// See fetchAndCacheOwnerRef's doc for why the leader's Get keeps
		// running on its own detached budget rather than this ctx: this
		// caller must not block past its own deadline just because the
		// leader (or another waiter's deadline) is slower, but that must not
		// abort the shared Get for whoever else is still waiting on it.
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(map[string]string), nil
	}
}

// fetchAndCacheOwnerAnnotations performs the single Get that backs a whole
// singleflight-collapsed burst for ownerAnnotations, then populates
// ownerAnnCache. See fetchAndCacheOwnerRef's doc for why it runs on its own
// detached, fixed-budget context rather than any one caller's ctx.
func (h *Handler) fetchAndCacheOwnerAnnotations(namespace, kind, name, key string) (any, error) {
	obj := workload.ObjectForKind(kind)
	if obj == nil {
		// Unreachable in practice — ownerAnnotations already checked this
		// before ever joining the singleflight group — but keeps this
		// function correct standalone rather than relying on the caller's
		// invariant holding forever.
		var nilAnn map[string]string
		return nilAnn, nil
	}
	getCtx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	if err := h.Client.Get(getCtx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			// Cached too: an owner that does not exist is as good a negative
			// result as one that exists but carries no annotations, and
			// unmanaged/ephemeral workloads are the common case this cache
			// exists to stop re-Getting on every admission.
			h.ownerAnnCache.set(key, nil, ownerAnnotationsCacheTTL, ownerAnnotationsCacheMaxEntries)
			var nilAnn map[string]string
			return nilAnn, nil
		}
		// Not cached: an error is not a legitimate result, matching the
		// pre-singleflight behaviour of never caching a failed Get.
		return map[string]string(nil), fmt.Errorf("reading %s %s/%s: %w", kind, namespace, name, err)
	}
	ann := cacheableOwnerAnnotations(obj.GetAnnotations())
	h.ownerAnnCache.set(key, ann, ownerAnnotationsCacheTTL, ownerAnnotationsCacheMaxEntries)
	return ann, nil
}

// cacheableOwnerAnnotations copies out of the owner's full
// metadata.annotations only the two keys anything downstream of
// ownerAnnotations ever reads — sustainv1alpha1.PolicyAnnotation and
// sustainv1alpha1.OptOutAnnotation, both consulted solely by
// policymatch.ResolvePolicy's decidesAt check — into a small fresh map.
//
// This exists so that map, not the owner's own map, is what
// ownerAnnCache.set stores. Storing the live map directly would mean: (1)
// the cache retains an owner's ENTIRE annotations map for as long as the
// entry lives, which for anything kubectl-apply-managed includes
// kubectl.kubernetes.io/last-applied-configuration — the whole serialized
// object, routinely a few KB and up to Kubernetes' 256KB per-object
// annotation ceiling — turning ownerAnnotationsCacheMaxEntries's 4096-entry
// bound into a multi-hundred-MB-to-1GB one instead of the small, genuinely
// bounded cache its doc comment claims; and (2) the exact same map instance
// would be handed to every concurrent admission that hits this key, so a
// future caller that starts mutating the returned map would be racing every
// other admission sharing it. Copying only the two keys ever read closes
// both at once: nothing unrelated is retained, and the stored map is never
// the same instance as the object's live one, so there is nothing left to
// alias.
//
// Returns nil (not an empty map) when neither key is present, matching what
// a missing/unannotated owner already produces — callers and the cache's own
// "nil is a legitimate negative result" contract do not need a special case
// for "present but empty".
func cacheableOwnerAnnotations(ann map[string]string) map[string]string {
	if len(ann) == 0 {
		return nil
	}
	var out map[string]string
	for _, k := range []string{sustainv1alpha1.PolicyAnnotation, sustainv1alpha1.OptOutAnnotation} {
		if v, ok := ann[k]; ok {
			if out == nil {
				out = make(map[string]string, 2)
			}
			out[k] = v
		}
	}
	return out
}
