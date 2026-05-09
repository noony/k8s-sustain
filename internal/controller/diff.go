package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/noony/k8s-sustain/internal/workload"
)

// changedContainers returns the names of containers whose recommended CPU/memory
// requests or limits differ from the current spec. A nil/zero quantity in either
// side is treated as "unset" and matches another unset value.
func changedContainers(current []corev1.Container, recs map[string]workload.ContainerRecommendation) []string {
	byName := make(map[string]corev1.Container, len(current))
	for _, c := range current {
		byName[c.Name] = c
	}
	var changed []string
	for name, rec := range recs {
		c, ok := byName[name]
		if !ok {
			changed = append(changed, name)
			continue
		}
		if !requestEqual(rec.CPURequest, c.Resources.Requests.Cpu()) ||
			!requestEqual(rec.MemoryRequest, c.Resources.Requests.Memory()) ||
			!limitEqual(rec.CPULimit, rec.RemoveCPULimit, c.Resources.Limits.Cpu()) ||
			!limitEqual(rec.MemoryLimit, rec.RemoveMemoryLimit, c.Resources.Limits.Memory()) {
			changed = append(changed, name)
		}
	}
	return changed
}

// requestEqual reports whether the recommendation matches the current request,
// treating a nil recommendation as "leave it alone" (i.e. unchanged) since the
// patcher takes no action in that case.
func requestEqual(rec *resource.Quantity, current *resource.Quantity) bool {
	if rec == nil {
		return true
	}
	return quantityEqual(rec, current)
}

func quantityEqual(a *resource.Quantity, b *resource.Quantity) bool {
	aZero := a == nil || a.IsZero()
	bZero := b == nil || b.IsZero()
	if aZero && bZero {
		return true
	}
	if aZero != bZero {
		return false
	}
	return a.Cmp(*b) == 0
}

// limitEqual reports whether the limit recommendation matches the current
// limit. A nil rec without remove=true means "leave it alone" → unchanged.
func limitEqual(rec *resource.Quantity, remove bool, current *resource.Quantity) bool {
	currentZero := current == nil || current.IsZero()
	if remove {
		return currentZero
	}
	if rec == nil {
		return true
	}
	return quantityEqual(rec, current)
}
