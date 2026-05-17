package workload

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
