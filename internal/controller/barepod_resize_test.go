package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
	"github.com/noony/k8s-sustain/internal/workload"
)

// barePod builds a running bare pod opted into policy "p" under the
// owner-name identity ownerName, with the owner-name label the webhook
// mirrors at admission (so a label-selector implementation would also find
// it — see TestResizeBarePods_SkipsControlledPod).
func barePod(ns, name, ownerName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Annotations: map[string]string{
				sustainv1alpha1.PolicyAnnotation:    "p",
				sustainv1alpha1.OwnerNameAnnotation: ownerName,
			},
			Labels: map[string]string{sustainv1alpha1.OwnerNameAnnotation: ownerName},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "worker",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("999m")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// barePodTarget builds the target the way listBarePodTargets does — by running
// workload.GroupBarePods over the namespace's pods and taking the group for
// ownerName — so the membership carried on the target is produced by the
// production rule rather than hand-assembled. pods is what the namespace holds;
// pods belonging to other identities, or disqualified by the grouping rule
// (controller ownerRef, missing annotation), are passed in too and must not end
// up as members.
//
// An identity with no live pods yields a target with no members, which is the
// common bare-pod shape between runs.
func barePodTarget(ns, ownerName string, pods ...*corev1.Pod) *workloadTarget {
	items := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		items = append(items, *p)
	}
	t := &workloadTarget{
		Kind:         "Pod",
		Name:         ownerName,
		Namespace:    ns,
		IdentityKind: "Pod",
		IdentityName: ownerName,
		PolicyName:   "p",
	}
	for _, g := range workload.GroupBarePods(items, nil) {
		if g.Namespace == ns && g.Name == ownerName {
			t.Containers = g.Containers
			t.InitContainers = g.InitContainers
			t.Object = g.Representative
			t.BarePodMembers = g.Members
			break
		}
	}
	return t
}

// resizeRecorder wraps a fake client that records which pods received a
// /resize subresource patch and whether anything was ever evicted.
type resizeRecorder struct {
	resized map[string]bool
	evicted bool
}

func newResizeRecorderClient(t *testing.T, r *PolicyReconciler, objs ...client.Object) *resizeRecorder {
	t.Helper()
	rec := &resizeRecorder{resized: map[string]bool{}}
	r.Client = fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithStatusSubresource(&sustainv1alpha1.Policy{}, &sustainv1alpha1.WorkloadRecommendation{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					if _, ok := obj.(*corev1.Pod); ok {
						rec.resized[obj.GetName()] = true
					}
				}
				return nil
			},
			SubResourceCreate: func(_ context.Context, _ client.Client, sub string, _ client.Object, _ client.Object, _ ...client.SubResourceCreateOption) error {
				if sub == "eviction" {
					rec.evicted = true
				}
				return nil
			},
		}).
		Build()
	r.patcher = workload.New(r.Client, true /* in-place */)
	return rec
}

// TestResizeBarePods_SkipsControlledPod is the ownerRef guard: a pod carrying
// the mirrored owner-name label but owned by a ReplicaSet is not a member of
// the bare-pod identity and must never be resized by this path. Membership
// comes from workload.GroupBarePods precisely so that a shared label cannot
// pull a controlled pod in.
func TestResizeBarePods_SkipsControlledPod(t *testing.T) {
	member := barePod("ns", "member", "dag-task")
	impostor := member.DeepCopy()
	impostor.Name = "impostor"
	impostor.OwnerReferences = []metav1.OwnerReference{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "u", Controller: ptr.To(true)},
	}

	r := makeReconciler(t)
	rec := newResizeRecorderClient(t, r, member, impostor)

	target := barePodTarget("ns", "dag-task", member, impostor)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("200m")}}

	resized, err := r.resizeBarePods(context.Background(), target, recs, workload.Tolerance{}, func(string) {})
	if err != nil {
		t.Fatalf("resizeBarePods: %v", err)
	}
	if resized != 1 {
		t.Errorf("resized %d pods, want 1: the controlled pod must be left alone", resized)
	}
	if rec.resized["impostor"] {
		t.Error("a pod with a controller ownerRef must never be resized by the bare-pod path")
	}
	if !rec.resized["member"] {
		t.Error("expected the bare-pod group member to be resized")
	}
	if rec.evicted {
		t.Error("bare pods must never be evicted")
	}
}

