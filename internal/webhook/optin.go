package webhook

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/governance"
	"github.com/noony/k8s-sustain/internal/workload"
)

// governanceTimeout is one shared budget for every read governance.Decide
// makes for a pod. A shared budget keeps a slow path here from eating the
// admission budget the WorkloadRecommendation read still needs.
const governanceTimeout = 2 * time.Second

// admissionReader is the governance.Reader of one admission: Policies and
// Namespaces come from the informer cache, the owner chain through the TTL
// caches below.
type admissionReader struct{ h *Handler }

func (r admissionReader) Policies(ctx context.Context) (*governance.Policies, error) {
	return r.h.policies.List(ctx, r.h.Client, r.h.ExcludedNamespaces)
}

func (r admissionReader) PodOwner(ctx context.Context, pod *corev1.Pod) (string, string, error) {
	return r.h.resolveCachedPodOwner(ctx, pod)
}

func (r admissionReader) WorkloadAnnotations(ctx context.Context, namespace, kind, name string) (map[string]string, error) {
	return r.h.ownerAnnotations(ctx, namespace, kind, name)
}

func (r admissionReader) NamespaceAnnotations(ctx context.Context, namespace string) (map[string]string, error) {
	return workload.NamespaceAnnotations(ctx, r.h.Client, namespace)
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
			reportPanic(logger, op, rec, "key", key)
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
