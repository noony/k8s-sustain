package webhook

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/cache"
	"github.com/noony/k8s-sustain/internal/governance"
	"github.com/noony/k8s-sustain/internal/workload"
)

// ownerAnnotationsCacheTTL bounds how long a cached owner-annotations lookup
// is trusted. 30s is sized against the read-amplification this cache exists
// to fix — a rolling restart creates N pods behind the same owner in a burst —
// and is deliberately far shorter than the staleness budget a Recommendation
// is served for: this is load-shedding for a hot read, not a source of truth.
//
// The cost is that a workload gaining or losing the policy annotation may take
// up to this TTL to be seen by admission. Acceptable because the controller
// reconciles independently and resizes such a pod once it catches up.
const ownerAnnotationsCacheTTL = 30 * time.Second

// ownerAnnotationsCacheMaxEntries bounds the cache's footprint independent of
// TTL, so many distinct owners cannot grow it without bound between expiries.
// Entries stay small only because cacheableOwnerAnnotations copies out just the
// two keys governance reads — an owner's raw annotations can carry
// last-applied-configuration, up to Kubernetes' 256KB per-object ceiling.
const ownerAnnotationsCacheMaxEntries = 4096

// ownerRefCacheTTL and ownerRefCacheMaxEntries govern the pod-owner cache.
// Same values and reasoning as the owner-annotations pair — a rolling restart
// is the pattern both bound — but named separately so either can be retuned on
// its own.
const (
	ownerRefCacheTTL        = ownerAnnotationsCacheTTL
	ownerRefCacheMaxEntries = ownerAnnotationsCacheMaxEntries
)

// admissionReader is the governance.Reader of one admission: Policies and
// Namespaces come from the informer cache, the owner chain through the
// handler's owner caches.
type admissionReader struct{ h *Handler }

func (r admissionReader) Policies(ctx context.Context) (*governance.Policies, error) {
	return r.h.Policies.List(ctx, r.h.Client)
}

// PodOwner costs a pod with no controller ownerRef no Get and no cache entry.
func (r admissionReader) PodOwner(ctx context.Context, pod *corev1.Pod) (string, string, error) {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return "", "", nil
	}
	owner, err := r.h.ownerCaches().podOwners.Get(ctx, controllerRef{
		Namespace: pod.Namespace, Kind: ref.Kind, Name: ref.Name, UID: ref.UID,
	})
	return owner.Kind, owner.Name, err
}

// WorkloadAnnotations resolves an unknown kind to nil without a Get.
func (r admissionReader) WorkloadAnnotations(ctx context.Context, namespace, kind, name string) (map[string]string, error) {
	if workload.ObjectForKind(kind) == nil {
		return nil, nil
	}
	return r.h.ownerCaches().annotations.Get(ctx, workloadRef{Namespace: namespace, Kind: kind, Name: name})
}

func (r admissionReader) NamespaceAnnotations(ctx context.Context, namespace string) (map[string]string, error) {
	return workload.NamespaceAnnotations(ctx, r.h.Client, namespace)
}

// controllerRef keys the pod-owner cache by a pod's controller ownerRef. The
// UID is in it so a ReplicaSet or Job name reused under a different owner
// within the TTL cannot hit the stale entry.
type controllerRef struct {
	Namespace, Kind, Name string
	UID                   types.UID
}

// workloadRef keys the owner-annotations cache by an already-resolved
// top-level workload. It needs no UID: unlike a ReplicaSet or Job ref, a
// delete-and-recreate cannot reuse it under a different owner.
type workloadRef struct {
	Namespace, Kind, Name string
}

// resolvedOwner is the top-level kind and name one controller ownerRef
// resolves to.
type resolvedOwner struct {
	Kind, Name string
}

// ownerCaches are the admission's two owner-chain Gets, each cached and
// collapsed per key, bounding them on clusters where a selector-less Policy
// covers every pod.
type ownerCaches struct {
	podOwners   *cache.ReadThrough[controllerRef, resolvedOwner]
	annotations *cache.ReadThrough[workloadRef, map[string]string]
}

func (h *Handler) ownerCaches() ownerCaches {
	h.ownersOnce.Do(func() {
		h.owners = ownerCaches{
			podOwners: cache.NewReadThrough(
				cache.NewLRU[controllerRef, resolvedOwner](ownerRefCacheMaxEntries, ownerRefCacheTTL, nil),
				apiCallTimeout, h.fetchPodOwner, reportFetchPanic[controllerRef](panicLabelPodOwner)),
			annotations: cache.NewReadThrough(
				cache.NewLRU[workloadRef, map[string]string](ownerAnnotationsCacheMaxEntries, ownerAnnotationsCacheTTL, nil),
				apiCallTimeout, h.fetchOwnerAnnotations, reportFetchPanic[workloadRef](panicLabelWorkloadAnnotations)),
		}
	})
	return h.owners
}

func (h *Handler) fetchPodOwner(ctx context.Context, ref controllerRef) (resolvedOwner, error) {
	kind, name, err := workload.ResolveControllerOwner(ctx, h.Client, ref.Namespace,
		metav1.OwnerReference{Kind: ref.Kind, Name: ref.Name, UID: ref.UID})
	return resolvedOwner{Kind: kind, Name: name}, err
}

// fetchOwnerAnnotations caches a NotFound owner as nil, since unmanaged
// workloads are the common case.
func (h *Handler) fetchOwnerAnnotations(ctx context.Context, ref workloadRef) (map[string]string, error) {
	raw, err := workload.OwnerAnnotations(ctx, h.Client, ref.Namespace, ref.Kind, ref.Name)
	if err != nil {
		return nil, err
	}
	return cacheableOwnerAnnotations(raw), nil
}

// reportFetchPanic logs and counts an owner fetch's recovered panic under op,
// which must be a bounded constant, never the key.
func reportFetchPanic[K any](op string) func(context.Context, K, any) {
	return func(ctx context.Context, key K, rec any) {
		reportPanic(log.FromContext(ctx), op, rec, "key", key)
	}
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
