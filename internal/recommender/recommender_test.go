package recommender

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// helpers

func int32p(i int32) *int32            { return &i }
func float64p(f float64) *float64      { return &f }
func qty(s string) resource.Quantity   { return resource.MustParse(s) }
func qtyp(s string) *resource.Quantity { q := qty(s); return &q }

// --- ComputeCPURequest ---

func TestComputeCPURequest(t *testing.T) {
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
			cfg:      sustainv1alpha1.ResourceRequestsConfig{HeadroomPercentage: int32p(20)},
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
				HeadroomPercentage: int32p(50),
				MaxAllowed:         qtyp("1"),
			},
			wantQty: "1", // 0.9 * 1.5 = 1.35 → clamped to 1
		},
		{
			name:     "fractional millicore rounds up",
			rawCores: 0.1005,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "101m", // ceil(100.5) = 101
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeCPURequest(tc.rawCores, tc.cfg)
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

// --- ComputeMemoryRequest ---

func TestComputeMemoryRequest(t *testing.T) {
	tests := []struct {
		name     string
		rawBytes float64
		cfg      sustainv1alpha1.ResourceRequestsConfig
		wantNil  bool
		wantQty  string
	}{
		{
			name:     "basic 100Mi",
			rawBytes: 100 * 1024 * 1024,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "100Mi",
		},
		{
			name:     "with 10% headroom",
			rawBytes: 100 * 1024 * 1024,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{HeadroomPercentage: int32p(10)},
			wantQty:  "110Mi",
		},
		{
			name:     "keep request returns nil",
			rawBytes: 512 * 1024 * 1024,
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
			rawBytes: 4 * 1024 * 1024 * 1024,
			cfg:      sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("2Gi")},
			wantQty:  "2Gi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeMemoryRequest(tc.rawBytes, tc.cfg)
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

// --- ComputeLimit ---

func TestComputeLimit(t *testing.T) {
	request := qtyp("200m")
	currentReq := qtyp("100m")
	currentLim := qtyp("300m") // ratio 3×

	tests := []struct {
		name       string
		cfg        sustainv1alpha1.ResourceLimitsConfig
		wantRemove bool
		wantNil    bool
		wantQty    string
	}{
		{
			name:    "keep limit returns nil",
			cfg:     sustainv1alpha1.ResourceLimitsConfig{KeepLimit: true},
			wantNil: true,
		},
		{
			name:       "no limit",
			cfg:        sustainv1alpha1.ResourceLimitsConfig{NoLimit: true},
			wantRemove: true,
		},
		{
			name:    "equals to request",
			cfg:     sustainv1alpha1.ResourceLimitsConfig{EqualsToRequest: true},
			wantQty: "200m",
		},
		{
			name:    "fixed ratio 2×",
			cfg:     sustainv1alpha1.ResourceLimitsConfig{RequestsLimitsRatio: float64p(2.0)},
			wantQty: "400m",
		},
		{
			name:    "keep limit-to-request ratio (3×)",
			cfg:     sustainv1alpha1.ResourceLimitsConfig{KeepLimitRequestRatio: true},
			wantQty: "600m", // 200m * (300m/100m) = 600m
		},
		{
			name:    "no strategy — keep existing (nil)",
			cfg:     sustainv1alpha1.ResourceLimitsConfig{},
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeLimit(request, currentReq, currentLim, tc.cfg)
			if tc.wantRemove {
				if !got.Remove {
					t.Error("expected Remove=true")
				}
				return
			}
			if tc.wantNil {
				if got.Quantity != nil || got.Remove {
					t.Errorf("expected empty LimitResult, got %+v", got)
				}
				return
			}
			if got.Quantity == nil {
				t.Fatal("expected non-nil Quantity")
			}
			want := qty(tc.wantQty)
			if got.Quantity.Cmp(want) != 0 {
				t.Errorf("got %s, want %s", got.Quantity, &want)
			}
		})
	}
}

// --- helpers ---

func TestPercentileQuantile(t *testing.T) {
	if q := PercentileQuantile(nil); q != 0.95 {
		t.Errorf("nil → want 0.95, got %v", q)
	}
	if q := PercentileQuantile(int32p(70)); q != 0.70 {
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
