package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/config"
	"github.com/noony/k8s-sustain/internal/governance"
	"github.com/noony/k8s-sustain/internal/workload"
)

// webPod is a pod of Deployment web in namespace default, through ReplicaSet
// web-rs, opting into policy on its own annotations (none when empty).
func webPod(policy string) *corev1.Pod {
	return podWithRSOwner("default", "web-rs-1", "web-rs", policy)
}

func webRS() *appsv1.ReplicaSet { return deploymentReplicaSet("default", "web-rs", "web") }

// webWLR is Deployment web's WorkloadRecommendation, computed by p just now,
// with f applied.
func webWLR(f func(*sustainv1alpha1.WorkloadRecommendation)) *sustainv1alpha1.WorkloadRecommendation {
	w := freshWLR("Deployment", "default", "web", map[string]sustainv1alpha1.ContainerRecommendation{
		"app": wlrRec("100m", "64Mi"),
	})
	if f != nil {
		f(w)
	}
	return w
}

// withSnapshot gives an object the container snapshot the controller writes,
// so a row asserts what the verdict asks for rather than a snapshot refresh.
func withSnapshot(w *sustainv1alpha1.WorkloadRecommendation) {
	w.Status.ObservedResources = map[string]sustainv1alpha1.ObservedContainerResources{"app": {}}
}

// barePod is a pod with no controller, its own workload, in namespace default.
func barePod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "etl-daily-run-1", Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
}

func etlPod(policy string) *corev1.Pod {
	return barePod(map[string]string{
		sustainv1alpha1.PolicyAnnotation:    policy,
		sustainv1alpha1.OwnerNameAnnotation: "etl-daily",
	})
}

// podPolicy is a Policy managing bare pods only.
func podPolicy(name string) *sustainv1alpha1.Policy {
	p := basicPolicy(name, sustainv1alpha1.UpdateModeOnCreate)
	mode := sustainv1alpha1.UpdateModeOnCreate
	p.Spec.RightSizing.Update.Types = sustainv1alpha1.UpdateTypes{Pod: &mode}
	return p
}

func policyWith(f func(*sustainv1alpha1.Policy)) *sustainv1alpha1.Policy {
	p := basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate)
	f(p)
	return p
}

func failGet[T client.Object]() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(T); ok {
				return errors.New("boom")
			}
			return cl.Get(ctx, key, o, opts...)
		},
	}
}

