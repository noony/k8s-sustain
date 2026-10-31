package controller

import (
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/noony/k8s-sustain/internal/inventory"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
	"github.com/noony/k8s-sustain/internal/workload"
)

// workloadTarget is the unit the apply step works on: one governed member of
// an identity, or every governed pod of a bare-pod identity together, held as
// the Member's Pods: bare pods cannot be found from a label selector at apply
// time, membership is the inventory's rule.
type workloadTarget struct {
	workload.Member
	Name, Namespace string
}

// key names the member in the ledger and the health tracker, across passes
// and Policies.
func (w *workloadTarget) key() string {
	return w.Kind + "/" + w.Namespace + "/" + w.Name
}

// targetsOf returns the apply targets of an identity policy governs: one per
// governed member, or one for all governed pods of a bare-pod identity, whose
// pods are resized together and attributed to the newest.
func targetsOf(id *inventory.Identity, policyName string) []*workloadTarget {
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
		targets = append(targets, targetFromMember(id.Key, m))
	}
	if len(pods) == 0 {
		return targets
	}
	newest := slices.MaxFunc(pods, func(a, b *corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	return append(targets, &workloadTarget{
		Member: workload.Member{
			Kind: id.Key.OwnerKind, Object: newest, Pods: pods,
			Containers: id.Containers, InitContainers: id.InitContainers,
		},
		Name: id.Key.OwnerName, Namespace: id.Key.Namespace,
	})
}

func targetFromMember(key promclient.WorkloadIdentity, m inventory.Member) *workloadTarget {
	return &workloadTarget{
		Member: workload.Member{
			Kind: key.OwnerKind, Object: m.Object,
			Containers: m.Containers, InitContainers: m.InitContainers,
		},
		Name: m.Object.GetName(), Namespace: key.Namespace,
	}
}
