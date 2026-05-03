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
			cfg:      sustainv1alpha1.ResourceRequestsConfig{Headroom: int32p(20)},
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
				Headroom: int32p(50),
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
			cfg:      sustainv1alpha1.ResourceRequestsConfig{Headroom: int32p(10)},
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

// --- ComputeMemoryRequestWithOOM ---

func TestComputeMemoryRequestWithOOM(t *testing.T) {
	mib := int64(1024 * 1024)
	tests := []struct {
		name     string
		rawBytes float64
		signal   OOMSignal
		cfg      sustainv1alpha1.ResourceRequestsConfig
		wantNil  bool
		wantQty  string
	}{
		{
			name:     "no recent oom keeps default behavior",
			rawBytes: 100 * float64(mib),
			signal:   OOMSignal{Recent: false},
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "100Mi",
		},
		{
			// Floor = peak. Headroom is applied once to peak; current_request
			// is NOT a floor (would otherwise compound across reconciles).
			name:     "recent oom raises floor to peak",
			rawBytes: 50 * float64(mib),
			signal:   OOMSignal{Recent: true, PeakBytes: 200 * float64(mib), CurrentRequestBytes: 100 * float64(mib)},
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "200Mi",
		},
		{
			// current_request is intentionally NOT a floor: the previous reco's
			// already-headroomed value would otherwise multiply on each
			// reconcile, causing runaway growth even after the workload fits.
			// MinAllowed is the proper way to enforce "never shrink below X".
			name:     "current_request is not a floor (no runaway)",
			rawBytes: 50 * float64(mib),
			signal:   OOMSignal{Recent: true, PeakBytes: 80 * float64(mib), CurrentRequestBytes: 150 * float64(mib)},
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "80Mi",
		},
		{
			name:     "recent oom raw above floor wins",
			rawBytes: 300 * float64(mib),
			signal:   OOMSignal{Recent: true, PeakBytes: 100 * float64(mib), CurrentRequestBytes: 100 * float64(mib)},
			cfg:      sustainv1alpha1.ResourceRequestsConfig{},
			wantQty:  "300Mi",
		},
		{
			// Headroom is applied to the peak ONCE, never compounded with raw.
			// raw=50Mi → 60Mi; peak=100Mi → 120Mi; max(60, 120) = 120Mi.
			name:     "recent oom headroom applied to peak",
			rawBytes: 50 * float64(mib),
			signal:   OOMSignal{Recent: true, PeakBytes: 100 * float64(mib), CurrentRequestBytes: 100 * float64(mib)},
			cfg:      sustainv1alpha1.ResourceRequestsConfig{Headroom: int32p(20)},
			wantQty:  "120Mi",
		},
		{
			name:     "max allowed wins over oom floor",
			rawBytes: 50 * float64(mib),
			signal:   OOMSignal{Recent: true, PeakBytes: 500 * float64(mib), CurrentRequestBytes: 100 * float64(mib)},
			cfg:      sustainv1alpha1.ResourceRequestsConfig{MaxAllowed: qtyp("256Mi")},
			wantQty:  "256Mi",
		},
		{
			name:     "keep request returns nil even with recent oom",
			rawBytes: 50 * float64(mib),
			signal:   OOMSignal{Recent: true, PeakBytes: 200 * float64(mib), CurrentRequestBytes: 100 * float64(mib)},
			cfg:      sustainv1alpha1.ResourceRequestsConfig{KeepRequest: true},
			wantNil:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeMemoryRequestWithOOM(tc.rawBytes, tc.signal, tc.cfg)
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

// FloorApplied indicates the OOM floor produced the final value, used by metrics.
func TestComputeMemoryRequestWithOOM_FloorAppliedFlag(t *testing.T) {
	mib := int64(1024 * 1024)
	// Floor wins
	q, applied := ComputeMemoryRequestWithOOMFloorReport(
		50*float64(mib),
		OOMSignal{Recent: true, PeakBytes: 200 * float64(mib), CurrentRequestBytes: 100 * float64(mib)},
		sustainv1alpha1.ResourceRequestsConfig{},
	)
	if !applied {
		t.Errorf("expected floor applied, got false (q=%s)", q)
	}
	// Raw wins
	_, applied = ComputeMemoryRequestWithOOMFloorReport(
		400*float64(mib),
		OOMSignal{Recent: true, PeakBytes: 200 * float64(mib), CurrentRequestBytes: 100 * float64(mib)},
		sustainv1alpha1.ResourceRequestsConfig{},
	)
	if applied {
		t.Errorf("expected floor NOT applied when raw exceeds it")
	}
	// No recent OOM
	_, applied = ComputeMemoryRequestWithOOMFloorReport(
		50*float64(mib),
		OOMSignal{Recent: false},
		sustainv1alpha1.ResourceRequestsConfig{},
	)
	if applied {
		t.Errorf("expected floor NOT applied when no recent OOM")
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
