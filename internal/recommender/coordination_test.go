package recommender

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/autoscaler"
	"github.com/noony/k8s-sustain/internal/workload"
)

func TestApplyOverhead_NoTarget(t *testing.T) {
	in := resource.MustParse("100m")
	out := ApplyOverhead(&in, 0)
	if out == nil {
		t.Fatal("expected non-nil quantity")
	}
	if out.MilliValue() != in.MilliValue() {
		t.Errorf("no target → unchanged: got %d, want %d", out.MilliValue(), in.MilliValue())
	}
}

func TestApplyOverhead_70Percent(t *testing.T) {
	in := resource.MustParse("100m")
	out := ApplyOverhead(&in, 70)
	if out == nil {
		t.Fatal("expected non-nil quantity")
	}
	// 100 * 110 / 70 = 157.14... → ceil → 158m
	if got, want := out.MilliValue(), int64(158); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

func TestApplyOverhead_TargetClampedHigh(t *testing.T) {
	in := resource.MustParse("100m")
	// target 200 → clamped to 99; 100 * 110 / 99 ≈ 111.1 → ceil = 112
	out := ApplyOverhead(&in, 200)
	if out == nil {
		t.Fatal("expected non-nil quantity")
	}
	if got, want := out.MilliValue(), int64(112); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

func TestApplyOverhead_NegativeTarget(t *testing.T) {
	in := resource.MustParse("100m")
	// negative or zero target → no HPA target → unchanged
	out := ApplyOverhead(&in, -5)
	if out == nil {
		t.Fatal("expected non-nil quantity")
	}
	if out.MilliValue() != in.MilliValue() {
		t.Errorf("negative target → unchanged: got %d, want %d", out.MilliValue(), in.MilliValue())
	}
}

func TestApplyOverhead_Memory(t *testing.T) {
	in := resource.MustParse("128Mi")
	out := ApplyOverhead(&in, 80)
	if out == nil {
		t.Fatal("expected non-nil quantity")
	}
	// 128Mi × 110 / 80 = 176Mi
	if got, want := out.Value(), int64(176*1024*1024); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

func TestApplyOverhead_Nil(t *testing.T) {
	out := ApplyOverhead(nil, 70)
	if out != nil {
		t.Errorf("expected nil, got %v", out)
	}
}

func TestApplyReplicaCorrection_Disabled(t *testing.T) {
	in := resource.MustParse("100m")
	var anchor *float64
	out := applyReplicaCorrection(&in, anchor, 5, 1, 10)
	if out.MilliValue() != in.MilliValue() {
		t.Fatalf("nil anchor → no-op, got %dm want %dm", out.MilliValue(), in.MilliValue())
	}
}

func TestApplyReplicaCorrection_AtTarget(t *testing.T) {
	in := resource.MustParse("100m")
	anchor := 0.10
	// target = round(1 + 0.10 * (10-1)) = round(1.9) = 2
	out := applyReplicaCorrection(&in, &anchor, 2, 1, 10)
	if out.MilliValue() != in.MilliValue() {
		t.Fatalf("current==target → factor 1, got %dm", out.MilliValue())
	}
}

func TestApplyReplicaCorrection_AboveTarget_Capped(t *testing.T) {
	in := resource.MustParse("100m")
	anchor := 0.10
	// target=2, current=20 → raw=10 → clamped to 2.0 → 200m
	out := applyReplicaCorrection(&in, &anchor, 20, 1, 10)
	if out.MilliValue() != 200 {
		t.Fatalf("expected 200m, got %dm", out.MilliValue())
	}
}

func TestApplyReplicaCorrection_BelowTarget_Capped(t *testing.T) {
	in := resource.MustParse("400m")
	anchor := 0.10
	// target=2, current=1 → raw=0.5 → clamped to 0.5 → 200m
	out := applyReplicaCorrection(&in, &anchor, 1, 1, 10)
	if out.MilliValue() != 200 {
		t.Fatalf("expected 200m, got %dm", out.MilliValue())
	}
}

func TestApplyReplicaCorrection_NoBudget(t *testing.T) {
	in := resource.MustParse("100m")
	anchor := 0.10
	out := applyReplicaCorrection(&in, &anchor, 5, 5, 5)
	if out.MilliValue() != in.MilliValue() {
		t.Fatalf("min == max → no-op, got %dm", out.MilliValue())
	}
}

func TestApplyReplicaCorrection_ZeroCurrent(t *testing.T) {
	in := resource.MustParse("100m")
	anchor := 0.10
	out := applyReplicaCorrection(&in, &anchor, 0, 1, 10)
	if out.MilliValue() != in.MilliValue() {
		t.Fatalf("current == 0 → no-op, got %dm", out.MilliValue())
	}
}

func TestApplyReplicaCorrection_AnchorAtMax(t *testing.T) {
	in := resource.MustParse("400m")
	anchor := 1.0
	// target=10, current=2 → raw=0.2 → clamped to 0.5 → 200m
	out := applyReplicaCorrection(&in, &anchor, 2, 1, 10)
	if out.MilliValue() != 200 {
		t.Fatalf("expected 200m, got %dm", out.MilliValue())
	}
}

func TestApplyReplicaCorrection_AnchorAtMin(t *testing.T) {
	in := resource.MustParse("100m")
	anchor := 0.0
	// target=1, current=4 → raw=4 → clamped to 2.0 → 200m
	out := applyReplicaCorrection(&in, &anchor, 4, 1, 10)
	if out.MilliValue() != 200 {
		t.Fatalf("expected 200m, got %dm", out.MilliValue())
	}
}

func TestApplyReplicaCorrection_NilQty(t *testing.T) {
	var anchor = 0.10
	if got := applyReplicaCorrection(nil, &anchor, 5, 1, 10); got != nil {
		t.Fatalf("nil qty → nil out, got %v", got)
	}
}

func TestApplyCoordination_Disabled(t *testing.T) {
	base := workload.ContainerRecommendation{
		CPURequest:    qtyp("100m"),
		MemoryRequest: qtyp("128Mi"),
	}
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: false}
	info := autoscaler.Info{Kind: autoscaler.KindNone}

	out := ApplyCoordination(base, cfg, info, sustainv1alpha1.ResourcesConfigs{})
	if out.CPURequest.MilliValue() != 100 {
		t.Errorf("CPU expected 100m unchanged, got %dm", out.CPURequest.MilliValue())
	}
	if out.MemoryRequest.Value() != 128*1024*1024 {
		t.Errorf("memory expected 128Mi unchanged, got %d", out.MemoryRequest.Value())
	}
}

func TestApplyCoordination_KindNone_NoOp(t *testing.T) {
	base := workload.ContainerRecommendation{CPURequest: qtyp("100m")}
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: true}
	info := autoscaler.Info{Kind: autoscaler.KindNone}

	out := ApplyCoordination(base, cfg, info, sustainv1alpha1.ResourcesConfigs{})
	if out.CPURequest.MilliValue() != 100 {
		t.Errorf("KindNone -> no-op, got %dm", out.CPURequest.MilliValue())
	}
}

func TestApplyCoordination_OverheadOnly(t *testing.T) {
	base := workload.ContainerRecommendation{
		CPURequest:    qtyp("100m"),
		MemoryRequest: qtyp("128Mi"),
	}
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: true}
	info := autoscaler.Info{
		Kind:              autoscaler.KindHPA,
		ConfiguredTargets: map[string]int32{"cpu": 70},
		MinReplicas:       1, MaxReplicas: 10, CurrentReplicas: 5,
	}

	out := ApplyCoordination(base, cfg, info, sustainv1alpha1.ResourcesConfigs{})
	if out.CPURequest.MilliValue() != 158 {
		t.Errorf("CPU overhead expected 158m, got %dm", out.CPURequest.MilliValue())
	}
	if out.MemoryRequest.Value() != 128*1024*1024 {
		t.Errorf("memory unchanged when no memory target, got %d", out.MemoryRequest.Value())
	}
}

