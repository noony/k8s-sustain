package recommender

import (
	"math"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestApplyOverhead(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		target     int32
		want       string
		wantFactor float64
	}{
		{"no target leaves it unchanged", "100m", 0, "100m", 1},
		{"negative target leaves it unchanged", "100m", -5, "100m", 1},
		{"100m at 70% rounds up", "100m", 70, "158m", 110.0 / 70},
		{"target clamped to 99", "100m", 200, "112m", 110.0 / 99},
		{"memory", "128Mi", 80, "176Mi", 1.375},
		// 100Mi × 110 / 70 = 164776228.57... bytes: milli math would emit a
		// fractional-byte quantity Kubernetes warns about.
		{"memory rounds up to whole bytes", "100Mi", 70, "164776229", 110.0 / 70},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, factor := applyOverhead(resource.MustParse(tc.in), tc.target)
			if want := resource.MustParse(tc.want); got.Cmp(want) != 0 {
				t.Errorf("got %s, want %s", &got, &want)
			}
			if got.Format == resource.BinarySI && got.MilliValue()%1000 != 0 {
				t.Errorf("got fractional bytes %s", &got)
			}
			if math.Abs(factor-tc.wantFactor) > 1e-9 {
				t.Errorf("factor = %v, want %v", factor, tc.wantFactor)
			}
		})
	}
}

func TestApplyReplicaCorrection(t *testing.T) {
	cases := []struct {
		name            string
		in              string
		anchor          float64
		current, mn, mx int32
		want            string
		wantFactor      float64
	}{
		// target = round(1 + 0.10 × 9) = 2 for the 0.10 anchor.
		{"at target", "100m", 0.10, 2, 1, 10, "100m", 1},
		{"above target capped at 2", "100m", 0.10, 20, 1, 10, "200m", 2},
		{"below target capped at 0.5", "400m", 0.10, 1, 1, 10, "200m", 0.5},
		{"no budget", "100m", 0.10, 5, 5, 5, "100m", 1},
		{"scaled to zero", "100m", 0.10, 0, 1, 10, "100m", 1},
		{"anchor at max targets max replicas", "400m", 1.0, 2, 1, 10, "200m", 0.5},
		{"anchor at min targets min replicas", "100m", 0.0, 4, 1, 10, "200m", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, factor := applyReplicaCorrection(resource.MustParse(tc.in), tc.anchor, tc.current, tc.mn, tc.mx)
			if want := resource.MustParse(tc.want); got.Cmp(want) != 0 {
				t.Errorf("got %s, want %s", &got, &want)
			}
			if factor != tc.wantFactor {
				t.Errorf("factor = %v, want %v", factor, tc.wantFactor)
			}
		})
	}
}
