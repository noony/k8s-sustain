package controller

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
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
	Object         client.Object
	// UpdateMode is the Policy's mode for the kind. OnCreate targets get
	// recommendations and WLR cache writes but are never recycled or
	// resized: the webhook applies resources at pod admission.
	UpdateMode sustainv1alpha1.UpdateMode
	// BarePodMembers is every governed pod of a bare-pod identity, set only
	// for Kind == "Pod". Bare pods cannot be found from a label selector at
	// apply time: membership is the inventory's rule, not a selector.
	BarePodMembers []*corev1.Pod
}

// key names the member in the ledger and the health tracker, across passes
// and Policies.
func (w *workloadTarget) key() string {
	return w.Kind + "/" + w.Namespace + "/" + w.Name
}

// member is what the apply step drives toward the Recommendation.
func (w *workloadTarget) member() workload.Member {
	return workload.Member{Kind: w.Kind, Object: w.Object, Pods: w.BarePodMembers}
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
		UpdateMode:     mode,
		BarePodMembers: pods,
	})
}

func targetFromMember(key promclient.WorkloadIdentity, m inventory.Member, mode sustainv1alpha1.UpdateMode) *workloadTarget {
	return &workloadTarget{
		Kind:           key.OwnerKind,
		Name:           m.Object.GetName(),
		Namespace:      key.Namespace,
		Containers:     m.Containers,
		InitContainers: m.InitContainers,
		Object:         m.Object,
		UpdateMode:     mode,
	}
}