func TestApplyCoordination_OverheadPlusReplica(t *testing.T) {
	base := workload.ContainerRecommendation{CPURequest: qtyp("100m")}
	anchor := 0.10
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: true, ReplicaBudgetAnchor: &anchor}
	info := autoscaler.Info{
		Kind:              autoscaler.KindHPA,
		ConfiguredTargets: map[string]int32{"cpu": 70},
		MinReplicas:       1, MaxReplicas: 10, CurrentReplicas: 20,
	}

	out := ApplyCoordination(base, cfg, info, sustainv1alpha1.ResourcesConfigs{})
	// overhead: 100 * 110 / 70 = ceil(157.14) = 158
	// replica: target = round(1 + 0.1*9) = 2; current=20 -> raw=10 -> clamp=2.0
	// 158 * 2 = 316
	if out.CPURequest.MilliValue() != 316 {
		t.Errorf("expected 316m, got %dm", out.CPURequest.MilliValue())
	}
}

func TestApplyCoordination_RespectsMaxAllowed(t *testing.T) {
	base := workload.ContainerRecommendation{CPURequest: qtyp("100m")}
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: true}
	info := autoscaler.Info{
		Kind:              autoscaler.KindHPA,
		ConfiguredTargets: map[string]int32{"cpu": 70},
	}
	cap := resource.MustParse("120m")
	res := sustainv1alpha1.ResourcesConfigs{
		CPU: sustainv1alpha1.ResourceConfig{
			Requests: sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: &cap},
		},
	}
	out := ApplyCoordination(base, cfg, info, res)
	if out.CPURequest.MilliValue() != 120 {
		t.Errorf("MaxAllowed should clamp bumped request to 120m, got %dm", out.CPURequest.MilliValue())
	}
}

