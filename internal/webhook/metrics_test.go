package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/wlrcache"
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

func TestAdmit_RecommendationSourceMetric_Hit(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	wlr := freshWLR("Deployment", "default", "my-app", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	env := newAdmitEnv(t, policy, rs, wlr)
	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")

	var resp *admissionv1.AdmissionResponse
	delta := recSourceDelta(t, RecSourceHit, func() {
		resp = env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	})

	if delta != 1 {
		t.Errorf("RecSourceHit delta = %v, want 1", delta)
	}
	if !resp.Allowed || resp.Patch == nil {
		t.Fatal("expected an allowed admission with an injection patch")
	}
}

// A stale WLR must change only the metric label: the fail-open admission
// behaviour (no injection patch) stays unchanged.
func TestAdmit_RecommendationSourceMetric_Stale(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	staleWLR := &sustainv1alpha1.WorkloadRecommendation{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: wlrcache.Name("Deployment", "my-app")},
		Status: sustainv1alpha1.WorkloadRecommendationStatus{
			ObservedAt: metav1.NewTime(time.Now().Add(-2 * time.Hour)), // > DefaultCacheStaleness (30m)
			Containers: map[string]sustainv1alpha1.ContainerRecommendation{
				"app": wlrRec("100m", "64Mi"),
			},
		},
	}
	env := newAdmitEnv(t, policy, rs, staleWLR)
	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")

	var resp *admissionv1.AdmissionResponse
	delta := recSourceDelta(t, RecSourceStale, func() {
		resp = env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	})

	if delta != 1 {
		t.Errorf("RecSourceStale delta = %v, want 1", delta)
	}
	if !resp.Allowed {
		t.Fatal("expected fail-open allow for stale WLR")
	}
	if resp.Patch != nil {
		t.Errorf("a stale WLR must never produce a resource injection patch, got %d bytes: %s", len(resp.Patch), string(resp.Patch))
	}
}

func TestAdmit_RecommendationSourceMetric_Missing(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")
	env := newAdmitEnv(t, policy, rs) // no WLR seeded
	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")

	var resp *admissionv1.AdmissionResponse
	delta := recSourceDelta(t, RecSourceMissing, func() {
		resp = env.handler.admit(context.Background(), admissionRequestFor(t, pod))
	})

	if delta != 1 {
		t.Errorf("RecSourceMissing delta = %v, want 1", delta)
	}
	if !resp.Allowed || resp.Patch != nil {
		t.Errorf("expected fail-open allow without a patch, got allowed=%v patch=%d bytes", resp.Allowed, len(resp.Patch))
	}
}

// A WLR read failure other than NotFound must land in "error", not "missing".
// The interceptor fails only WorkloadRecommendation Gets, so the Policy and
// ReplicaSet lookups earlier in admit() still succeed.
func TestAdmit_RecommendationSourceMetric_Error(t *testing.T) {
	policy := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	rs := deploymentReplicaSet("default", "my-app-rs", "my-app")

	scheme := runtime.NewScheme()
	if err := sustainv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme sustain: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme apps: %v", err)
	}
	boom := errors.New("boom: simulated apiserver failure")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, rs).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*sustainv1alpha1.WorkloadRecommendation); ok {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	h := &Handler{Client: fc}
	pod := podWithRSOwner("default", "my-app-rs-xyz", "my-app-rs", "p")

	var resp *admissionv1.AdmissionResponse
	delta := recSourceDelta(t, RecSourceError, func() {
		resp = h.admit(context.Background(), admissionRequestFor(t, pod))
	})

	if delta != 1 {
		t.Errorf("RecSourceError delta = %v, want 1", delta)
	}
	if !resp.Allowed {
		t.Fatal("expected fail-open allow on WLR read error")
	}
	if resp.Patch != nil {
		t.Errorf("expected no patch on WLR read error, got %d bytes", len(resp.Patch))
	}
}
