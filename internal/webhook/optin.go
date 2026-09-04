package webhook

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/policymatch"
	"github.com/noony/k8s-sustain/internal/workload"
)

// optInTimeout is one shared budget for the whole opt-in chain, which runs
// for every pod without its own annotation. A shared budget keeps a slow
// path here from eating the admission budget the Policy and
// WorkloadRecommendation reads still need.
const optInTimeout = 2 * time.Second

// podOwner carries a resolved pod owner so admit() does not resolve it twice.
// Resolved distinguishes an orphan pod from "not resolved yet".
type podOwner struct {
	Kind, Name string
	Resolved   bool
}

// resolveOptIn finds the Policy a pod opts into when the pod itself carries no
// policy annotation, by reading the owning workload's and the Namespace's
// annotations. The pre-gate short-circuits when no Policy could cover the
// pod; past it both reads always run, because ResolvePolicy needs all levels
// together. Every error is returned for the caller to fail open on.
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

	nsAnnotations, err := workload.NamespaceAnnotations(ctx, h.Client, pod.Namespace)
	if err != nil {
		return "", policymatch.LevelNone, owner, err
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

	name, level := policymatch.ResolvePolicy(pod.Annotations, workloadAnnotations, nsAnnotations)
	return name, level, owner, nil
}

// resolveCachedPodOwner is workload.ResolvePodOwner with the ReplicaSet/Job
// Get memoised in ownerRefCache and collapsed through singleflight, keyed by
// the pod's immediate controller ownerRef. A pod with no controller ownerRef
// costs zero Gets and never touches the cache.
func (h *Handler) resolveCachedPodOwner(ctx context.Context, pod *corev1.Pod) (kind, name string, err error) {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return "", "", nil
	}
	// UID is part of the key so a ReplicaSet/Job name reused under a different
	// owner within the TTL cannot hit the stale entry.
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
		// Only this caller gives up; the leader keeps running for other waiters.
		return "", "", ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return "", "", res.Err
		}
		v := res.Val.(resolvedOwnerRef)
		return v.Kind, v.Name, nil
	}
}

// sfPanicSafe wraps a singleflight leader function so a panic inside it
// becomes an ordinary error. singleflight re-raises a leader panic on a bare
// goroutine once a DoChan caller has joined, which would take down the whole
// webhook process instead of failing open. The panic is logged with its stack
// and counted on PanicTotal. op must be a bounded constant, never the key.
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
			val, err = nil, fmt.Errorf("panic resolving %s for %s: %v", op, key, rec)
		}()
		return fn()
	}
}

// fetchAndCacheOwnerRef performs the single Get backing a singleflight burst
// and populates ownerRefCache. It runs on a detached context so a timed-out
// leader does not abort the shared work for waiters that still have budget.
func (h *Handler) fetchAndCacheOwnerRef(namespace string, ref metav1.OwnerReference, key string) (any, error) {
	getCtx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	kind, name, err := workload.ResolveControllerOwner(getCtx, h.Client, namespace, ref)
	if err != nil {
		return resolvedOwnerRef{}, err
	}
	v := resolvedOwnerRef{Kind: kind, Name: name}
	h.ownerRefCache.set(key, v, ownerRefCacheTTL, ownerRefCacheMaxEntries)
	return v, nil
}

// anyPolicyCovers reports whether at least one Policy's selector could claim a
// pod in this namespace with these labels. A Policy with an unparseable label
// selector is skipped, matching admit()'s fail-open handling.
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

// ownerAnnotations reads the workload object's own metadata.annotations,
// served from ownerAnnCache when possible. An unknown kind and a missing
// object both resolve to nil rather than an error.
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
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(map[string]string), nil
	}
}

// fetchAndCacheOwnerAnnotations performs the single Get backing a singleflight
// burst and populates ownerAnnCache. A NotFound owner is cached as nil; an
// error is never cached.
func (h *Handler) fetchAndCacheOwnerAnnotations(namespace, kind, name, key string) (any, error) {
	getCtx, cancel := context.WithTimeout(context.Background(), apiCallTimeout)
	defer cancel()
	raw, err := workload.OwnerAnnotations(getCtx, h.Client, namespace, kind, name)
	if err != nil {
		return map[string]string(nil), err
	}
	ann := cacheableOwnerAnnotations(raw)
	h.ownerAnnCache.set(key, ann, ownerAnnotationsCacheTTL, ownerAnnotationsCacheMaxEntries)
	return ann, nil
}

// cacheableOwnerAnnotations copies only the policy and opt-out keys into a
// fresh map, so the cache neither retains the owner's whole annotations map
// (last-applied-configuration can be hundreds of KB) nor aliases it across
// admissions. Returns nil when neither key is present.
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
