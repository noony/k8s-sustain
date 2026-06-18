package controller

import (
	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// buildTolerance resolves a policy's per-resource downsize thresholds into a
// workload.Tolerance, applying defaults (5% / 10m CPU / 15Mi memory) for any
// unset value. Setting both percent and minDecrease to 0 disables suppression
// for that resource (band 0 => every decrease is acted on).
func buildTolerance(rs sustainv1alpha1.ResourcesConfigs) workload.Tolerance {
	cp, cf := resolveDownsizeThreshold(rs.CPU.DownsizeThreshold, workload.DefaultCPUDownsizeFloor)
	mp, mf := resolveDownsizeThreshold(rs.Memory.DownsizeThreshold, workload.DefaultMemoryDownsizeFloor)
	return workload.Tolerance{CPUPercent: cp, CPUFloor: cf, MemPercent: mp, MemFloor: mf}
}

func resolveDownsizeThreshold(t *sustainv1alpha1.DownsizeThreshold, defFloor resource.Quantity) (int32, resource.Quantity) {
	pct := workload.DefaultDownsizePercent
	floor := defFloor
	if t != nil {
		if t.Percent != nil {
			pct = *t.Percent
		}
		if t.MinDecrease != nil {
			floor = *t.MinDecrease
		}
	}
	return pct, floor
}
