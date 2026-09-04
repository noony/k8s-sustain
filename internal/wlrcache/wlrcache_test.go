package wlrcache

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

func TestStatusEquivalent_DistinguishesSameValuesFromDifferentSources(t *testing.T) {
	cpu := resource.MustParse("250m")
	now := metav1.NewTime(time.Now())
	later := metav1.NewTime(now.Add(time.Minute))

	a := sustainv1alpha1.WorkloadRecommendationStatus{
		ObservedAt: now,
		Source:     "prometheus",
		Containers: map[string]sustainv1alpha1.ContainerRecommendation{
			"app": {CPURequest: &cpu},
		},
	}
	b := a
	b.ObservedAt = later
	if !statusEquivalent(a, b) {
		t.Error("differ only by ObservedAt → should be equivalent")
	}

	c := a
	c.Source = "fallback"
	if statusEquivalent(a, c) {
		t.Error("different Source → should NOT be equivalent")
	}
}

func TestContainersFromObserved_SplitsAndSorts(t *testing.T) {
	cpu := resource.MustParse("100m")
	obs := map[string]sustainv1alpha1.ObservedContainerResources{
		"zeta":  {CPURequest: &cpu},
		"alpha": {},
		"init":  {Init: true, MemoryLimit: &cpu},
	}
	containers, initContainers := ContainersFromObserved(obs)
	if len(containers) != 2 || containers[0].Name != "alpha" || containers[1].Name != "zeta" {
		t.Fatalf("containers = %v, want [alpha zeta]", containers)
	}
	if got := containers[1].Resources.Requests.Cpu(); got.Cmp(cpu) != 0 {
		t.Errorf("zeta cpu request = %v, want %v", got, cpu)
	}
	if containers[0].Resources.Requests != nil || containers[0].Resources.Limits != nil {
		t.Errorf("alpha should carry no ResourceList, got %+v", containers[0].Resources)
	}
	if len(initContainers) != 1 || initContainers[0].Name != "init" {
		t.Fatalf("initContainers = %v, want [init]", initContainers)
	}
	if got := initContainers[0].Resources.Limits.Memory(); got.Cmp(cpu) != 0 {
		t.Errorf("init memory limit = %v, want %v", got, cpu)
	}
}

func TestRecsFromStatus_RoundTripsBuildStatus(t *testing.T) {
	cpu := resource.MustParse("250m")
	mem := resource.MustParse("64Mi")
	in := map[string]workload.ContainerRecommendation{
		"app": {CPURequest: &cpu, MemoryRequest: &mem, MemoryLimit: &mem, RemoveCPULimit: true},
	}
	got := RecsFromStatus(buildStatus(in, nil, metav1.Now()))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %+v, want %+v", got, in)
	}
	if RecsFromStatus(sustainv1alpha1.WorkloadRecommendationStatus{}) != nil {
		t.Error("empty status should yield nil")
	}
}
