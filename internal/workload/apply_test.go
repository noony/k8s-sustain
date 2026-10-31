package workload

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

func TestApply_EveryKindHasAnApplyRule(t *testing.T) {
	for _, kind := range SupportedKinds {
		if _, ok := applyRules[kind]; !ok {
			t.Errorf("kind %s has no apply rule: its members would be refused at apply time", kind)
		}
	}
	for kind := range applyRules {
		if !slices.Contains(SupportedKinds, kind) {
			t.Errorf("apply rule for %s, which is not in SupportedKinds", kind)
		}
	}
}

func ownerRef(kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}
}

// kindPod is a Running, Ready pod at 50m CPU, stale against applyRecs.
func kindPod(name string, labels map[string]string, owner *metav1.OwnerReference) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-" + name), Labels: labels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")}},
		}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	if owner != nil {
		pod.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return pod
}

var (
	appLabels     = map[string]string{"app": "test"}
	appContainers = []corev1.Container{{Name: "app"}}
	applyRecs     = map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
)

func jobNamed(name string, uid types.UID, owner *metav1.OwnerReference, finished bool) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: uid}}
	if owner != nil {
		j.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	if finished {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return j
}

func replicaSetOf(name string, owner metav1.OwnerReference) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: name, UID: types.UID("uid-" + name), OwnerReferences: []metav1.OwnerReference{owner},
	}}
}

// kindFixture is a member, the one stale pod it owns ("owned"), and
// bystanders that a careless pod source would pick up: pods sharing its
// labels, pods of a sibling workload, pods of a finished Job.
type kindFixture struct {
	member Member
	objs   []client.Object
}

func kindFixtures() map[string]kindFixture {
	jobLabel := func(job string) map[string]string { return map[string]string{batchv1.JobNameLabel: job} }
	bystander := kindPod("bystander", appLabels, nil)
	byRS := func(kind string, obj client.Object) kindFixture {
		mine := replicaSetOf("web-abc", ownerRef(kind, "web", "web-uid"))
		theirs := replicaSetOf("other-abc", ownerRef(kind, "other", "other-uid"))
		rs := func(r *appsv1.ReplicaSet) *metav1.OwnerReference {
			ref := ownerRef("ReplicaSet", r.Name, r.UID)
			return &ref
		}
		return kindFixture{member: Member{Kind: kind, Object: obj, Containers: appContainers}, objs: []client.Object{
			mine, theirs, bystander,
			kindPod("owned", appLabels, rs(mine)),
			kindPod("sibling", appLabels, rs(theirs)),
		}}
	}
	direct := func(kind string, obj client.Object) kindFixture {
		return kindFixture{member: Member{Kind: kind, Object: obj, Containers: appContainers}, objs: []client.Object{
			bystander,
			kindPod("owned", appLabels, ptr.To(ownerRef(kind, "web", "web-uid"))),
			kindPod("sibling", appLabels, ptr.To(ownerRef(kind, "other", "other-uid"))),
		}}
	}
	meta := metav1.ObjectMeta{Namespace: "default", Name: "web", UID: "web-uid"}
	sel := testSelector()
	cronOwner := ownerRef("CronJob", "web", "web-uid")
	runningJob := jobNamed("web-1", "job-1-uid", &cronOwner, false)
	finishedJob := jobNamed("web-0", "job-0-uid", &cronOwner, true)
	otherJob := jobNamed("other-1", "other-job-uid", ptr.To(ownerRef("CronJob", "other", "other-uid")), false)
	controlled := kindPod("controlled", appLabels, ptr.To(ownerRef("ReplicaSet", "rs", "rs-uid")))
	ownedBare := kindPod("owned", appLabels, nil)
	return map[string]kindFixture{
		"Deployment":  byRS("Deployment", &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Selector: sel}}),
		"Rollout":     byRS("Rollout", &rolloutsv1alpha1.Rollout{ObjectMeta: meta, Spec: rolloutsv1alpha1.RolloutSpec{Selector: sel}}),
		"StatefulSet": direct("StatefulSet", &appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Selector: sel}}),
		"DaemonSet":   direct("DaemonSet", &appsv1.DaemonSet{ObjectMeta: meta, Spec: appsv1.DaemonSetSpec{Selector: sel}}),
		"CronJob": {member: Member{Kind: "CronJob", Object: &batchv1.CronJob{ObjectMeta: meta}, Containers: appContainers}, objs: []client.Object{
			runningJob, finishedJob, otherJob,
			kindPod("owned", jobLabel("web-1"), ptr.To(ownerRef("Job", "web-1", "job-1-uid"))),
			kindPod("finished-run", jobLabel("web-0"), ptr.To(ownerRef("Job", "web-0", "job-0-uid"))),
			kindPod("other-run", jobLabel("other-1"), ptr.To(ownerRef("Job", "other-1", "other-job-uid"))),
			kindPod("forged-label", jobLabel("web-1"), nil),
		}},
		"Job": {member: Member{Kind: "Job", Object: &batchv1.Job{ObjectMeta: meta}, Containers: appContainers}, objs: []client.Object{
			kindPod("owned", jobLabel("web"), ptr.To(ownerRef("Job", "web", "web-uid"))),
			kindPod("forged-label", jobLabel("web"), nil),
		}},
		"Pod": {member: Member{Kind: "Pod", Pods: []*corev1.Pod{ownedBare, controlled}, Containers: appContainers}, objs: []client.Object{
			ownedBare, controlled, bystander,
		}},
	}
}

