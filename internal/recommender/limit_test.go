package recommender

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	promclient "github.com/noony/k8s-sustain/internal/prometheus"
)

// Each limit strategy is applied to the final request and recorded as a stage
// named after its Policy field. A kept limit is recorded with the container's
// own value and leaves the Recommendation's limit unset.
func TestLimit_Strategies(t *testing.T) {
	app := limited(corev1.ResourceList{corev1.ResourceCPU: qty("100m")}, corev1.ResourceList{corev1.ResourceCPU: qty("300m")})
	cases := []struct {
		name       string
		cfg        sustainv1alpha1.ResourceLimitsConfig
		wantStage  string
		wantRec    string
		wantRemove bool
		wantStaged string
		wantFacts  map[string]string
	}{
		{"keepLimit", sustainv1alpha1.ResourceLimitsConfig{KeepLimit: true}, "keepLimit", "", false, "300m", nil},
		{"no strategy keeps the limit", sustainv1alpha1.ResourceLimitsConfig{}, "keepLimit", "", false, "300m", nil},
		{"noLimit", sustainv1alpha1.ResourceLimitsConfig{NoLimit: true}, "noLimit", "", true, "", nil},
		{"equalsToRequest", sustainv1alpha1.ResourceLimitsConfig{EqualsToRequest: true}, "equalsToRequest", "200m", false, "200m", nil},
		{"requestsLimitsRatio", sustainv1alpha1.ResourceLimitsConfig{RequestsLimitsRatio: ptr.To(2.0)}, "requestsLimitsRatio", "400m", false, "400m", map[string]string{"ratio": "2"}},
		// 200m x (300m / 100m)
		{"keepLimitRequestRatio", sustainv1alpha1.ResourceLimitsConfig{KeepLimitRequestRatio: true}, "keepLimitRequestRatio", "600m", false, "600m", map[string]string{"ratio": "3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, tr := computeLimited(t, app, 0.2, tc.cfg)

			assertStages(t, "limit", tr.Limit, tc.wantStage)
			st := tr.Limit[0]
			assertFacts(t, st, tc.wantFacts)
			if st.Removed != tc.wantRemove || rec.RemoveCPULimit != tc.wantRemove {
				t.Errorf("removed = stage %v rec %v, want %v", st.Removed, rec.RemoveCPULimit, tc.wantRemove)
			}
			if tc.wantStaged == "" {
				if st.Value != nil {
					t.Errorf("stage value = %s, want none", st.Value)
				}
			} else {
				assertQty(t, "stage value", st.Value, tc.wantStaged)
			}
			if tc.wantRec == "" {
				if rec.CPULimit != nil {
					t.Errorf("recommended limit = %s, want the container's own", rec.CPULimit)
				}
			} else {
				assertQty(t, "recommended limit", rec.CPULimit, tc.wantRec)
			}
		})
	}
}

// A container without a limit has no limit-to-request ratio to keep: it keeps
// having none rather than getting a limit of 0, which the apiserver rejects
// below any request.
func TestLimit_KeepLimitRequestRatioWithoutALimitKeepsNone(t *testing.T) {
	rec, tr := computeLimited(t, limited(corev1.ResourceList{corev1.ResourceCPU: qty("100m")}, nil), 0.2,
		sustainv1alpha1.ResourceLimitsConfig{KeepLimitRequestRatio: true})

	if rec.CPULimit != nil || rec.RemoveCPULimit {
		t.Errorf("cpu limit = %v remove %v, want the container to keep having none", rec.CPULimit, rec.RemoveCPULimit)
	}
	assertStages(t, "limit", tr.Limit, "keepLimitRequestRatio")
	if st := tr.Limit[0]; st.Value != nil || st.Removed {
		t.Errorf("limit stage = %+v, want no limit", st)
	}
}

// Memory ratio limits must be whole bytes: milli math on 100Mi × 1.1 carries
// float64 representation error into a fractional-byte limit (115343360001m)
// that Kubernetes warns about. ceil(104857600 × 1.1) = 115343361 bytes, one
// byte over 110Mi from float64 rounding.
func TestLimit_MemoryRatiosAreWholeBytes(t *testing.T) {
	app := limited(corev1.ResourceList{corev1.ResourceMemory: qty("100Mi")}, corev1.ResourceList{corev1.ResourceMemory: qty("110Mi")})
	for name, cfg := range map[string]sustainv1alpha1.ResourceLimitsConfig{
		"fixed ratio 1.1":                   {RequestsLimitsRatio: ptr.To(1.1)},
		"keep limit-to-request ratio (1.1)": {KeepLimitRequestRatio: true},
	} {
		t.Run(name, func(t *testing.T) {
			res := Compute(Request{
				Containers: []corev1.Container{app},
				Inputs:     &WorkloadInputs{MemPerPod: promclient.ContainerValues{"app": 100 * mib}},
				Resources:  sustainv1alpha1.ResourcesConfigs{Memory: sustainv1alpha1.ResourceConfig{Limits: cfg}},
				Now:        now,
			})

			got := res.Recs["app"].MemoryLimit
			if got == nil || got.MilliValue()%1000 != 0 || got.Value() != 115343361 {
				t.Errorf("limit = %v, want 115343361 whole bytes", got)
			}
		})
	}
}