// writesOf is what an admission sets, one "container=cpuRequest[/cpuLimit]"
// per write, init containers prefixed "init:".
func writesOf(a admission) []string {
	var out []string
	for _, w := range a.writes {
		s := w.Name + "=" + w.Resources.Requests.Cpu().String()
		if l, ok := w.Resources.Limits[corev1.ResourceCPU]; ok {
			s += "/" + l.String()
		}
		if w.Init {
			s = "init:" + s
		}
		out = append(out, s)
	}
	return out
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name          string
		objs          []client.Object
		funcs         interceptor.Funcs
		pod           *corev1.Pod
		recommendOnly bool
		excluded      []string
		reason        reason
		source        string
		stub          bool
		writes        []string
	}{
		{
			name:   "a pod without a controller or an owner-name reports into no identity",
			objs:   []client.Object{podPolicy("p")},
			pod:    barePod(map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}),
			reason: reason(governance.NoOwner),
		},
		{
			name:   "no Policy could select the pod",
			objs:   []client.Object{policyWith(func(p *sustainv1alpha1.Policy) { p.Spec.Selector.Namespaces = []string{"other"} }), webRS()},
			pod:    webPod(""),
			reason: reason(governance.NoPolicySelects),
		},
		{
			name: "nothing opts the pod in",
			objs: []client.Object{
				basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(),
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}},
			},
			pod:    webPod(""),
			reason: reason(governance.NotOptedIn),
		},
		{
			name: "the pod opts out",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(nil)},
			pod: func() *corev1.Pod {
				p := webPod("p")
				p.Annotations[sustainv1alpha1.OptOutAnnotation] = "true"
				return p
			}(),
			reason: reason(governance.OptedOut),
		},
		{
			name:   "the Policy the pod opts into does not exist",
			objs:   []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(nil)},
			pod:    webPod("ghost"),
			reason: reason(governance.PolicyMissing),
		},
		{
			name:     "the namespace is excluded",
			objs:     []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(nil)},
			excluded: []string{"default"},
			pod:      webPod("p"),
			reason:   reason(governance.NamespaceExcluded),
		},
		{
			name:   "the Policy does not select the namespace",
			objs:   []client.Object{policyWith(func(p *sustainv1alpha1.Policy) { p.Spec.Selector.Namespaces = []string{"other"} }), webRS(), webWLR(nil)},
			pod:    webPod("p"),
			reason: reason(governance.NamespaceNotSelected),
		},
		{
			name: "the Policy's label selector is invalid",
			objs: []client.Object{policyWith(func(p *sustainv1alpha1.Policy) {
				p.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: "Bogus"}}}
			}), webRS(), webWLR(nil)},
			pod:    webPod("p"),
			reason: reason(governance.SelectorInvalid),
		},
		{
			name: "the Policy's label selector does not match the pod",
			objs: []client.Object{policyWith(func(p *sustainv1alpha1.Policy) {
				p.Spec.Selector.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "web"}}
			}), webRS(), webWLR(nil)},
			pod:    webPod("p"),
			reason: reason(governance.LabelsNotSelected),
		},
		{
			// A Recommendation is seeded, so only the kind check can withhold it.
			name: "the Policy does not manage the kind",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), freshWLR("Pod", "default", "etl-daily", map[string]sustainv1alpha1.ContainerRecommendation{
				"app": wlrRec("100m", "64Mi"),
			})},
			pod:    etlPod("p"),
			reason: reason(governance.KindNotManaged),
		},
		{
			name:   "a governance read fails",
			objs:   []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(nil)},
			funcs:  failGet[*appsv1.ReplicaSet](),
			pod:    webPod("p"),
			reason: reasonGovernanceFailed,
		},
		{
			name:   "the WorkloadRecommendation read fails",
			objs:   []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(nil)},
			funcs:  failGet[*sustainv1alpha1.WorkloadRecommendation](),
			pod:    webPod("p"),
			reason: reasonRecommendationUnreadable,
			source: RecSourceError,
		},
		{
			name:   "no WorkloadRecommendation asks for a stub",
			objs:   []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS()},
			pod:    webPod("p"),
			reason: reason(RecSourceMissing),
			source: RecSourceMissing,
			stub:   true,
		},
		{
			name: "an undecided object is already in the controller's work-list",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status = sustainv1alpha1.WorkloadRecommendationStatus{}
				withSnapshot(w)
			})},
			pod:    webPod("p"),
			reason: reason(RecSourceUndecided),
			source: RecSourceUndecided,
		},
		{
			name: "no data yet",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status.Containers = nil
				w.Status.Outcome = sustainv1alpha1.OutcomeNoData
				withSnapshot(w)
			})},
			pod:    webPod("p"),
			reason: reason(RecSourceNoData),
			source: RecSourceNoData,
		},
		{
			// The losing side of a Conflicted identity (ADR 0002).
			name: "the object belongs to another Policy",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Spec.Policy, w.Status.ComputedBy = "q", "q"
				w.Status.Outcome = sustainv1alpha1.OutcomeConflicted
			})},
			pod:    webPod("p"),
			reason: reason(RecSourceOtherPolicy),
			source: RecSourceOtherPolicy,
		},
		{
			// Adopted by p, still holding the previous Policy's numbers (ADR 0003).
			name: "the Recommendation was computed by another Policy",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status.ComputedBy = "previous"
				w.Status.Outcome = sustainv1alpha1.OutcomeNoData
				withSnapshot(w)
			})},
			pod:    webPod("p"),
			reason: reason(RecSourceOtherPolicy),
			source: RecSourceOtherPolicy,
		},
		{
			name: "a stale Recommendation",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status.ObservedAt = metav1.NewTime(time.Now().Add(-24 * time.Hour))
				withSnapshot(w)
			})},
			pod:    webPod("p"),
			reason: reason(RecSourceStale),
			source: RecSourceStale,
		},
		{
			name: "a retained Recommendation past the retention window",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status.Departed = true
				w.Status.ObservedAt = metav1.NewTime(time.Now().Add(-200 * time.Hour))
				withSnapshot(w)
			})},
			pod:    webPod("p"),
			reason: reason(RecSourceStale),
			source: RecSourceStale,
		},
		{
			name: "the Recommendation names none of the pod's containers",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status.Containers = map[string]sustainv1alpha1.ContainerRecommendation{"other": wlrRec("100m", "64Mi")}
				withSnapshot(w)
			})},
			pod:    webPod("p"),
			reason: reasonNoContainerMatches,
			source: RecSourceHit,
		},
		{
			name:          "recommend-only by flag",
			objs:          []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(withSnapshot)},
			recommendOnly: true,
			pod:           webPod("p"),
			reason:        reasonRecommendOnly,
			source:        RecSourceHit,
		},
		{
			name:   "recommend-only by Policy",
			objs:   []client.Object{policyWith(func(p *sustainv1alpha1.Policy) { p.Spec.RightSizing.RecommendOnly = true }), webRS(), webWLR(withSnapshot)},
			pod:    webPod("p"),
			reason: reasonRecommendOnly,
			source: RecSourceHit,
		},
		{
			name: "the pod already declares the Recommendation",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(withSnapshot)},
			pod: func() *corev1.Pod {
				p := webPod("p")
				p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi"),
				}
				return p
			}(),
			reason: reasonAtRecommendation,
			source: RecSourceHit,
		},
		{
			name:   "injected",
			objs:   []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(withSnapshot)},
			pod:    webPod("p"),
			reason: reasonInjected,
			source: RecSourceHit,
			writes: []string{"app=100m"},
		},
		{
			name: "injected through the namespace's opt-in",
			objs: []client.Object{
				basicPolicy("p", sustainv1alpha1.UpdateModeOngoing), webRS(), webWLR(withSnapshot),
				&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "p"}}},
			},
			pod:    webPod(""),
			reason: reasonInjected,
			source: RecSourceHit,
			writes: []string{"app=100m"},
		},
		{
			name: "a bare pod is injected as its owner-name's Pod identity",
			objs: []client.Object{podPolicy("p"), freshWLR("Pod", "default", "etl-daily", map[string]sustainv1alpha1.ContainerRecommendation{
				"app": wlrRec("100m", "64Mi"),
			})},
			pod:    etlPod("p"),
			reason: reasonInjected,
			source: RecSourceHit,
			stub:   true,
			writes: []string{"app=100m"},
		},
		{
			// Both identities hold a Recommendation; only the overridden one may
			// supply the injected values.
			name: "an owner-name override reads the overridden identity",
			objs: []client.Object{
				basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(),
				webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
					w.Status.Containers["app"] = wlrRec("999m", "999Mi")
					withSnapshot(w)
				}),
				freshWLR("Deployment", "default", "app", map[string]sustainv1alpha1.ContainerRecommendation{"app": wlrRec("100m", "64Mi")}),
			},
			pod: func() *corev1.Pod {
				p := webPod("p")
				p.Annotations[sustainv1alpha1.OwnerNameAnnotation] = "app"
				return p
			}(),
			reason: reasonInjected,
			source: RecSourceHit,
			stub:   true,
			writes: []string{"app=100m"},
		},
		{
			// The winning side of a Conflicted identity keeps the frozen numbers.
			name: "a Conflicted identity's frozen Recommendation, to its own Policy's pod",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status.Outcome = sustainv1alpha1.OutcomeConflicted
				withSnapshot(w)
			})},
			pod:    webPod("p"),
			reason: reasonInjected,
			source: RecSourceRetained,
			writes: []string{"app=100m"},
		},
		{
			// The snapshot is the controller's only container list for a
			// departed identity, and this pod runs one it lacks.
			name: "a departed identity's next run gets the retained Recommendation and adds to its snapshot",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(func(w *sustainv1alpha1.WorkloadRecommendation) {
				w.Status.Departed = true
				w.Status.ObservedResources = map[string]sustainv1alpha1.ObservedContainerResources{"retired": {}}
			})},
			pod:    webPod("p"),
			reason: reasonInjected,
			source: RecSourceRetained,
			stub:   true,
			writes: []string{"app=100m"},
		},
		{
			name: "a template limit below the recommended request is raised to it",
			objs: []client.Object{basicPolicy("p", sustainv1alpha1.UpdateModeOnCreate), webRS(), webWLR(withSnapshot)},
			pod: func() *corev1.Pod {
				p := webPod("p")
				p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}
				return p
			}(),
			reason: reasonInjected,
			source: RecSourceHit,
			writes: []string{"app=100m/100m"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(config.Scheme()).WithObjects(tc.objs...).
				WithStatusSubresource(&sustainv1alpha1.WorkloadRecommendation{}).
				WithInterceptorFuncs(tc.funcs).Build()
			h := &Handler{Client: c, RecommendOnly: tc.recommendOnly, ExcludedNamespaces: tc.excluded}

			a := h.decide(context.Background(), tc.pod)
			if a.reason != tc.reason {
				t.Errorf("reason = %q, want %q (err %v)", a.reason, tc.reason, a.err)
			}
			if a.source != tc.source {
				t.Errorf("source = %q, want %q", a.source, tc.source)
			}
			if got := a.stub != nil; got != tc.stub {
				t.Errorf("stub requested = %v, want %v", got, tc.stub)
			}
			if got := writesOf(a); !slices.Equal(got, tc.writes) {
				t.Errorf("writes = %v, want %v", got, tc.writes)
			}
		})
	}
}