// podActions records what an Apply did to pods, and any write to anything
// else: a workload spec is never patched.
type podActions struct {
	mu      sync.Mutex
	resized []string
	evicted []string
	writes  []string
}

func (a *podActions) record(list *[]string, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	*list = append(*list, name)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, policyv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func (a *podActions) client(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			if sub == "resize" {
				a.record(&a.resized, obj.GetName())
			}
			return nil
		},
		SubResourceCreate: func(ctx context.Context, inner client.Client, sub string, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
			if sub != "eviction" {
				return nil
			}
			a.record(&a.evicted, obj.GetName())
			return inner.Delete(ctx, obj)
		},
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			a.record(&a.writes, obj.GetName())
			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			a.record(&a.writes, obj.GetName())
			return nil
		},
	}).Build()
}

// Every kind under the three ways a pass runs: which pods it touches, how,
// what it counts, and which family the kind belongs to.
func TestApply_EveryKindUnderEveryMode(t *testing.T) {
	inPlaceOnly := map[string]bool{"CronJob": true, "Job": true, "Pod": true}
	modes := []struct {
		name     string
		inPlace  bool
		dryRun   bool
		resized  func(kind string) []string
		evicted  func(kind string) []string
		wantPods func(kind string) PodCounts
	}{
		{
			name: "Ongoing in-place", inPlace: true,
			resized:  func(string) []string { return []string{"owned"} },
			evicted:  func(string) []string { return nil },
			wantPods: func(string) PodCounts { return PodCounts{Total: 1, Stale: 0} },
		},
		{
			name:    "Ongoing eviction",
			resized: func(string) []string { return nil },
			evicted: func(kind string) []string {
				if inPlaceOnly[kind] {
					return nil
				}
				return []string{"owned"}
			},
			wantPods: func(kind string) PodCounts {
				if inPlaceOnly[kind] {
					return PodCounts{Total: 1, Stale: 1}
				}
				return PodCounts{Total: 1, Stale: 0}
			},
		},
		{
			name: "OnCreate dry run", inPlace: true, dryRun: true,
			resized:  func(string) []string { return nil },
			evicted:  func(string) []string { return nil },
			wantPods: func(string) PodCounts { return PodCounts{Total: 1, Stale: 1} },
		},
	}
	for _, kind := range SupportedKinds {
		if _, ok := kindFixtures()[kind]; !ok {
			t.Errorf("no fixture for kind %s", kind)
			continue
		}
		for _, mode := range modes {
			t.Run(kind+"/"+mode.name, func(t *testing.T) {
				// A pass writes the resources it applied back onto the pods it
				// was handed, so every case starts from fresh ones.
				fx := kindFixtures()[kind]
				var acts podActions
				p := New(acts.client(t, fx.objs...), mode.inPlace, testEvictionOpts()...)

				out := p.Apply(context.Background(), fx.member, applyRecs, ApplySettings{DryRun: mode.dryRun})
				if out.Err != nil {
					t.Fatalf("Apply: %v", out.Err)
				}
				if !slices.Equal(acts.resized, mode.resized(kind)) {
					t.Errorf("resized %v, want %v", acts.resized, mode.resized(kind))
				}
				if !slices.Equal(acts.evicted, mode.evicted(kind)) {
					t.Errorf("evicted %v, want %v", acts.evicted, mode.evicted(kind))
				}
				if len(acts.writes) > 0 {
					t.Errorf("patched or updated %v: apply never writes a workload object", acts.writes)
				}
				if !slices.Equal(out.Resized, mode.resized(kind)) || !slices.Equal(out.Evicted, mode.evicted(kind)) {
					t.Errorf("result resized %v evicted %v, want what was done to the pods", out.Resized, out.Evicted)
				}
				wantContainers := []string{"app"}
				if out.Changed() == 0 {
					wantContainers = nil
				}
				if !slices.Equal(out.Containers, wantContainers) {
					t.Errorf("containers = %v, want %v", out.Containers, wantContainers)
				}
				if !out.Counted || out.Pods != mode.wantPods(kind) {
					t.Errorf("pods = %+v (counted %v), want %+v", out.Pods, out.Counted, mode.wantPods(kind))
				}
				wantKind := ResultDone
				if mode.dryRun {
					wantKind = ResultCountedOnly
				}
				if out.Kind != wantKind {
					t.Errorf("kind = %v, want %v", out.Kind, wantKind)
				}
			})
		}
	}
}

