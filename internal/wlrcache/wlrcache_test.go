package wlrcache

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
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
