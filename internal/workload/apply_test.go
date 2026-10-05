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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	appLabels = map[string]string{"app": "test"}
	applyRecs = map[string]ContainerRecommendation{"app": {CPURequest: qtyp("200m")}}
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
		return kindFixture{member: Member{Kind: kind, Object: obj}, objs: []client.Object{
			mine, theirs, bystander,
			kindPod("owned", appLabels, rs(mine)),
			kindPod("sibling", appLabels, rs(theirs)),
		}}
	}
	direct := func(kind string, obj client.Object) kindFixture {
		return kindFixture{member: Member{Kind: kind, Object: obj}, objs: []client.Object{
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
		"CronJob": {member: Member{Kind: "CronJob", Object: &batchv1.CronJob{ObjectMeta: meta}}, objs: []client.Object{
			runningJob, finishedJob, otherJob,
			kindPod("owned", jobLabel("web-1"), ptr.To(ownerRef("Job", "web-1", "job-1-uid"))),
			kindPod("finished-run", jobLabel("web-0"), ptr.To(ownerRef("Job", "web-0", "job-0-uid"))),
			kindPod("other-run", jobLabel("other-1"), ptr.To(ownerRef("Job", "other-1", "other-job-uid"))),
			kindPod("forged-label", jobLabel("web-1"), nil),
		}},
		"Job": {member: Member{Kind: "Job", Object: &batchv1.Job{ObjectMeta: meta}}, objs: []client.Object{
			kindPod("owned", jobLabel("web"), ptr.To(ownerRef("Job", "web", "web-uid"))),
			kindPod("forged-label", jobLabel("web"), nil),
		}},
		"Pod": {member: Member{Kind: "Pod", Pods: []*corev1.Pod{ownedBare, controlled}}, objs: []client.Object{
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

func (a *podActions) client(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, policyv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
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

				out, err := p.Apply(context.Background(), fx.member, applyRecs, ApplySettings{DryRun: mode.dryRun})
				if err != nil {
					t.Fatalf("Apply: %v", err)
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
				if want := len(mode.resized(kind)) + len(mode.evicted(kind)); out.Changed != want {
					t.Errorf("changed = %d, want %d", out.Changed, want)
				}
				if out.Pods != mode.wantPods(kind) {
					t.Errorf("pods = %+v, want %+v", out.Pods, mode.wantPods(kind))
				}
				if out.InPlaceOnly != inPlaceOnly[kind] {
					t.Errorf("in-place-only = %v, want %v", out.InPlaceOnly, inPlaceOnly[kind])
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
			out, err := p.Apply(context.Background(), m, applyRecs, ApplySettings{})
			if err == nil {
				t.Fatal("expected a member without UID to be refused")
			}
			if len(acts.resized)+len(acts.evicted) > 0 {
				t.Errorf("touched pods of a member without UID: resized %v, evicted %v", acts.resized, acts.evicted)
			}
			if out.Changed != 0 {
				t.Errorf("changed = %d, want 0", out.Changed)
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
	if _, err := p.Apply(context.Background(), m, applyRecs, ApplySettings{}); err == nil {
		t.Fatal("expected a workload without selector to be refused")
	}
}

func TestApply_RefusesKindWithoutRule(t *testing.T) {
	var acts podActions
	p := New(acts.client(t), true)
	m := Member{Kind: "ReplicaSet", Object: &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", UID: "rs-uid"}}}
	if _, err := p.Apply(context.Background(), m, applyRecs, ApplySettings{}); err == nil {
		t.Fatal("expected a kind without apply rule to be refused")
	}
}

func TestApply_RefusesObjectOfAnotherKind(t *testing.T) {
	var acts podActions
	p := New(acts.client(t), true)
	m := Member{Kind: "CronJob", Object: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", UID: "j-uid"}}}
	if _, err := p.Apply(context.Background(), m, applyRecs, ApplySettings{}); err == nil {
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
	out, err := p.Apply(context.Background(), Member{Kind: "Pod", Pods: []*corev1.Pod{a, b}}, applyRecs, ApplySettings{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Changed != 2 || !slices.Equal(acts.resized, []string{"etl-run-1", "etl-run-2"}) {
		t.Errorf("changed = %d, resized %v, want both members resized", out.Changed, acts.resized)
	}
}

// The common shape of job-like kinds between runs: a member with no pod to
// apply to is converged, not an error.
func TestApply_InPlaceOnlyMemberWithoutPods(t *testing.T) {
	finishedRun := jobNamed("web-0", "job-0-uid", ptr.To(ownerRef("CronJob", "web", "web-uid")), true)
	meta := metav1.ObjectMeta{Namespace: "default", Name: "web", UID: "web-uid"}
	for _, m := range []Member{
		{Kind: "CronJob", Object: &batchv1.CronJob{ObjectMeta: meta}},
		{Kind: "Job", Object: &batchv1.Job{ObjectMeta: meta}},
		{Kind: "Pod"},
	} {
		t.Run(m.Kind, func(t *testing.T) {
			var acts podActions
			p := New(acts.client(t, finishedRun), true)
			out, err := p.Apply(context.Background(), m, applyRecs, ApplySettings{})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !out.InPlaceOnly || out.Changed != 0 || out.Pods != (PodCounts{}) {
				t.Errorf("outcome = %+v, want an in-place-only member with nothing changed or counted", out)
			}
		})
	}
}
