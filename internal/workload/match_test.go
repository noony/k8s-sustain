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

// reqsRR builds a ResourceRequirements from request/limit strings; empty string
// means "leave that key unset".
func reqsRR(cpuReq, memReq, cpuLim, memLim string) corev1.ResourceRequirements {
	r := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	if cpuReq != "" {
		r.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	}
	if memReq != "" {
		r.Requests[corev1.ResourceMemory] = resource.MustParse(memReq)
	}
	if cpuLim != "" {
		r.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
	}
	if memLim != "" {
		r.Limits[corev1.ResourceMemory] = resource.MustParse(memLim)
	}
	return r
}

// tol5 is the default-shaped tolerance: 5% with 10m CPU / 15Mi memory floors.
var tol5 = Tolerance{CPUPercent: 5, CPUFloor: resource.MustParse("10m"), MemPercent: 5, MemFloor: resource.MustParse("15Mi")}

func assertQtyEq(t *testing.T, field string, want, got *resource.Quantity) {
	t.Helper()
	switch {
	case want == nil && got == nil:
	case want == nil && got != nil:
		t.Fatalf("%s: want nil (cleared), got %s", field, got.String())
	case want != nil && got == nil:
		t.Fatalf("%s: want %s, got nil", field, want.String())
	case want.Cmp(*got) != 0:
		t.Fatalf("%s: want %s, got %s", field, want.String(), got.String())
	}
}

func TestClampDecreaseToTolerance(t *testing.T) {
	tests := []struct {
		name       string
		current    corev1.ResourceRequirements
		rec        ContainerRecommendation
		tol        Tolerance
		wantCPUReq *resource.Quantity // nil => expect cleared
		wantMemReq *resource.Quantity
	}{
		{name: "cpu increase always kept", current: reqsRR("100m", "", "", ""), rec: ContainerRecommendation{CPURequest: qtyp("110m")}, tol: tol5, wantCPUReq: qtyp("110m")},
		{name: "cpu small decrease below percent and floor cleared", current: reqsRR("1000m", "", "", ""), rec: ContainerRecommendation{CPURequest: qtyp("995m")}, tol: tol5, wantCPUReq: nil},
		{name: "cpu decrease above percent kept", current: reqsRR("1000m", "", "", ""), rec: ContainerRecommendation{CPURequest: qtyp("900m")}, tol: tol5, wantCPUReq: qtyp("900m")},
		{name: "cpu tiny absolute decrease below floor cleared", current: reqsRR("4m", "", "", ""), rec: ContainerRecommendation{CPURequest: qtyp("2m")}, tol: tol5, wantCPUReq: nil},
		{name: "memory decrease below 15Mi floor cleared", current: reqsRR("", "100Mi", "", ""), rec: ContainerRecommendation{MemoryRequest: qtyp("90Mi")}, tol: tol5, wantMemReq: nil},
		{name: "memory decrease above floor kept", current: reqsRR("", "100Mi", "", ""), rec: ContainerRecommendation{MemoryRequest: qtyp("80Mi")}, tol: tol5, wantMemReq: qtyp("80Mi")},
		{name: "memory large workload percent dominates kept", current: reqsRR("", "2Gi", "", ""), rec: ContainerRecommendation{MemoryRequest: qtyp("1900Mi")}, tol: tol5, wantMemReq: qtyp("1900Mi")},
		{name: "zero tolerance keeps every decrease", current: reqsRR("1000m", "", "", ""), rec: ContainerRecommendation{CPURequest: qtyp("999m")}, tol: Tolerance{}, wantCPUReq: qtyp("999m")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clampDecreaseToTolerance(tc.current, tc.rec, tc.tol)
			assertQtyEq(t, "cpuReq", tc.wantCPUReq, got.CPURequest)
			if tc.wantMemReq != nil || tc.rec.MemoryRequest != nil {
				assertQtyEq(t, "memReq", tc.wantMemReq, got.MemoryRequest)
			}
		})
	}
}

func TestClampLimitRemovalAlwaysKept(t *testing.T) {
	cur := reqsRR("", "", "200m", "256Mi")
	rec := ContainerRecommendation{RemoveCPULimit: true, RemoveMemoryLimit: true}
	got := clampDecreaseToTolerance(cur, rec, tol5)
	if !got.RemoveCPULimit {
		t.Fatal("cpu limit removal must never be suppressed")
	}
	if !got.RemoveMemoryLimit {
		t.Fatal("memory limit removal must never be suppressed")
	}
}

func TestClampRecsToTolerance(t *testing.T) {
	containers := []corev1.Container{{Name: "app", Resources: reqsRR("1000m", "", "", "")}}

	t.Run("clamps matched container and passes through unmatched", func(t *testing.T) {
		recs := map[string]ContainerRecommendation{
			"app":     {CPURequest: qtyp("995m")}, // sub-threshold decrease -> cleared
			"sidecar": {CPURequest: qtyp("3m")},   // no current -> passes through unchanged
		}
		got := ClampRecsToTolerance(containers, recs, tol5)
		if got["app"].CPURequest != nil {
			t.Fatalf("app: want cleared, got %s", got["app"].CPURequest.String())
		}
		if got["sidecar"].CPURequest == nil || got["sidecar"].CPURequest.Cmp(resource.MustParse("3m")) != 0 {
			t.Fatalf("sidecar: want 3m passthrough, got %v", got["sidecar"].CPURequest)
		}
		// input map must not be mutated
		if recs["app"].CPURequest == nil {
			t.Fatal("input recs map was mutated")
		}
	})

	t.Run("empty recs returns same map", func(t *testing.T) {
		recs := map[string]ContainerRecommendation{}
		if got := ClampRecsToTolerance(containers, recs, tol5); len(got) != 0 {
			t.Fatalf("want empty, got %d entries", len(got))
		}
	})
}

// ContainerMatches only ever sees a container that exists — the missing case is
// handled by the caller's mapping and covered in the controller package.
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
