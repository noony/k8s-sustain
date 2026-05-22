package oomwatch

import (
	"context"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// stubSink captures the calls the Watcher makes against the Sink interface
// and can be configured to return false to simulate a duplicate record.
type stubSink struct {
	mu       sync.Mutex
	calls    []sinkCall
	returnFn func(Key, OOMRecord) bool
}

type sinkCall struct {
	Key    Key
	Record OOMRecord
}

func (s *stubSink) Record(k Key, r OOMRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sinkCall{Key: k, Record: r})
	if s.returnFn != nil {
		return s.returnFn(k, r)
	}
	return true
}

func (s *stubSink) snapshot() []sinkCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sinkCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// Compile-time guard: our stub satisfies the Sink contract this file relies on.
var _ Sink = (*stubSink)(nil)

type stubHandler struct {
	mu    sync.Mutex
	calls []sinkCall
}

func (h *stubHandler) OnOOMDetected(_ context.Context, k Key, r OOMRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, sinkCall{Key: k, Record: r})
}

func (h *stubHandler) snapshot() []sinkCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]sinkCall, len(h.calls))
	copy(out, h.calls)
	return out
}

var _ EventHandler = (*stubHandler)(nil)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("add batchv1: %v", err)
	}
	if err := sustainv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add sustain: %v", err)
	}
	return s
}

// podBuilder is a minimal helper that keeps each test focused on the bits
// that matter to it (owner refs, container statuses) rather than the boilerplate
// of constructing a corev1.Pod.
type podBuilder struct {
	pod *corev1.Pod
}

func newPod(name string) *podBuilder {
	return &podBuilder{pod: &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			UID:         types.UID("uid-" + name),
			Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "my-policy"},
		},
	}}
}

func (b *podBuilder) noAnnotation() *podBuilder {
	delete(b.pod.Annotations, sustainv1alpha1.PolicyAnnotation)
	return b
}

func (b *podBuilder) owner(kind, name string) *podBuilder {
	ctrl := true
	b.pod.OwnerReferences = append(b.pod.OwnerReferences, metav1.OwnerReference{
		Kind:       kind,
		Name:       name,
		UID:        types.UID("uid-" + name),
		Controller: &ctrl,
	})
	return b
}

func (b *podBuilder) container(name string, memLimit string) *podBuilder {
	c := corev1.Container{Name: name}
	if memLimit != "" {
		c.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse(memLimit),
			},
		}
	}
	b.pod.Spec.Containers = append(b.pod.Spec.Containers, c)
	return b
}

func (b *podBuilder) statusOOM(name string, finishedAt time.Time, restartCount int32) *podBuilder {
	b.pod.Status.ContainerStatuses = append(b.pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: name,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "OOMKilled",
				FinishedAt: metav1.NewTime(finishedAt),
			},
		},
		RestartCount: restartCount,
	})
	return b
}

func (b *podBuilder) statusReason(name, reason string) *podBuilder {
	b.pod.Status.ContainerStatuses = append(b.pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: name,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: reason},
		},
	})
	return b
}

func (b *podBuilder) build() *corev1.Pod { return b.pod }

func reconcile(t *testing.T, c client.Client, sink Sink, handler EventHandler, pod *corev1.Pod, now time.Time) {
	t.Helper()
	w := &Watcher{
		Client:  c,
		Sink:    sink,
		Handler: handler,
		Now:     func() time.Time { return now },
	}
	res, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("Reconcile result = %#v, want zero", res)
	}
}

func TestReconcile_OOMKilledRecorded(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-30 * time.Second)

	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", finished, 3).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}
	handler := &stubHandler{}

	reconcile(t, c, sink, handler, pod, now)

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	gotKey := calls[0].Key
	wantKey := Key{Namespace: "default", OwnerKind: "StatefulSet", OwnerName: "app", Container: "main"}
	if gotKey != wantKey {
		t.Errorf("key = %#v, want %#v", gotKey, wantKey)
	}
	rec := calls[0].Record
	if !rec.TerminatedAt.Equal(finished) {
		t.Errorf("TerminatedAt = %v, want %v", rec.TerminatedAt, finished)
	}
	if rec.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want 3", rec.RestartCount)
	}
	if rec.OOMLimitBytes != 256*1024*1024 {
		t.Errorf("OOMLimitBytes = %d, want %d", rec.OOMLimitBytes, 256*1024*1024)
	}
	if rec.PolicyName != "my-policy" {
		t.Errorf("PolicyName = %q, want %q", rec.PolicyName, "my-policy")
	}
	if rec.PodName != "app-xyz" {
		t.Errorf("PodName = %q", rec.PodName)
	}
	if !rec.ObservedAt.Equal(now) {
		t.Errorf("ObservedAt = %v, want %v", rec.ObservedAt, now)
	}

	if len(handler.snapshot()) != 1 {
		t.Errorf("handler calls = %d, want 1", len(handler.snapshot()))
	}
}

