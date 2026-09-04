package workload

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OwnerAnnotations reads a workload object's own metadata.annotations, the
// opt-in level a pod does not inherit. An unknown kind and a missing object
// both resolve to nil rather than an error: neither is a failure, the workload
// simply has no annotations at that level. Any other read error is returned so
// an RBAC gap or apiserver outage cannot silently un-manage the workload.
func OwnerAnnotations(ctx context.Context, c client.Client, namespace, kind, name string) (map[string]string, error) {
	obj := ObjectForKind(kind)
	if obj == nil {
		return nil, nil
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s %s/%s: %w", kind, namespace, name, err)
	}
	return obj.GetAnnotations(), nil
}

// NamespaceAnnotations reads a Namespace's metadata.annotations, the least
// specific opt-in level. A missing Namespace resolves to nil: namespaces are
// deleted while reconciles and admissions are in flight, and that race is not
// worth failing the caller over.
func NamespaceAnnotations(ctx context.Context, c client.Client, name string) (map[string]string, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading namespace %s: %w", name, err)
	}
	return ns.Annotations, nil
}
