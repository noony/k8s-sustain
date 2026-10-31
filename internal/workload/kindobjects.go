package workload

import (
	"sort"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// kindSpec is everything the codebase needs to know about one workload kind
// to Get it, List it, or name it in an error.
type kindSpec struct {
	newObject     func() client.Object
	newList       func() client.ObjectList
	groupResource schema.GroupResource
}

// ownerKinds maps each workload kind to its constructors. A table rather than
// a switch so hack/kindcontract can enumerate it via OwnerChainKinds and check
// every other place a kind is registered in against it.
var ownerKinds = map[string]kindSpec{
	"Deployment": {
		newObject:     func() client.Object { return &appsv1.Deployment{} },
		newList:       func() client.ObjectList { return &appsv1.DeploymentList{} },
		groupResource: schema.GroupResource{Group: "apps", Resource: "deployments"},
	},
	"StatefulSet": {
		newObject:     func() client.Object { return &appsv1.StatefulSet{} },
		newList:       func() client.ObjectList { return &appsv1.StatefulSetList{} },
		groupResource: schema.GroupResource{Group: "apps", Resource: "statefulsets"},
	},
	"DaemonSet": {
		newObject:     func() client.Object { return &appsv1.DaemonSet{} },
		newList:       func() client.ObjectList { return &appsv1.DaemonSetList{} },
		groupResource: schema.GroupResource{Group: "apps", Resource: "daemonsets"},
	},
	"ReplicaSet": {
		newObject:     func() client.Object { return &appsv1.ReplicaSet{} },
		newList:       func() client.ObjectList { return &appsv1.ReplicaSetList{} },
		groupResource: schema.GroupResource{Group: "apps", Resource: "replicasets"},
	},
	"CronJob": {
		newObject:     func() client.Object { return &batchv1.CronJob{} },
		newList:       func() client.ObjectList { return &batchv1.CronJobList{} },
		groupResource: schema.GroupResource{Group: "batch", Resource: "cronjobs"},
	},
	"Job": {
		newObject:     func() client.Object { return &batchv1.Job{} },
		newList:       func() client.ObjectList { return &batchv1.JobList{} },
		groupResource: schema.GroupResource{Group: "batch", Resource: "jobs"},
	},
	"Rollout": {
		newObject:     func() client.Object { return &rolloutsv1alpha1.Rollout{} },
		newList:       func() client.ObjectList { return &rolloutsv1alpha1.RolloutList{} },
		groupResource: schema.GroupResource{Group: "argoproj.io", Resource: "rollouts"},
	},
}

// SupportedKinds is the canonical ordering of the kinds a Policy can target,
// the one kind table hack/kindcontract checks every other place against.
// "Pod" means bare-pod identities formed via the owner-name annotation; it is
// deliberately absent from ownerKinds, since a bare pod has no separate owner
// object to read.
var SupportedKinds = []string{"Deployment", "StatefulSet", "DaemonSet", "Rollout", "CronJob", "Job", "Pod"}

// Kind is one of SupportedKinds as the dashboard describes it.
type Kind struct {
	Name string
	// UpdateField is the Policy's spec.rightSizing.update.types field that
	// manages the kind.
	UpdateField string
	// InPlaceOnly kinds never have a pod evicted.
	InPlaceOnly bool
}

// Kinds describes SupportedKinds, in their order.
func Kinds() []Kind {
	out := make([]Kind, 0, len(SupportedKinds))
	for _, name := range SupportedKinds {
		out = append(out, Kind{
			Name:        name,
			UpdateField: sustainv1alpha1.UpdateTypesField(name),
			InPlaceOnly: applyRules[name].inPlaceOnly,
		})
	}
	return out
}

// ObjectForKind returns a fresh, empty client.Object for a workload kind, or
// nil when the kind is unknown — an unknown kind (a custom controller) is not
// an error, it simply has no workload-level annotations to read.
func ObjectForKind(kind string) client.Object {
	spec, ok := ownerKinds[kind]
	if !ok {
		return nil
	}
	return spec.newObject()
}

// ListForKind returns a fresh, empty client.ObjectList for a workload kind, or
// nil when the kind is unknown.
func ListForKind(kind string) client.ObjectList {
	spec, ok := ownerKinds[kind]
	if !ok {
		return nil
	}
	return spec.newList()
}

// GroupResourceForKind returns the GroupResource a kind is served under, for
// building NotFound errors. Unknown kinds (including bare-pod identities)
// resolve to core pods.
func GroupResourceForKind(kind string) schema.GroupResource {
	if spec, ok := ownerKinds[kind]; ok {
		return spec.groupResource
	}
	return schema.GroupResource{Resource: "pods"}
}

// OwnerChainKinds returns every kind ObjectForKind knows, for callers that
// must cross-check their own coverage against it. Sorted so its output is
// stable — map iteration order would otherwise vary run to run.
func OwnerChainKinds() []string {
	kinds := make([]string, 0, len(ownerKinds))
	for kind := range ownerKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}
