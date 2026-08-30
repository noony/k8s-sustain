package workload

import (
	"sort"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ownerKindObjects maps every workload kind ObjectForKind knows how to
// construct to a constructor for the client.Object it fetches. Kept as a
// table rather than inlined in a switch so callers that need to read a
// workload owner's own metadata.annotations — the webhook's opt-in resolution
// and the OOM watcher's Reconcile alike — can enumerate it via
// OwnerChainKinds. TestDisableForCoversOwnerAnnotationKinds
// (internal/webhook/optin_test.go) cross-checks that enumeration against
// k8s.OwnerChainDisableFor: every kind read this way must be there, or its
// first Get stands up a cluster-wide informer on a hot path instead of
// costing one Get (see NewCached's doc comment for why that is a silent,
// self-healing failure that is very hard to diagnose after the fact).
var ownerKindObjects = map[string]func() client.Object{
	"Deployment":  func() client.Object { return &appsv1.Deployment{} },
	"StatefulSet": func() client.Object { return &appsv1.StatefulSet{} },
	"DaemonSet":   func() client.Object { return &appsv1.DaemonSet{} },
	"ReplicaSet":  func() client.Object { return &appsv1.ReplicaSet{} },
	"CronJob":     func() client.Object { return &batchv1.CronJob{} },
	"Job":         func() client.Object { return &batchv1.Job{} },
	"Rollout":     func() client.Object { return &rolloutsv1alpha1.Rollout{} },
}

// ObjectForKind returns a fresh, empty client.Object for a workload kind, or
// nil when the kind is unknown — an unknown kind (a custom controller) is not
// an error, it simply has no workload-level annotations to read.
func ObjectForKind(kind string) client.Object {
	newObj, ok := ownerKindObjects[kind]
	if !ok {
		return nil
	}
	return newObj()
}

// OwnerChainKinds returns every kind ObjectForKind knows, for callers that
// must cross-check their own coverage against it. Sorted so its output is
// stable — map iteration order would otherwise vary run to run.
func OwnerChainKinds() []string {
	kinds := make([]string, 0, len(ownerKindObjects))
	for kind := range ownerKindObjects {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}