// The UID is the only thing telling a member's pods from a bystander's: a
// member without one is refused rather than applied to every labelled pod.
func TestApply_RefusesMemberWithoutUID(t *testing.T) {
	for _, kind := range SupportedKinds {
		t.Run(kind, func(t *testing.T) {
			fx := kindFixtures()[kind]
			m := fx.member
			if kind == "Pod" {
				pod := m.Pods[0].DeepCopy()
				pod.UID = ""
				m.Pods = []*corev1.Pod{pod}
			} else {
				obj := m.Object.DeepCopyObject().(client.Object)
				obj.SetUID("")
				m.Object = obj
			}
			var acts podActions
			p := New(acts.client(t, fx.objs...), true)
			out := p.Apply(context.Background(), m, applyRecs, ApplySettings{})
			if !errors.Is(out.Err, ErrRefused) {
				t.Fatal("expected a member without UID to be refused")
			}
			if len(acts.resized)+len(acts.evicted) > 0 {
				t.Errorf("touched pods of a member without UID: resized %v, evicted %v", acts.resized, acts.evicted)
			}
			if out.Changed() != 0 {
				t.Errorf("changed = %d, want 0", out.Changed())
			}
		})
	}
}

// LabelSelectorAsSelector(nil) selects nothing: applying through it would
// report a converged member whose pods were never looked at.
func TestApply_RefusesWorkloadWithoutSelector(t *testing.T) {
	var acts podActions
	p := New(acts.client(t, cpuPod("a", "50m")), true)
	m := testMember()
	m.Object.(*appsv1.DaemonSet).Spec.Selector = nil
	if err := p.Apply(context.Background(), m, applyRecs, ApplySettings{}).Err; !errors.Is(err, ErrRefused) {
		t.Fatal("expected a workload without selector to be refused")
	}
}

func TestApply_RefusesKindWithoutRule(t *testing.T) {
	var acts podActions
	p := New(acts.client(t), true)
	m := Member{Kind: "ReplicaSet", Object: &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", UID: "rs-uid"}}, Containers: appContainers}
	if err := p.Apply(context.Background(), m, applyRecs, ApplySettings{}).Err; !errors.Is(err, ErrRefused) {
		t.Fatal("expected a kind without apply rule to be refused")
	}
}

func TestApply_RefusesObjectOfAnotherKind(t *testing.T) {
	var acts podActions
	p := New(acts.client(t), true)
	m := Member{Kind: "CronJob", Object: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", UID: "j-uid"}}, Containers: appContainers}
	if err := p.Apply(context.Background(), m, applyRecs, ApplySettings{}).Err; !errors.Is(err, ErrRefused) {
		t.Fatal("expected a CronJob member carrying a Job to be refused")
	}
}

// A bare-pod identity's members are the pods the inventory found. Listing the
// namespace again would cost a cache-wide pod List per identity per cycle.
func TestApply_BarePodsAreNotListed(t *testing.T) {
	a := kindPod("etl-run-1", nil, nil)
	b := kindPod("etl-run-2", nil, nil)
	var acts podActions
	c := interceptor.NewClient(acts.client(t, a, b).(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.PodList); ok {
				return errors.New("bare pods must not be listed")
			}
			return inner.List(ctx, list, opts...)
		},
	})
	p := New(c, true)
	out := p.Apply(context.Background(), Member{Kind: "Pod", Pods: []*corev1.Pod{a, b}, Containers: appContainers}, applyRecs, ApplySettings{})
	if out.Err != nil {
		t.Fatalf("Apply: %v", out.Err)
	}
	if out.Changed() != 2 || !slices.Equal(acts.resized, []string{"etl-run-1", "etl-run-2"}) {
		t.Errorf("changed = %d, resized %v, want both members resized", out.Changed(), acts.resized)
	}
}

