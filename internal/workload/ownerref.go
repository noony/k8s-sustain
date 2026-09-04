package workload

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// IsOwnedBy reports whether refs contains a controller ownerReference with
// the given UID. Used to confirm "this Job is owned by that CronJob" etc.
func IsOwnedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.UID == uid {
			return true
		}
	}
	return false
}

// IsOwnedByKind reports whether refs contains a controller ownerReference of
// the given Kind. Useful when the UID isn't available (e.g. checking that a
// Job has any CronJob parent, or a Pod has a StatefulSet parent).
func IsOwnedByKind(refs []metav1.OwnerReference, kind string) bool {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller && ref.Kind == kind {
			return true
		}
	}
	return false
}

// ResolvePodOwner walks a pod's controller ownerReferences to the top-level
// workload kind+name. Handles three indirect chains via one API call each:
//   - Pod → ReplicaSet → Deployment
//   - Pod → ReplicaSet → Rollout (Argo)
//   - Pod → Job → CronJob
//
// For unknown owner kinds (StatefulSet, DaemonSet, custom controllers) the
// immediate controller ref is returned as terminal. Returns ("", "", nil) for
// orphan pods.
func ResolvePodOwner(ctx context.Context, c client.Client, pod *corev1.Pod) (kind, name string, err error) {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return "", "", nil
	}
	return ResolveControllerOwner(ctx, c, pod.Namespace, *ref)
}

// ResolveControllerOwner resolves a single controller ownerReference to the
// top-level workload kind+name, performing at most one apiserver Get (for
// ReplicaSet or Job); every other kind is already terminal.
//
// Split out of ResolvePodOwner so the webhook can cache the walk keyed by this
// one ref — the identity repeated across every pod behind a ReplicaSet or Job
// during a rolling restart — without duplicating the switch below.
func ResolveControllerOwner(ctx context.Context, c client.Client, namespace string, ref metav1.OwnerReference) (kind, name string, err error) {
	switch ref.Kind {
	case "ReplicaSet":
		var rs appsv1.ReplicaSet
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &rs); err != nil {
			return "", "", fmt.Errorf("getting replicaset %s: %w", ref.Name, err)
		}
		for _, rsRef := range rs.OwnerReferences {
			if rsRef.Controller == nil || !*rsRef.Controller {
				continue
			}
			switch rsRef.Kind {
			case "Deployment", "Rollout":
				return rsRef.Kind, rsRef.Name, nil
			}
		}
		return "ReplicaSet", ref.Name, nil
	case "Job":
		var job batchv1.Job
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &job); err != nil {
			return "", "", fmt.Errorf("getting job %s: %w", ref.Name, err)
		}
		for _, jobRef := range job.OwnerReferences {
			if jobRef.Controller != nil && *jobRef.Controller && jobRef.Kind == "CronJob" {
				return "CronJob", jobRef.Name, nil
			}
		}
		return "Job", ref.Name, nil
	default:
		return ref.Kind, ref.Name, nil
	}
}

// PodOwnedByWorkload reports whether the pod's controller ownerRef chain
// resolves to the workload with the given UID. StatefulSet/DaemonSet/Job
// pods carry a direct controller ref to the workload; Deployment and Rollout
// pods are owned via an intermediate ReplicaSet, which is fetched and
// checked against the workload UID. rsOwned memoizes ReplicaSet lookups by
// name within one pass so N pods of the same ReplicaSet cost one GET, not N.
//
// A pod with no controller ownerRef (bare debug pod) is never owned. A
// ReplicaSet that no longer exists (GC race) cannot be verified, so its pods
// are treated as unowned for this pass.
func PodOwnedByWorkload(ctx context.Context, c client.Client, pod *corev1.Pod, uid types.UID, rsOwned map[string]bool) (bool, error) {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return false, nil
	}
	if ref.UID == uid {
		return true, nil
	}
	if ref.Kind != "ReplicaSet" {
		return false, nil
	}
	if owned, ok := rsOwned[ref.Name]; ok {
		return owned, nil
	}
	var rs appsv1.ReplicaSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &rs); err != nil {
		if apierrors.IsNotFound(err) {
			rsOwned[ref.Name] = false
			return false, nil
		}
		return false, fmt.Errorf("getting replicaset %s: %w", ref.Name, err)
	}
	owned := IsOwnedBy(rs.OwnerReferences, uid)
	rsOwned[ref.Name] = owned
	return owned, nil
}

// ApplyOwnerNameOverride applies the k8s.sustain.io/owner-name annotation
// override to a resolved (kind, name) pair. The annotation doubles as a
// Kubernetes label value once the webhook mirrors it onto the pod (see
// internal/webhook), so it is validated against label-value rules
// (RFC 1123, <=63 chars) — stricter than PolicyAnnotation's DNS-1123-subdomain
// check. An absent, empty, or invalid annotation leaves kind and name
// unchanged. Empty is treated the same as absent because the Prometheus
// recording rules (kube_pod_labels{label_k8s_sustain_io_owner_name!=""})
// never produce an override row for an empty label value — Go and
// Prometheus must agree on whether the override "applies".
//
// When valid: name becomes the annotation value. kind becomes the input kind
// when non-empty (an owned workload grouping itself under a shared identity)
// or "Pod" when empty (no real controller owner — a bare pod).
func ApplyOwnerNameOverride(kind, name string, annotations map[string]string) (string, string) {
	v, ok := annotations[sustainv1alpha1.OwnerNameAnnotation]
	if !ok || v == "" || len(apivalidation.IsValidLabelValue(v)) != 0 {
		return kind, name
	}
	if kind == "" {
		return "Pod", v
	}
	return kind, v
}