// TestResizeBarePods_ResizesEveryMemberNeverEvicts verifies every live pod of
// the identity is corrected, not just the representative, and that no other
// identity in the namespace is touched.
func TestResizeBarePods_ResizesEveryMemberNeverEvicts(t *testing.T) {
	a := barePod("airflow", "etl-run-1", "etl-daily")
	b := barePod("airflow", "etl-run-2", "etl-daily")
	other := barePod("airflow", "other-run-1", "other-task")

	r := makeReconciler(t)
	rec := newResizeRecorderClient(t, r, a, b, other)

	target := barePodTarget("airflow", "etl-daily", a, b, other)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("200m")}}

	resized, err := r.resizeBarePods(context.Background(), target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeBarePods: %v", err)
	}
	if resized != 2 {
		t.Errorf("resized = %d, want 2 (both members of the identity)", resized)
	}
	if !rec.resized["etl-run-1"] || !rec.resized["etl-run-2"] {
		t.Errorf("expected both members resized, got %v", rec.resized)
	}
	if rec.resized["other-run-1"] {
		t.Error("a pod belonging to a different owner-name identity must not be resized")
	}
	if rec.evicted {
		t.Error("bare pods must never be evicted")
	}
}

// TestResizeBarePods_ZeroWhenNoInPlaceSupport pins the k8s < 1.33 behaviour:
// the whole path is a no-op there, so bare pods stay exactly as untouched as
// they were before in-place resize existed.
func TestResizeBarePods_ZeroWhenNoInPlaceSupport(t *testing.T) {
	pod := barePod("airflow", "etl-run-1", "etl-daily")
	r := makeReconciler(t, pod)
	r.patcher = workload.New(r.Client, false /* no in-place */)

	target := barePodTarget("airflow", "etl-daily", pod)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("200m")}}

	resized, err := r.resizeBarePods(context.Background(), target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeBarePods: %v", err)
	}
	if resized != 0 {
		t.Errorf("resized = %d, want 0 without in-place support", resized)
	}
}

// TestResizeBarePods_NoLivePodsForIdentity covers the common bare-pod shape:
// the identity is known from its cached recommendation but no pod of it is
// currently running.
func TestResizeBarePods_NoLivePodsForIdentity(t *testing.T) {
	other := barePod("airflow", "other-run-1", "other-task")
	r := makeReconciler(t, other)
	r.patcher = workload.New(r.Client, true /* in-place */)

	target := barePodTarget("airflow", "etl-daily", other)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("200m")}}

	resized, err := r.resizeBarePods(context.Background(), target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeBarePods: %v", err)
	}
	if resized != 0 {
		t.Errorf("resized = %d, want 0 (no live pods for the identity)", resized)
	}
}

// TestResizeBarePods_DoesNotReListTheNamespace pins the grouping to the
// listing phase. Re-deriving membership here meant one namespace-wide,
// cache-backed pod List per bare-pod identity per cycle — issued concurrently
// under the errgroup, each deep-copying every pod — to recompute what
// listBarePodTargets had already computed once. An Airflow namespace with
// hundreds of pods and dozens of DAG-task identities paid it M times over.
//
// A List interceptor that always fails makes the regression loud: with the
// members carried on the target no List is needed, so the resize succeeds.
func TestResizeBarePods_DoesNotReListTheNamespace(t *testing.T) {
	pod := barePod("airflow", "etl-run-1", "etl-daily")
	target := barePodTarget("airflow", "etl-daily", pod)

	r := makeReconciler(t)
	rec := &resizeRecorder{resized: map[string]bool{}}
	r.Client = fake.NewClientBuilder().
		WithScheme(r.Scheme).
		WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return errors.New("resizeBarePods must not List pods: membership comes from the target")
				}
				return nil
			},
			SubResourcePatch: func(_ context.Context, _ client.Client, sub string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if sub == "resize" {
					rec.resized[obj.GetName()] = true
				}
				return nil
			},
		}).
		Build()
	r.patcher = workload.New(r.Client, true /* in-place */)

	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("200m")}}
	resized, err := r.resizeBarePods(context.Background(), target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeBarePods: %v", err)
	}
	if resized != 1 || !rec.resized["etl-run-1"] {
		t.Errorf("resized = %d (%v), want the single member resized without any pod List", resized, rec.resized)
	}
}