// The common shape of job-like kinds between runs: a member with no pod to
// apply to is converged, not an error.
func TestApply_InPlaceOnlyMemberWithoutPods(t *testing.T) {
	finishedRun := jobNamed("web-0", "job-0-uid", ptr.To(ownerRef("CronJob", "web", "web-uid")), true)
	meta := metav1.ObjectMeta{Namespace: "default", Name: "web", UID: "web-uid"}
	for _, m := range []Member{
		{Kind: "CronJob", Object: &batchv1.CronJob{ObjectMeta: meta}, Containers: appContainers},
		{Kind: "Job", Object: &batchv1.Job{ObjectMeta: meta}, Containers: appContainers},
		{Kind: "Pod", Containers: appContainers},
	} {
		t.Run(m.Kind, func(t *testing.T) {
			var acts podActions
			p := New(acts.client(t, finishedRun), true)
			out := p.Apply(context.Background(), m, applyRecs, ApplySettings{})
			if out.Kind != ResultDone || out.Changed() != 0 || out.Pods != (PodCounts{}) {
				t.Errorf("result = %+v, want a converged member with nothing changed and no pod counted", out)
			}
		})
	}
}

// A Pending pod cannot be resized in place. One stuck on an oversized request
// is evicted so the webhook re-injects the replacement, unless its kind is
// in-place-only: there it waits for the next run, as it would without in-place
// support.
func TestApply_InPlace_PendingPod(t *testing.T) {
	inPlaceOnly := map[string]bool{"CronJob": true, "Job": true, "Pod": true}
	for _, kind := range SupportedKinds {
		t.Run(kind, func(t *testing.T) {
			fx := kindFixtures()[kind]
			for _, o := range fx.objs {
				if pod, ok := o.(*corev1.Pod); ok && pod.Name == "owned" {
					pod.Status.Phase = corev1.PodPending
					pod.Status.Conditions = nil
				}
			}
			var acts podActions
			p := New(acts.client(t, fx.objs...), true, testEvictionOpts()...)

			out := p.Apply(context.Background(), fx.member, applyRecs, ApplySettings{})
			if out.Err != nil {
				t.Fatalf("Apply: %v", out.Err)
			}
			if len(acts.resized) != 0 {
				t.Errorf("resized %v: a Pending pod cannot be resized in place", acts.resized)
			}
			wantEvicted := []string{"owned"}
			if inPlaceOnly[kind] {
				wantEvicted = nil
			}
			if !slices.Equal(acts.evicted, wantEvicted) {
				t.Errorf("evicted %v, want %v", acts.evicted, wantEvicted)
			}
			if out.Changed() != len(wantEvicted) {
				t.Errorf("changed = %d, want %d", out.Changed(), len(wantEvicted))
			}
		})
	}
}

