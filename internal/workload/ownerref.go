package workload

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "ReplicaSet":
			var rs appsv1.ReplicaSet
			if err := c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &rs); err != nil {
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
			if err := c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &job); err != nil {
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
	return "", "", nil
}
