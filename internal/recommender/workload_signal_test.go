package recommender

import (
	"testing"
)

func TestEffectiveReplicas(t *testing.T) {
	tests := []struct {
		name        string
		median      float64
		minReplicas int32
		want        float64
	}{
		{"normal", 4.0, 2, 4.0},
		{"zero falls back to min", 0.0, 3, 3.0},
		{"zero with zero min defaults to 1", 0.0, 0, 1.0},
		{"non-zero ignored by min", 5.5, 10, 5.5},
		{"negative coerced to 1", -1.0, 0, 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveReplicas(tc.median, tc.minReplicas)
			if got != tc.want {
				t.Errorf("EffectiveReplicas(%v,%v) = %v, want %v", tc.median, tc.minReplicas, got, tc.want)
			}
		})
	}
}

func TestPerPodFromTotal(t *testing.T) {
	if got := PerPodFromTotal(10.0, 4.0); got != 2.5 {
		t.Errorf("PerPodFromTotal(10,4) = %v, want 2.5", got)
	}
	if got := PerPodFromTotal(10.0, 0.0); got != 10.0 {
		t.Errorf("PerPodFromTotal with zero replicas should fall back to total, got %v", got)
	}
}

func TestApplyFloor(t *testing.T) {
	if got := ApplyFloor(2.0, 5.0); got != 5.0 {
		t.Errorf("ApplyFloor(2,5) = %v, want 5 (floor wins)", got)
	}
	if got := ApplyFloor(7.0, 5.0); got != 7.0 {
		t.Errorf("ApplyFloor(7,5) = %v, want 7 (value wins)", got)
	}
	if got := ApplyFloor(7.0, 0.0); got != 7.0 {
		t.Errorf("ApplyFloor with zero floor should return value, got %v", got)
	}
}
