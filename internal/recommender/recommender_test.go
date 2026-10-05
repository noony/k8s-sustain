package recommender

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

func qty(s string) resource.Quantity   { return resource.MustParse(s) }
func qtyp(s string) *resource.Quantity { q := qty(s); return &q }

func cpuRequestOf(rawCores float64, cfg sustainv1alpha1.ResourceRequestsConfig) *resource.Quantity {
	rec, _ := computeApp(&WorkloadInputs{CPUPerPod: promclient.ContainerValues{"app": rawCores}},
		sustainv1alpha1.ResourcesConfigs{CPU: sustainv1alpha1.ResourceConfig{Requests: cfg}})
	return rec.CPURequest
}

func memoryRequestOf(rawBytes float64, cfg sustainv1alpha1.ResourceRequestsConfig) *resource.Quantity {
	rec, _ := computeApp(&WorkloadInputs{MemPerPod: promclient.ContainerValues{"app": rawBytes}},
		sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{Requests: cfg}})
	return rec.MemoryRequest
}

func TestCPURequest(t *testing.T) {
	tests := []struct {
		name     string
		rawCores float64
		cfg      sustainv1alpha1.ResourceRequestsConfig
		wantNil  bool
		wantQty  string
	}{
		{
			name:     "basic no headroom",
			rawCores: 0.1,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "100m",
		},
		{
			name:     "with 20% headroom",
			rawCores: 0.1,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](20)},
			wantQty:  "120m",
		},
		{
			name:     "keep request returns nil",
			rawCores: 0.5,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true},
			wantNil:  true,
		},
		{
			name:     "clamp to min",
			rawCores: 0.001,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{MinAllowed: qtyp("50m")},
			wantQty:  "50m",
		},
		{
			name:     "clamp to max",
			rawCores: 4.0,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("2")},
			wantQty:  "2",
		},
		{
			name:     "headroom then clamped to max",
			rawCores: 0.9,
			cfg: sustainv1alpha1.ResourceRequestsConfig{
				Headroom:   ptr.To[int32](50),
				MaxAllowed: qtyp("1"),
			},
			wantQty: "1", // 0.9 * 1.5 = 1.35 → clamped to 1
		},
		{
			name:     "fractional millicore rounds up",
			rawCores: 0.1005,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "101m", // ceil(100.5) = 101
		},
		{
			name:     "idle container gets the hard minimum",
			rawCores: 0,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "1m",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cpuRequestOf(tc.rawCores, tc.cfg)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %s", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil quantity")
			}
			want := qty(tc.wantQty)
			if got.Cmp(want) != 0 {
				t.Errorf("got %s, want %s", got, want.String())
			}
		})
	}
}

func TestMemoryRequest(t *testing.T) {
	tests := []struct {
		name     string
		rawBytes float64
		cfg      sustainv1alpha1.ResourceRequestsConfig
		wantNil  bool
		wantQty  string
	}{
		{
			name:     "basic 100Mi",
			rawBytes: 100 * mib,
			wantQty:  "100Mi",
		},
		{
			name:     "with 10% headroom",
			rawBytes: 100 * mib,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{Headroom: ptr.To[int32](10)},
			wantQty:  "110Mi",
		},
		{
			name:     "keep request returns nil",
			rawBytes: 512 * mib,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true},
			wantNil:  true,
		},
		{
			name:     "clamp to min",
			rawBytes: 1024,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{MinAllowed: qtyp("64Mi")},
			wantQty:  "64Mi",
		},
		{
			name:     "clamp to max",
			rawBytes: 4 * 1024 * mib,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("2Gi")},
			wantQty:  "2Gi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := memoryRequestOf(tc.rawBytes, tc.cfg)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %s", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil quantity")
			}
			want := qty(tc.wantQty)
			if got.Cmp(want) != 0 {
				t.Errorf("got %s, want %s", got, want.String())
			}
		})
	}
}

func TestPercentileQuantile(t *testing.T) {
	if q := PercentileQuantile(nil); q != 0.95 {
		t.Errorf("nil → want 0.95, got %v", q)
	}
	if q := PercentileQuantile(ptr.To[int32](70)); q != 0.70 {
		t.Errorf("70 → want 0.70, got %v", q)
	}
}

func TestResourceWindow(t *testing.T) {
	if w := ResourceWindow(""); w != defaultWindow {
		t.Errorf("empty → want %s, got %s", defaultWindow, w)
	}
	if w := ResourceWindow("96h"); w != "96h" {
		t.Errorf("96h → want 96h, got %s", w)
	}
}