func TestApplySettingsFor(t *testing.T) {
	ongoing, onCreate := sustainv1alpha1.UpdateModeOngoing, sustainv1alpha1.UpdateModeOnCreate
	defaults := Tolerance{CPUPercent: 5, CPUFloor: resource.MustParse("10m"), MemPercent: 5, MemFloor: resource.MustParse("15Mi")}
	cases := []struct {
		name   string
		kind   string
		flag   bool
		mutate func(*sustainv1alpha1.RightSizingSpec)
		want   ApplySettings
	}{
		{name: "Ongoing touches running pods", kind: "Deployment", want: ApplySettings{Tolerance: defaults}},
		{name: "OnCreate is a dry run", kind: "StatefulSet", want: ApplySettings{DryRun: true, Tolerance: defaults}},
		{name: "a kind the Policy does not manage is a dry run", kind: "DaemonSet", want: ApplySettings{DryRun: true, Tolerance: defaults}},
		{name: "the flag is recommend-only", kind: "Deployment", flag: true, want: ApplySettings{RecommendOnly: true, Tolerance: defaults}},
		{
			name: "the Policy's field is recommend-only", kind: "Deployment",
			mutate: func(rs *sustainv1alpha1.RightSizingSpec) { rs.RecommendOnly = true },
			want:   ApplySettings{RecommendOnly: true, Tolerance: defaults},
		},
		{
			name: "the safe-to-evict override", kind: "Deployment",
			mutate: func(rs *sustainv1alpha1.RightSizingSpec) {
				rs.Update.Eviction.IgnoreAutoscalerSafeToEvictAnnotations = true
			},
			want: ApplySettings{IgnoreSafeToEvict: true, Tolerance: defaults},
		},
		{
			name: "a threshold overrides its resource's defaults only", kind: "Deployment",
			mutate: func(rs *sustainv1alpha1.RightSizingSpec) {
				rs.ResourcesConfigs.CPU.DownsizeThreshold = &sustainv1alpha1.DownsizeThreshold{Percent: ptr.To[int32](20), MinDecrease: ptr.To(resource.MustParse("50m"))}
			},
			want: ApplySettings{Tolerance: Tolerance{CPUPercent: 20, CPUFloor: resource.MustParse("50m"), MemPercent: 5, MemFloor: resource.MustParse("15Mi")}},
		},
		{
			name: "an unset half of a threshold keeps its default", kind: "Deployment",
			mutate: func(rs *sustainv1alpha1.RightSizingSpec) {
				rs.ResourcesConfigs.CPU.DownsizeThreshold = &sustainv1alpha1.DownsizeThreshold{Percent: ptr.To[int32](30)}
				rs.ResourcesConfigs.Memory.DownsizeThreshold = &sustainv1alpha1.DownsizeThreshold{MinDecrease: ptr.To(resource.MustParse("40Mi"))}
			},
			want: ApplySettings{Tolerance: Tolerance{CPUPercent: 30, CPUFloor: resource.MustParse("10m"), MemPercent: 5, MemFloor: resource.MustParse("40Mi")}},
		},
		{
			name: "zero thresholds act on every decrease", kind: "Deployment",
			mutate: func(rs *sustainv1alpha1.RightSizingSpec) {
				zero := &sustainv1alpha1.DownsizeThreshold{Percent: ptr.To[int32](0), MinDecrease: ptr.To(resource.MustParse("0"))}
				rs.ResourcesConfigs.CPU.DownsizeThreshold, rs.ResourcesConfigs.Memory.DownsizeThreshold = zero, zero
			},
			want: ApplySettings{Tolerance: Tolerance{CPUFloor: resource.MustParse("0"), MemFloor: resource.MustParse("0")}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := &sustainv1alpha1.Policy{Spec: sustainv1alpha1.PolicySpec{RightSizing: sustainv1alpha1.RightSizingSpec{
				Update: sustainv1alpha1.UpdateSpec{Types: sustainv1alpha1.UpdateTypes{Deployment: &ongoing, StatefulSet: &onCreate}},
			}}}
			if tc.mutate != nil {
				tc.mutate(&policy.Spec.RightSizing)
			}
			got := ApplySettingsFor(policy, tc.kind, tc.flag)
			if got.RecommendOnly != tc.want.RecommendOnly || got.DryRun != tc.want.DryRun || got.IgnoreSafeToEvict != tc.want.IgnoreSafeToEvict {
				t.Errorf("settings = %+v, want %+v", got, tc.want)
			}
			g, w := got.Tolerance, tc.want.Tolerance
			if g.CPUPercent != w.CPUPercent || g.MemPercent != w.MemPercent || g.CPUFloor.Cmp(w.CPUFloor) != 0 || g.MemFloor.Cmp(w.MemFloor) != 0 {
				t.Errorf("tolerance = %+v, want %+v", g, w)
			}
		})
	}
}

// Nothing to apply reads no pod: recommend-only, or a Recommendation that
// names none of the member's containers.
func TestApply_NothingToApplyIsIdle(t *testing.T) {
	for _, tc := range []struct {
		name string
		recs map[string]ContainerRecommendation
		s    ApplySettings
	}{
		{"recommend-only", applyRecs, ApplySettings{RecommendOnly: true}},
		{"no Recommendation for the member's containers", map[string]ContainerRecommendation{"other": {CPURequest: qtyp("200m")}}, ApplySettings{}},
		{"no Recommendation at all", nil, ApplySettings{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var acts podActions
			c := interceptor.NewClient(acts.client(t, cpuPod("a", "50m")).(client.WithWatch), interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("no pod may be read")
				},
			})
			out := New(c, true).Apply(context.Background(), testMember(), tc.recs, tc.s)
			if out.Kind != ResultIdle || out.Err != nil || out.Counted {
				t.Errorf("result = %+v, want idle, uncounted", out)
			}
		})
	}
}