// everyReason is the closed set an admission ends in.
func everyReason() []reason {
	return []reason{
		reason(governance.NoOwner), reason(governance.NoPolicySelects), reason(governance.NotOptedIn),
		reason(governance.OptedOut), reason(governance.PolicyMissing), reason(governance.NamespaceExcluded),
		reason(governance.NamespaceNotSelected), reason(governance.SelectorInvalid), reason(governance.LabelsNotSelected),
		reason(governance.KindNotManaged),
		reason(RecSourceMissing), reason(RecSourceUndecided), reason(RecSourceNoData), reason(RecSourceOtherPolicy),
		reason(RecSourceStale),
		reasonUndecodable, reasonPanic, reasonGovernanceFailed, reasonRecommendationUnreadable,
		reasonNoContainerMatches, reasonRecommendOnly, reasonAtRecommendation, reasonInjected,
	}
}

func patchesOf(t *testing.T, resp *admissionv1.AdmissionResponse) []jsonPatch {
	t.Helper()
	if !resp.Allowed {
		t.Fatal("the webhook must allow every pod")
	}
	if len(resp.Patch) == 0 {
		return nil
	}
	if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatalf("patch type = %v, want JSONPatch", resp.PatchType)
	}
	var patches []jsonPatch
	if err := json.Unmarshal(resp.Patch, &patches); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	return patches
}

