package workload

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
)

func TestObjectForKind(t *testing.T) {
	if _, ok := ObjectForKind("Deployment").(*appsv1.Deployment); !ok {
		t.Errorf("ObjectForKind(Deployment) = %T, want *appsv1.Deployment", ObjectForKind("Deployment"))
	}
	if _, ok := ObjectForKind("CronJob").(*batchv1.CronJob); !ok {
		t.Errorf("ObjectForKind(CronJob) = %T, want *batchv1.CronJob", ObjectForKind("CronJob"))
	}
	if got := ObjectForKind("SomeCustomKind"); got != nil {
		t.Errorf("ObjectForKind(unknown) = %T, want nil — an unknown kind is not an error, it just has no workload level to read", got)
	}
}

// Each call must return a FRESH object. Returning a shared value would let one
// Get's result leak into the next caller's read.
func TestObjectForKind_ReturnsFreshObject(t *testing.T) {
	a := ObjectForKind("Deployment")
	b := ObjectForKind("Deployment")
	if a == b {
		t.Fatal("ObjectForKind returned the same pointer twice; each call must allocate")
	}
	a.SetName("mutated")
	if b.GetName() != "" {
		t.Errorf("mutating one result affected another: got name %q", b.GetName())
	}
}

func TestOwnerChainKinds_CoversEveryObjectForKind(t *testing.T) {
	for _, kind := range OwnerChainKinds() {
		if ObjectForKind(kind) == nil {
			t.Errorf("OwnerChainKinds lists %q but ObjectForKind returns nil for it", kind)
		}
	}
}