// A dry run touches no pod, so failing to list them is no failure: it counts
// nothing, and the caller keeps the counts it had.
func TestApply_DryRunThatCannotListCountsNothing(t *testing.T) {
	var acts podActions
	c := interceptor.NewClient(acts.client(t, cpuPod("a", "50m")).(client.WithWatch), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewServiceUnavailable("apiserver unavailable")
		},
	})
	out := New(c, true).Apply(context.Background(), testMember(), applyRecs, ApplySettings{DryRun: true})
	if out.Kind != ResultCountedOnly || out.Err != nil || out.Counted {
		t.Errorf("result = %+v, want counted-only without counts or error", out)
	}
}

// A failed apply names its step by the kind's family: patch for kinds that
// evict, resize for in-place-only ones.
func TestApply_FailureNamesItsStep(t *testing.T) {
	unavailable := apierrors.NewServiceUnavailable("apiserver unavailable")
	failPods := interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return unavailable
		},
		SubResourceCreate: func(context.Context, client.Client, string, client.Object, client.Object, ...client.SubResourceCreateOption) error {
			return unavailable
		},
	}
	for _, tc := range []struct {
		kind    string
		inPlace bool
		want    Step
	}{
		{"Deployment", false, StepPatch},
		{"StatefulSet", true, StepPatch},
		{"Job", true, StepResize},
		{"Pod", true, StepResize},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			fx := kindFixtures()[tc.kind]
			c := interceptor.NewClient(fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(fx.objs...).Build(), failPods)
			out := New(c, tc.inPlace, testEvictionOpts()...).Apply(context.Background(), fx.member, applyRecs, ApplySettings{})
			if out.Kind != ResultFailed || out.Step != tc.want || !errors.Is(out.Err, unavailable) {
				t.Errorf("result = %+v, want failed on %s", out, tc.want)
			}
		})
	}
	t.Run("kind without a rule", func(t *testing.T) {
		m := Member{Kind: "ReplicaSet", Object: &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", UID: "rs-uid"}}, Containers: appContainers}
		if out := New(fake.NewClientBuilder().Build(), true).Apply(context.Background(), m, applyRecs, ApplySettings{}); out.Step != StepPatch {
			t.Errorf("step = %q, want %q", out.Step, StepPatch)
		}
	})
}

// The changed containers are what the pass did to each pod, not a guess from
// the template: a container already at its Recommendation on every pod is not
// one, and the union runs across pods.
func TestApply_ContainersAreWhatChangedOnThePods(t *testing.T) {
	twoContainers := func(name, app, side string) *corev1.Pod {
		pod := runningPod(name, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(app)})
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name:      "side",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(side)}},
		})
		return pod
	}
	recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}, "side": {CPURequest: qtyp("20m")}}
	for _, tc := range []struct {
		name    string
		inPlace bool
		pods    []*corev1.Pod
		want    []string
	}{
		{"one container stale, resized", true, []*corev1.Pod{twoContainers("a", "50m", "20m")}, []string{"app"}},
		{"one container stale on each pod", true, []*corev1.Pod{twoContainers("a", "50m", "20m"), twoContainers("b", "200m", "40m")}, []string{"app", "side"}},
		{"nothing stale", true, []*corev1.Pod{twoContainers("a", "200m", "20m")}, nil},
		{"one container stale, evicted", false, []*corev1.Pod{twoContainers("a", "50m", "20m")}, []string{"app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, len(tc.pods))
			for i, pod := range tc.pods {
				objs[i] = pod
			}
			var acts podActions
			p := New(acts.client(t, objs...), tc.inPlace, testEvictionOpts()...)
			out := p.Apply(context.Background(), declaring(testMember(), tc.pods[0]), recs, ApplySettings{})
			if out.Err != nil {
				t.Fatalf("Apply: %v", out.Err)
			}
			if !slices.Equal(out.Containers, tc.want) {
				t.Errorf("containers = %v, want %v", out.Containers, tc.want)
			}
		})
	}
}