func TestApplyCoordination_RespectsMinAllowedOnDownscale(t *testing.T) {
	base := workload.ContainerRecommendation{CPURequest: qtyp("400m")}
	anchor := 0.10
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: true, ReplicaBudgetAnchor: &anchor}
	info := autoscaler.Info{
		Kind:              autoscaler.KindHPA,
		ConfiguredTargets: map[string]int32{}, // no CPU target -> overhead is 1.0
		MinReplicas:       1, MaxReplicas: 10, CurrentReplicas: 1,
	}
	floor := resource.MustParse("300m")
	res := sustainv1alpha1.ResourcesConfigs{
		CPU: sustainv1alpha1.ResourceConfig{
			Requests: sustainv1alpha1.ResourceRequestsConfig{MinAllowed: &floor},
		},
	}
	// target=2, current=1 -> raw=0.5 -> clamped to 0.5 -> 200m, but MinAllowed=300m -> 300m
	out := ApplyCoordination(base, cfg, info, res)
	if out.CPURequest.MilliValue() != 300 {
		t.Errorf("MinAllowed should floor to 300m, got %dm", out.CPURequest.MilliValue())
	}
}

func TestApplyCoordination_MemoryOverheadApplied(t *testing.T) {
	base := workload.ContainerRecommendation{MemoryRequest: qtyp("128Mi")}
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: true}
	info := autoscaler.Info{
		Kind:              autoscaler.KindHPA,
		ConfiguredTargets: map[string]int32{"memory": 80},
	}

	out := ApplyCoordination(base, cfg, info, sustainv1alpha1.ResourcesConfigs{})
	// 128Mi * 110 / 80 = 176Mi (exact) — memory test from T5
	if out.MemoryRequest.Value() != 176*1024*1024 {
		t.Errorf("memory overhead expected 176Mi, got %d bytes", out.MemoryRequest.Value())
	}
}

func TestApplyCoordination_NilRequests(t *testing.T) {
	base := workload.ContainerRecommendation{} // both nil
	cfg := sustainv1alpha1.AutoscalerCoordination{Enabled: true}
	info := autoscaler.Info{
		Kind:              autoscaler.KindHPA,
		ConfiguredTargets: map[string]int32{"cpu": 70, "memory": 80},
	}
	out := ApplyCoordination(base, cfg, info, sustainv1alpha1.ResourcesConfigs{})
	if out.CPURequest != nil {
		t.Errorf("nil CPU should stay nil, got %v", out.CPURequest)
	}
	if out.MemoryRequest != nil {
		t.Errorf("nil memory should stay nil, got %v", out.MemoryRequest)
	}
}