// TestReconcileWorkload_BarePodOngoing_ResizesRunningPod is the end-to-end
// dispatch check: an Ongoing bare-pod identity reaches the in-place resize
// path and never the eviction path.
func TestReconcileWorkload_BarePodOngoing_ResizesRunningPod(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	pod := barePod("airflow", "etl-run-1", "etl-daily")
	pod.Spec.Containers[0].Name = "app" // the mock Prometheus reports on "app"

	r := reconcilerWithProm(t, server, true /* in-place */)
	rec := newResizeRecorderClient(t, r, pod)

	target := barePodTarget("airflow", "etl-daily", pod)
	target.UpdateMode = sustainv1alpha1.UpdateModeOngoing
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(target)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}
	if !rec.resized["etl-run-1"] {
		t.Error("expected the running bare pod to be resized in place under Ongoing")
	}
	if rec.evicted {
		t.Error("bare pods must never be evicted")
	}
}

// TestReconcileWorkload_BarePodOnCreate_NeverResizes pins the mode
// distinction: the OnCreate early return sits above the bare-pod branch, so
// an OnCreate identity is computed and cached but never resized.
func TestReconcileWorkload_BarePodOnCreate_NeverResizes(t *testing.T) {
	server := promServerForReconcile(t)
	defer server.Close()

	pod := barePod("airflow", "etl-run-1", "etl-daily")
	pod.Spec.Containers[0].Name = "app"

	r := reconcilerWithProm(t, server, true /* in-place */)
	rec := newResizeRecorderClient(t, r, pod)

	target := barePodTarget("airflow", "etl-daily", pod)
	target.UpdateMode = sustainv1alpha1.UpdateModeOnCreate
	policy := policyForReconcileWorkload(t, "p")

	if err := runComputeAndApply(context.Background(), r, policy, itemForTarget(target)); err != nil {
		t.Fatalf("reconcileWorkload: %v", err)
	}
	if len(rec.resized) != 0 {
		t.Errorf("OnCreate bare pods must never be resized, got %v", rec.resized)
	}
	if rec.evicted {
		t.Error("bare pods must never be evicted")
	}
}

// A bare-pod group is claimed by ONE policy, but every other kind verifies
// ownerRef UID and selector before the patcher touches a pod and this path has
// neither. A pod annotated for a different policy that happens to share the
// group's (namespace, owner-name) must not be resized under this group's
// recommendation — for memory an in-place resize can restart the container.
//
// Harmless while bare pods were computed but never applied; this branch made
// them resizable, and resizeBarePods feeds the whole member list straight to
// ResizePodsInPlace.
func TestResizeBarePods_SkipsPodOfAnotherPolicy(t *testing.T) {
	mine := barePod("airflow", "etl-run-1", "etl-daily")
	theirs := barePod("airflow", "etl-run-2", "etl-daily")
	theirs.Annotations[sustainv1alpha1.PolicyAnnotation] = "other-policy"

	r := makeReconciler(t)
	rec := newResizeRecorderClient(t, r, mine, theirs)

	target := barePodTarget("airflow", "etl-daily", mine, theirs)
	recs := map[string]workload.ContainerRecommendation{"worker": {CPURequest: qty("200m")}}

	resized, err := r.resizeBarePods(context.Background(), target, recs, workload.Tolerance{}, nil)
	if err != nil {
		t.Fatalf("resizeBarePods: %v", err)
	}
	if rec.resized["etl-run-2"] {
		t.Error("a pod annotated for another policy was resized under this group's recommendation")
	}
	if !rec.resized["etl-run-1"] {
		t.Error("the pod that actually opted into this policy must still be resized")
	}
	if resized != 1 {
		t.Errorf("resized = %d, want 1", resized)
	}
}