// A change to limits alone never evicts a pod (docs/adr/0009): it is resized
// in place where the cluster supports it, and otherwise waits for the pod's
// replacement, which the webhook gives the new limits. Either way the pod is
// not stale. A limit the guard raises with a rising request belongs to a
// request change, applied as any other.
func TestApply_LimitsAloneNeverEvict(t *testing.T) {
	limitsAlone := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("100m"), CPULimit: qtyp("400m")}}
	invalid := apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "p", nil)
	for _, tc := range []struct {
		name         string
		member       Member
		inPlace      bool
		pending      bool
		rejectResize bool
		recs         map[string]ContainerRecommendation
		wantResized  []string
		wantEvicted  []string
		wantStale    int
	}{
		{name: "limits alone, resized in place", member: testMember(), inPlace: true, recs: limitsAlone, wantResized: []string{"p"}},
		{name: "limits alone without in-place resize", member: testMember(), recs: limitsAlone},
		{name: "limits alone on a Pending pod", member: testMember(), inPlace: true, pending: true, recs: limitsAlone},
		{name: "limits alone, resize rejected", member: testMember(), inPlace: true, rejectResize: true, recs: limitsAlone},
		{
			name: "limits alone beside a request decrease the tolerance withholds", member: testMember(),
			recs: map[string]ContainerRecommendation{"app": {CPURequest: qtyp("98m"), CPULimit: qtyp("400m")}},
		},
		{name: "limits alone on an in-place-only kind without in-place resize", member: jobMember(), recs: limitsAlone},
		{
			name: "a request rising past the limit the guard raises, evicted", member: testMember(),
			recs: map[string]ContainerRecommendation{"app": {CPURequest: qtyp("300m")}}, wantEvicted: []string{"p"},
		},
		{
			name: "a request rising past the limit the guard raises, resized", member: testMember(), inPlace: true,
			recs: map[string]ContainerRecommendation{"app": {CPURequest: qtyp("300m")}}, wantResized: []string{"p"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newPod := runningPod
			if tc.member.Kind == "Job" {
				newPod = jobPod
			}
			pod := newPod("p", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")})
			pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}
			if tc.pending {
				pod.Status.Phase, pod.Status.Conditions = corev1.PodPending, nil
			}
			var acts podActions
			c := acts.client(t, pod)
			if tc.rejectResize {
				c = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
					SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
						return invalid
					},
				})
			}

			out := New(c, tc.inPlace, testEvictionOpts()...).Apply(context.Background(), tc.member, tc.recs, ApplySettings{Tolerance: tol5})

			if out.Kind != ResultDone {
				t.Fatalf("result = %+v, want done", out)
			}
			if !slices.Equal(acts.resized, tc.wantResized) || !slices.Equal(acts.evicted, tc.wantEvicted) {
				t.Errorf("resized %v evicted %v, want resized %v evicted %v", acts.resized, acts.evicted, tc.wantResized, tc.wantEvicted)
			}
			if out.Pods != (PodCounts{Total: 1, Stale: tc.wantStale}) {
				t.Errorf("counts = %+v, want {1 %d}", out.Pods, tc.wantStale)
			}
		})
	}
}

// A staged resize the kubelet cannot enact is evicted only when it changes a
// request: the spec already carries the target, so the resources the kubelet
// reports it enacted tell what is at stake.
func TestApply_StagedResizeOfLimitsAloneIsNotEvicted(t *testing.T) {
	enacted := func(request, limit string) *corev1.ResourceRequirements {
		return &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(request)},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limit)},
		}
	}
	for _, tc := range []struct {
		name        string
		running     *corev1.ResourceRequirements
		wantEvicted []string
	}{
		{name: "limits alone", running: enacted("100m", "200m")},
		{name: "a request", running: enacted("50m", "200m"), wantEvicted: []string{"p"}},
		{name: "nothing reported", wantEvicted: []string{"p"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := withResizeErrorCondition(runningPod("p", corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}))
			pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Resources: tc.running}}
			var acts podActions
			p := New(acts.client(t, pod), true, testEvictionOpts()...)
			recs := map[string]ContainerRecommendation{"app": {CPURequest: qtyp("100m"), CPULimit: qtyp("400m")}}

			if out := p.Apply(context.Background(), testMember(), recs, ApplySettings{}); out.Err != nil {
				t.Fatalf("Apply: %v", out.Err)
			}
			if !slices.Equal(acts.evicted, tc.wantEvicted) {
				t.Errorf("evicted %v, want %v", acts.evicted, tc.wantEvicted)
			}
		})
	}
}
