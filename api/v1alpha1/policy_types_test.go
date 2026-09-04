package v1alpha1

import "testing"

// TestEffectiveRecommendOnly pins the OR semantics: an explicit false on
// the policy cannot override the global flag.
func TestEffectiveRecommendOnly(t *testing.T) {
	cases := []struct {
		name   string
		global bool
		policy bool
		want   bool
	}{
		{"both off applies changes", false, false, false},
		{"policy field alone dry-runs", false, true, true},
		{"global flag alone dry-runs", true, false, true},
		{"both on dry-runs", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Policy{Spec: PolicySpec{RightSizing: RightSizingSpec{RecommendOnly: tc.policy}}}
			if got := p.EffectiveRecommendOnly(tc.global); got != tc.want {
				t.Errorf("EffectiveRecommendOnly(global=%v) with policy field %v = %v, want %v",
					tc.global, tc.policy, got, tc.want)
			}
		})
	}
}
