package workload

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// applyIsNoOp reports whether applying rec to a container with the given
// current resources leaves the resulting CPU/memory requests and limits
// unchanged (compared numerically). This is the ground truth ContainerMatches
// must agree with: "matches" iff applying the recommendation changes no value.
func applyIsNoOp(current corev1.ResourceRequirements, rec ContainerRecommendation) bool {
	c := corev1.Container{Resources: *current.DeepCopy()}
	applyRecToContainer(&c, rec)
	return resourcesValueEqual(current, c.Resources)
}

// resourcesValueEqual compares requests+limits for CPU and memory numerically,
// treating an absent key as a zero Quantity (the same normalization the
// Kubernetes ResourceList accessors use).
func resourcesValueEqual(a, b corev1.ResourceRequirements) bool {
	return a.Requests.Cpu().Cmp(*b.Requests.Cpu()) == 0 &&
		a.Requests.Memory().Cmp(*b.Requests.Memory()) == 0 &&
		a.Limits.Cpu().Cmp(*b.Limits.Cpu()) == 0 &&
		a.Limits.Memory().Cmp(*b.Limits.Memory()) == 0
}

func reqs(cpu, mem string) corev1.ResourceList {
	rl := corev1.ResourceList{}
	if cpu != "" {
		rl[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		rl[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return rl
}

func TestContainerMatches(t *testing.T) {
	cases := []struct {
		name    string
		current corev1.ResourceRequirements
		rec     ContainerRecommendation
		want    bool
	}{
		{
			name:    "exact match cpu+mem requests",
			current: corev1.ResourceRequirements{Requests: reqs("100m", "64Mi")},
			rec:     ContainerRecommendation{CPURequest: qtyp("100m"), MemoryRequest: qtyp("64Mi")},
			want:    true,
		},
		{
			name:    "empty rec is no-op match",
			current: corev1.ResourceRequirements{Requests: reqs("100m", "64Mi")},
			rec:     ContainerRecommendation{},
			want:    true,
		},
		{
			name:    "off by one milli cpu request",
			current: corev1.ResourceRequirements{Requests: reqs("100m", "")},
			rec:     ContainerRecommendation{CPURequest: qtyp("101m")},
			want:    false,
		},
		{
			// Unset request vs explicit zero rec: unset reads as zero, so applying
			// a zero request is a no-op → match.
			name:    "unset request vs zero rec matches",
			current: corev1.ResourceRequirements{},
			rec:     ContainerRecommendation{CPURequest: qtyp("0")},
			want:    true,
		},
		{
			name:    "unset request vs nonzero rec differs",
			current: corev1.ResourceRequirements{},
			rec:     ContainerRecommendation{CPURequest: qtyp("100m")},
			want:    false,
		},
		{
			// Unset limit vs explicit zero limit rec: both numerically zero → match.
			name:    "unset limit vs zero rec matches",
			current: corev1.ResourceRequirements{},
			rec:     ContainerRecommendation{CPULimit: qtyp("0")},
			want:    true,
		},
		{
			name:    "unset limit vs nonzero rec differs",
			current: corev1.ResourceRequirements{},
			rec:     ContainerRecommendation{CPULimit: qtyp("500m")},
			want:    false,
		},
		{
			name:    "limit matches",
			current: corev1.ResourceRequirements{Limits: reqs("500m", "")},
			rec:     ContainerRecommendation{CPULimit: qtyp("500m")},
			want:    true,
		},
		{
			name:    "limit drift",
			current: corev1.ResourceRequirements{Limits: reqs("", "512Mi")},
			rec:     ContainerRecommendation{MemoryLimit: qtyp("1Gi")},
			want:    false,
		},
		{
			// RemoveCPULimit with a limit still present → applying deletes it → mismatch.
			name:    "remove limit when present differs",
			current: corev1.ResourceRequirements{Limits: reqs("500m", "")},
			rec:     ContainerRecommendation{RemoveCPULimit: true},
			want:    false,
		},
		{
			// RemoveCPULimit with no limit present → no-op → match.
			name:    "remove limit when absent matches",
			current: corev1.ResourceRequirements{},
			rec:     ContainerRecommendation{RemoveCPULimit: true},
			want:    true,
		},
		{
			// RemoveCPULimit with an explicit zero limit present → deleting a
			// zero-valued key leaves the value at zero → no-op → match. This is
			// the exact unset-vs-explicit-zero boundary the consolidation fixes.
			name:    "remove limit when present-but-zero matches",
			current: corev1.ResourceRequirements{Limits: reqs("0", "")},
			rec:     ContainerRecommendation{RemoveCPULimit: true},
			want:    true,
		},
		{
			// CPU differs, memory matches: should be a mismatch (CPU-only drift).
			name:    "cpu-only drift",
			current: corev1.ResourceRequirements{Requests: reqs("100m", "64Mi")},
			rec:     ContainerRecommendation{CPURequest: qtyp("200m"), MemoryRequest: qtyp("64Mi")},
			want:    false,
		},
		{
			// Memory differs, CPU matches: mismatch (memory-only drift).
			name:    "memory-only drift",
			current: corev1.ResourceRequirements{Requests: reqs("100m", "64Mi")},
			rec:     ContainerRecommendation{CPURequest: qtyp("100m"), MemoryRequest: qtyp("128Mi")},
			want:    false,
		},
		{
			// Equal quantities written differently (1000m == 1) still match.
			name:    "equivalent forms match",
			current: corev1.ResourceRequirements{Requests: reqs("1000m", "")},
			rec:     ContainerRecommendation{CPURequest: qtyp("1")},
			want:    true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ContainerMatches(c.current, c.rec)
			if got != c.want {
				t.Errorf("ContainerMatches = %v, want %v", got, c.want)
			}
			// The predicate must agree with whether applying the rec is a no-op.
			if noop := applyIsNoOp(c.current, c.rec); got != noop {
				t.Errorf("ContainerMatches = %v but applying rec no-op = %v; predicate must equal apply-is-no-op", got, noop)
			}
		})
	}
}

// TestChangedContainers-style coverage for a missing container is exercised in
// the controller package; ContainerMatches itself only sees a container that
// exists, so "missing container" is handled by the caller mapping. We assert
// here that a container with a recommendation it does not satisfy is reported
// as a mismatch, mirroring the missing-vs-present distinction.
func TestContainerMatches_MissingResourceMapEntries(t *testing.T) {
	// Current has neither requests nor limits set at all (nil maps).
	current := corev1.ResourceRequirements{}
	// A rec that wants concrete values must report a mismatch.
	rec := ContainerRecommendation{CPURequest: qtyp("100m"), MemoryLimit: qtyp("256Mi")}
	if ContainerMatches(current, rec) {
		t.Error("expected mismatch: nil resource maps vs concrete recommendation")
	}
	if applyIsNoOp(current, rec) {
		t.Error("sanity: applying a concrete rec to empty resources must change something")
	}
}
