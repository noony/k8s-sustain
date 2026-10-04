package controller

import (
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// workloadTarget is the unit the apply step works on: one governed member of
// an identity, or every governed pod of a bare-pod identity together.
type workloadTarget struct {
	Kind           string
	Name           string
	Namespace      string
	Containers     []corev1.Container
	InitContainers []corev1.Container
	Selector       *metav1.LabelSelector
	Object         client.Object
	// IdentityKind and IdentityName are the identity this target reports
	// into: what Prometheus, the WorkloadRecommendation and every health
	// series are keyed by. They differ from Kind/Name under an owner-name
	// override; Kind/Name stay the real object, used for key(), recycling
	// and event attribution.
	IdentityKind string
	IdentityName string
	// UpdateMode is the Policy's mode for the kind. OnCreate targets get
	// recommendations and WLR cache writes but are never recycled or
	// resized: the webhook applies resources at pod admission.
	UpdateMode sustainv1alpha1.UpdateMode
	// BarePodMembers is every governed pod of a bare-pod identity, set only
	// for Kind == "Pod". Bare pods cannot be found from a label selector at
	// apply time: membership is the inventory's rule, not a selector.
	BarePodMembers []*corev1.Pod
}

// key returns a unique identifier for this workload target, used as the retry map key.
func (w *workloadTarget) key() string {
	return w.Kind + "/" + w.Namespace + "/" + w.Name
}

// identity is the identity this target reports into: what Prometheus, the
// WorkloadRecommendation and every health series are keyed by.
func (w *workloadTarget) identity() promclient.WorkloadIdentity {
	return promclient.WorkloadIdentity{Namespace: w.Namespace, OwnerKind: w.IdentityKind, OwnerName: w.IdentityName}
}

// recommendableContainers returns the containers to feed into the recommendation
// pipeline plus a set of container names that originate from InitContainers.
// When excludeInit is true (or the workload has no init containers), the init
// list is dropped and the returned set is empty.
func (w *workloadTarget) recommendableContainers(excludeInit bool) ([]corev1.Container, map[string]struct{}) {
	return workload.MergeContainersForRecommendation(w.Containers, w.InitContainers, excludeInit)
}

// targetsOf returns the apply targets of an identity policy governs: one per
// governed member, or one for all governed pods of a bare-pod identity, whose
// pods are resized together and attributed to the newest.
func targetsOf(id *inventory.Identity, policyName string, mode sustainv1alpha1.UpdateMode) []*workloadTarget {
	var targets []*workloadTarget
	var pods []*corev1.Pod
	for _, m := range id.Members {
		if m.Policy != policyName {
			continue
		}
		if pod, ok := m.Object.(*corev1.Pod); ok {
			pods = append(pods, pod)
			continue
		}
		targets = append(targets, targetFromMember(id.Key, m, mode))
	}
	if len(pods) == 0 {
		return targets
	}
	newest := slices.MaxFunc(pods, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	return append(targets, &workloadTarget{
		Kind:           id.Key.OwnerKind,
		Name:           id.Key.OwnerName,
		Namespace:      id.Key.Namespace,
		Containers:     id.Containers,
		InitContainers: id.InitContainers,
		Object:         newest,
		IdentityKind:   id.Key.OwnerKind,
		IdentityName:   id.Key.OwnerName,
		UpdateMode:     mode,
		BarePodMembers: pods,
	})
}

func targetFromMember(key promclient.WorkloadIdentity, m inventory.Member, mode sustainv1alpha1.UpdateMode) *workloadTarget {
	t := &workloadTarget{
		Kind:           key.OwnerKind,
		Name:           m.Object.GetName(),
		Namespace:      key.Namespace,
		Containers:     m.Containers,
		InitContainers: m.InitContainers,
		Object:         m.Object,
		IdentityKind:   key.OwnerKind,
		IdentityName:   key.OwnerName,
		UpdateMode:     mode,
	}
	if _, selector, ok := workload.PodTemplateOf(m.Object); ok {
		t.Selector = selector
	}
	return t
}

// sortedTargets returns a copy of targets ordered by key(), so every decision
// taken across an identity's members — which member's autoscaler shapes the
// recommendation — depends on the members' own names rather than the
// listing or goroutine-completion order.
func sortedTargets(targets []*workloadTarget) []*workloadTarget {
	return slices.SortedFunc(slices.Values(targets), func(a, b *workloadTarget) int {
		return strings.Compare(a.key(), b.key())
	})
}
