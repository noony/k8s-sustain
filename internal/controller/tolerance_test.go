package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestBuildTolerance_Defaults(t *testing.T) {
	tol := buildTolerance(sustainv1alpha1.ResourcesConfigs{})
	if tol.CPUPercent != 5 || tol.MemPercent != 5 {
		t.Fatalf("want 5%% defaults, got cpu=%d mem=%d", tol.CPUPercent, tol.MemPercent)
	}
	if tol.CPUFloor.Cmp(resource.MustParse("10m")) != 0 {
		t.Fatalf("want 10m cpu floor, got %s", tol.CPUFloor.String())
	}
	if tol.MemFloor.Cmp(resource.MustParse("15Mi")) != 0 {
		t.Fatalf("want 15Mi mem floor, got %s", tol.MemFloor.String())
	}
}

func TestBuildTolerance_Overrides(t *testing.T) {
	p := int32(20)
	md := resource.MustParse("50m")
	tol := buildTolerance(sustainv1alpha1.ResourcesConfigs{
		CPU: sustainv1alpha1.ResourceConfig{
			DownsizeThreshold: &sustainv1alpha1.DownsizeThreshold{Percent: &p, MinDecrease: &md},
		},
	})
	if tol.CPUPercent != 20 || tol.CPUFloor.Cmp(resource.MustParse("50m")) != 0 {
		t.Fatalf("override not applied: %+v", tol)
	}
	if tol.MemPercent != 5 || tol.MemFloor.Cmp(resource.MustParse("15Mi")) != 0 {
		t.Fatalf("memory defaults clobbered: %+v", tol)
	}
}

func TestBuildTolerance_PartialOverridePercentOnly(t *testing.T) {
	p := int32(30)
	tol := buildTolerance(sustainv1alpha1.ResourcesConfigs{
		CPU: sustainv1alpha1.ResourceConfig{
			DownsizeThreshold: &sustainv1alpha1.DownsizeThreshold{Percent: &p}, // MinDecrease nil
		},
	})
	if tol.CPUPercent != 30 {
		t.Fatalf("want custom percent 30, got %d", tol.CPUPercent)
	}
	if tol.CPUFloor.Cmp(resource.MustParse("10m")) != 0 {
		t.Fatalf("want default 10m floor when MinDecrease nil, got %s", tol.CPUFloor.String())
	}
}

func TestBuildTolerance_PartialOverrideMinDecreaseOnly(t *testing.T) {
	md := resource.MustParse("40Mi")
	tol := buildTolerance(sustainv1alpha1.ResourcesConfigs{
		Memory: sustainv1alpha1.ResourceConfig{
			DownsizeThreshold: &sustainv1alpha1.DownsizeThreshold{MinDecrease: &md}, // Percent nil
		},
	})
	if tol.MemPercent != 5 {
		t.Fatalf("want default percent 5 when Percent nil, got %d", tol.MemPercent)
	}
	if tol.MemFloor.Cmp(resource.MustParse("40Mi")) != 0 {
		t.Fatalf("want custom 40Mi floor, got %s", tol.MemFloor.String())
	}
}

func TestBuildTolerance_Disable(t *testing.T) {
	zero := int32(0)
	zeroQ := resource.MustParse("0")
	tol := buildTolerance(sustainv1alpha1.ResourcesConfigs{
		CPU:    sustainv1alpha1.ResourceConfig{DownsizeThreshold: &sustainv1alpha1.DownsizeThreshold{Percent: &zero, MinDecrease: &zeroQ}},
		Memory: sustainv1alpha1.ResourceConfig{DownsizeThreshold: &sustainv1alpha1.DownsizeThreshold{Percent: &zero, MinDecrease: &zeroQ}},
	})
	if tol.CPUPercent != 0 || tol.CPUFloor.MilliValue() != 0 || tol.MemPercent != 0 || tol.MemFloor.MilliValue() != 0 {
		t.Fatalf("disable not honoured: %+v", tol)
	}
}
