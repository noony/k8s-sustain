package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nsAnnotations memoises Namespace annotation lookups for the span of one
// target-collection pass: opt-in is resolved per workload, so a namespace with
// N Deployments would otherwise issue N identical reads per reconcile, per
// policy.
//
// Not safe for concurrent use — it is built and consumed inside a single
// collectTargets call, before the errgroup fan-out that processes targets.
type nsAnnotations struct {
	client client.Client
	cache  map[string]map[string]string
}

func newNSAnnotations(c client.Client) *nsAnnotations {
	return &nsAnnotations{client: c, cache: map[string]map[string]string{}}
}

// get returns the Namespace's annotations, or nil when the Namespace does not
// exist. A missing namespace is deliberately NOT an error: namespaces are
// deleted while reconciles are in flight, and failing the pass would stall
// every other workload the policy governs. Any other read failure IS returned,
// so a broken read (RBAC, apiserver outage) surfaces as a requeue rather than
// silently un-managing every namespace-opted-in workload.
func (n *nsAnnotations) get(ctx context.Context, namespace string) (map[string]string, error) {
	if a, ok := n.cache[namespace]; ok {
		return a, nil
	}
	var ns corev1.Namespace
	if err := n.client.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			n.cache[namespace] = nil
			return nil, nil
		}
		return nil, fmt.Errorf("reading namespace %s: %w", namespace, err)
	}
	n.cache[namespace] = ns.Annotations
	return ns.Annotations, nil
}

// forPods returns the annotations of every distinct namespace the pods live
// in, shaped for workload.GroupBarePods. Bare-pod grouping needs the whole map
// up front because it filters pods by resolved policy as it groups them.
func (n *nsAnnotations) forPods(ctx context.Context, pods []corev1.Pod) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for i := range pods {
		ns := pods[i].Namespace
		if _, ok := out[ns]; ok {
			continue
		}
		a, err := n.get(ctx, ns)
		if err != nil {
			return nil, err
		}
		out[ns] = a
	}
	return out, nil
}
