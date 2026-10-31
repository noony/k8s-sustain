package webhook

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/governance"
)

// recSourceDelta returns how much the source counter moved across fn. The
// collector is a package global shared by every test, so an absolute-value
// assertion would flake under -count>1 and -shuffle=on.
func recSourceDelta(t *testing.T, source string, fn func()) float64 {
	t.Helper()
	before := testutil.ToFloat64(RecommendationSourceTotal.WithLabelValues(source))
	fn()
	after := testutil.ToFloat64(RecommendationSourceTotal.WithLabelValues(source))
	return after - before
}

// admit counts the source of the WorkloadRecommendation read its decision
// made, whatever the decision then did with it.
func TestAdmitCountsTheRecommendationSource(t *testing.T) {
	for _, tc := range []struct {
		source string
		funcs  interceptor.Funcs
	}{
		{source: RecSourceHit},
		{source: RecSourceError, funcs: failGet[*sustainv1alpha1.WorkloadRecommendation]()},
	} {
		t.Run(tc.source, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(config.Scheme()).
				WithObjects(basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(withSnapshot)).
				WithInterceptorFuncs(tc.funcs).Build()
			h := &Handler{Policies: governance.NewPolicyLister(nil), Client: c}
			delta := recSourceDelta(t, tc.source, func() {
				h.admit(context.Background(), admissionRequestFor(t, webPod("p")))
			})
			if delta != 1 {
				t.Errorf("%s delta = %v, want 1", tc.source, delta)
			}
		})
	}
}