// The owner-name label is the user's own grouping request, mirrored whatever
// the admission decided, and only for a value that is a valid label value.
func TestRespondMirrorsTheOwnerNameOnEveryReason(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ownerName string
		labels    map[string]string
		wantPath  string
	}{
		{name: "valid, pod without labels", ownerName: "etl-daily", wantPath: "/metadata/labels"},
		{name: "valid, pod with labels", ownerName: "etl-daily", labels: map[string]string{"app": "etl"}, wantPath: "/metadata/labels/k8s.sustain.io~1owner-name"},
		{name: "invalid label value", ownerName: "Invalid/Value"},
		{name: "already mirrored", ownerName: "etl-daily", labels: map[string]string{sustainv1alpha1.OwnerNameAnnotation: "etl-daily"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, r := range everyReason() {
				pod := barePod(map[string]string{sustainv1alpha1.OwnerNameAnnotation: tc.ownerName})
				pod.Labels = tc.labels
				var paths []string
				for _, p := range patchesOf(t, respond(pod, admission{reason: r})) {
					paths = append(paths, p.Path)
				}
				var want []string
				if tc.wantPath != "" {
					want = []string{tc.wantPath}
				}
				if !slices.Equal(paths, want) {
					t.Errorf("%s: patch paths = %v, want %v", r, paths, want)
				}
			}
		})
	}
}

func TestRespondSetsTheResourcesOfEveryWrittenContainer(t *testing.T) {
	pod := barePod(nil)
	writes := workload.Inject(
		[]corev1.Container{{Name: "app"}},
		[]corev1.Container{{Name: "migrate"}, {Name: "warm-cache"}},
		false,
		map[string]workload.ContainerRecommendation{"app": {CPURequest: qtyp("100m")}, "warm-cache": {CPURequest: qtyp("50m")}},
	).Writes

	patches := patchesOf(t, respond(pod, admission{reason: reasonInjected, writes: writes}))
	got := map[string]string{}
	for _, p := range patches {
		var res corev1.ResourceRequirements
		if err := json.Unmarshal(p.Value, &res); err != nil {
			t.Fatalf("%s: %v", p.Path, err)
		}
		got[p.Path] = res.Requests.Cpu().String()
	}
	want := map[string]string{"/spec/containers/0/resources": "100m", "/spec/initContainers/1/resources": "50m"}
	if !maps.Equal(got, want) {
		t.Errorf("patches = %v, want %v", got, want)
	}
}

func TestAdmitAllowsAPodItCannotDecodeUnmutated(t *testing.T) {
	env := newAdmitEnv(t)
	for name, raw := range map[string][]byte{"empty object": nil, "not a pod": []byte("not json")} {
		resp := env.handler.admit(context.Background(), &admissionv1.AdmissionRequest{
			UID: "uid", Namespace: "default", Name: "n",
			Kind:   metav1.GroupVersionKind{Kind: "Pod"},
			Object: runtime.RawExtension{Raw: raw},
		})
		if patches := patchesOf(t, resp); len(patches) != 0 {
			t.Errorf("%s: patches = %v, want none", name, patches)
		}
	}
}

// A panic deciding an admission must not reach the HTTP middleware, which
// would answer 500 and leave the pod to the failurePolicy without its label.
func TestAdmitTurnsAPanicDecidingIntoAnAllowedPodWithItsLabel(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(config.Scheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				panic("boom")
			},
		}).Build()
	h := &Handler{Client: c}

	before := testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelDecide))
	resp := h.admit(context.Background(), admissionRequestFor(t, etlPod("p")))
	if got := testutil.ToFloat64(PanicTotal.WithLabelValues(panicLabelDecide)) - before; got != 1 {
		t.Errorf("panics counted = %v, want 1", got)
	}
	patches := patchesOf(t, resp)
	if len(patches) != 1 || patches[0].Path != "/metadata/labels" || !strings.Contains(string(patches[0].Value), "etl-daily") {
		t.Errorf("patches = %v, want the owner-name label only", patches)
	}
}