func TestReconcile_NonOOMReasonSkipped(t *testing.T) {
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusReason("main", "Error").
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0", got)
	}
}

func TestReconcile_NoAnnotationSkipped(t *testing.T) {
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0", got)
	}
}

func TestReconcile_MultiContainerOnlyOOMEmitted(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "256Mi").
		container("sidecar", "64Mi").
		statusOOM("main", now.Add(-time.Minute), 1).
		statusReason("sidecar", "Completed").
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, now)

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Key.Container != "main" {
		t.Errorf("container = %q, want main", calls[0].Key.Container)
	}
}

func TestReconcile_ReplicaSetToDeployment(t *testing.T) {
	ctrl := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-7d8f",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "Deployment", Name: "app", Controller: &ctrl},
			},
		},
	}
	pod := newPod("app-xyz").
		owner("ReplicaSet", "app-7d8f").
		container("main", "128Mi").
		statusOOM("main", time.Now(), 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, rs).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Key.OwnerKind != "Deployment" || calls[0].Key.OwnerName != "app" {
		t.Errorf("owner = %s/%s, want Deployment/app", calls[0].Key.OwnerKind, calls[0].Key.OwnerName)
	}
}

func TestReconcile_ReplicaSetMissingFallsBackToRS(t *testing.T) {
	// RS is not pre-loaded into the fake client; the watcher should fall back
	// to the ReplicaSet identity rather than silently dropping the signal.
	pod := newPod("app-xyz").
		owner("ReplicaSet", "app-7d8f").
		container("main", "128Mi").
		statusOOM("main", time.Now(), 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Key.OwnerKind != "ReplicaSet" || calls[0].Key.OwnerName != "app-7d8f" {
		t.Errorf("owner = %s/%s, want ReplicaSet/app-7d8f", calls[0].Key.OwnerKind, calls[0].Key.OwnerName)
	}
}

func TestReconcile_JobToCronJob(t *testing.T) {
	ctrl := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cron-123",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "cron", Controller: &ctrl},
			},
		},
	}
	pod := newPod("cron-xyz").
		owner("Job", "cron-123").
		container("main", "64Mi").
		statusOOM("main", time.Now(), 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, job).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Key.OwnerKind != "CronJob" || calls[0].Key.OwnerName != "cron" {
		t.Errorf("owner = %s/%s, want CronJob/cron", calls[0].Key.OwnerKind, calls[0].Key.OwnerName)
	}
}

func TestReconcile_DuplicateSkipsHandler(t *testing.T) {
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "128Mi").
		statusOOM("main", time.Now(), 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{returnFn: func(Key, OOMRecord) bool { return false }}
	handler := &stubHandler{}

	reconcile(t, c, sink, handler, pod, time.Now())

	if got := len(sink.snapshot()); got != 1 {
		t.Errorf("sink calls = %d, want 1", got)
	}
	if got := len(handler.snapshot()); got != 0 {
		t.Errorf("handler calls on duplicate = %d, want 0", got)
	}
}

func TestReconcile_NewRecordFiresHandler(t *testing.T) {
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "128Mi").
		statusOOM("main", time.Now(), 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{returnFn: func(Key, OOMRecord) bool { return true }}
	handler := &stubHandler{}

	reconcile(t, c, sink, handler, pod, time.Now())

	if got := len(handler.snapshot()); got != 1 {
		t.Errorf("handler calls = %d, want 1", got)
	}
}

func TestReconcile_OrphanPodSkipped(t *testing.T) {
	pod := newPod("orphan").
		container("main", "128Mi").
		statusOOM("main", time.Now(), 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0", got)
	}
}

func TestReconcile_NotFoundNoError(t *testing.T) {
	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).Build()
	sink := &stubSink{}

	w := &Watcher{Client: c, Sink: sink, Now: time.Now}
	res, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("Result = %#v, want zero", res)
	}
	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0", got)
	}
}

func TestContainerMemLimitBytes_NoLimit(t *testing.T) {
	pod := newPod("p").container("main", "").build()
	if got := containerMemLimitBytes(pod, "main"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestContainerMemLimitBytes_Missing(t *testing.T) {
	pod := newPod("p").container("main", "64Mi").build()
	if got := containerMemLimitBytes(pod, "other"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}
