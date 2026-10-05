package wlrcache

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestStatusEquivalent_DistinguishesSameValuesFromDifferentOutcomes(t *testing.T) {
	cpu := resource.MustParse("250m")
	now := metav1.NewTime(time.Now())
	later := metav1.NewTime(now.Add(time.Minute))

	a := sustainv1alpha1.WorkloadRecommendationStatus{
		ObservedAt: now,
		Outcome:    sustainv1alpha1.OutcomeComputed,
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
	c.Outcome = sustainv1alpha1.OutcomeNoData
	if statusEquivalent(a, c) {
		t.Error("different Outcome → should NOT be equivalent")
	}

	percentile := resource.MustParse("249900u")
	d := a
	d.Trace = map[string]sustainv1alpha1.ContainerTrace{"app": {CPU: &sustainv1alpha1.ResourceTrace{
		Percentile: &percentile, WithHeadroom: cpu, Clamped: cpu,
	}}}
	if !statusEquivalent(a, d) {
		t.Error("differ only by Trace → should be equivalent")
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
