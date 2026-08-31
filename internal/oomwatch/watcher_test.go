package oomwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	sustainv1alpha1 "github.com/noony/k8s-sustain/api/v1alpha1"
)

// stubSink captures the calls the Watcher makes against the Sink interface
// and can be configured to return false to simulate a duplicate record. It
// also backs AlreadyResolved/MarkResolved with a plain map (no TTL — tests
// that need TTL expiry exercise the real Cache instead) so the pre-walk dedup
// path is exercisable without pulling in Cache.
type stubSink struct {
	mu                sync.Mutex
	calls             []sinkCall
	returnFn          func(Key, OOMRecord) bool
	resolved          map[resolvedEventKey]bool
	markResolvedCalls []resolvedEventKey
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

func (s *stubSink) AlreadyResolved(podUID types.UID, container string, restartCount int32, terminatedAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := resolvedEventKey{PodUID: podUID, Container: container, RestartCount: restartCount, TerminatedAt: terminatedAt}
	return s.resolved[key]
}

func (s *stubSink) MarkResolved(podUID types.UID, container string, restartCount int32, terminatedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolved == nil {
		s.resolved = make(map[resolvedEventKey]bool)
	}
	key := resolvedEventKey{PodUID: podUID, Container: container, RestartCount: restartCount, TerminatedAt: terminatedAt}
	s.resolved[key] = true
	s.markResolvedCalls = append(s.markResolvedCalls, key)
}

// Compile-time guard: our stub satisfies the Sink contract this file relies on.
var _ Sink = (*stubSink)(nil)

// errFakeRead is the underlying error wrapped by the injected apierrors in
// the read-failure tests below; a fixed sentinel keeps the fake-client
// interceptor setups terse.
var errFakeRead = errors.New("boom")

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

func (b *podBuilder) annotation(key, value string) *podBuilder {
	if b.pod.Annotations == nil {
		b.pod.Annotations = map[string]string{}
	}
	b.pod.Annotations[key] = value
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

// appliedResources sets ContainerStatus.Resources — the limits the kubelet
// actually applied to the running container. It diverges from pod.Spec while
// an in-place resize is pending or infeasible, which is exactly the case the
// OOM anchor has to get right.
func (b *podBuilder) appliedResources(name, memLimit string) *podBuilder {
	res := &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(memLimit),
		},
	}
	for i := range b.pod.Status.ContainerStatuses {
		if b.pod.Status.ContainerStatuses[i].Name == name {
			b.pod.Status.ContainerStatuses[i].Resources = res
			return b
		}
	}
	b.pod.Status.ContainerStatuses = append(b.pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:      name,
		Resources: res,
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

// TestReconcile_NewOOMAfterResolvedStillFiresHandler is the load-bearing
// property behind resolvedEventKey: a genuinely new OOM kill on a pod that
// already had an earlier kill marked resolved must still be recorded and
// still fire the handler. TestReconcile_DuplicateSkipsHandler only exercises
// a single reconcile, so it cannot catch a regression that made
// resolvedEventKey (or AlreadyResolved) too coarse — e.g. keyed on pod UID +
// container alone, ignoring RestartCount/TerminatedAt. If that ever broke,
// real OOM kills would silently stop being recorded and the recommender's
// memory-floor bump would silently stop firing.
func TestReconcile_NewOOMAfterResolvedStillFiresHandler(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "128Mi").
		statusOOM("main", now.Add(-time.Minute), 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sinkCache := NewCache(time.Hour)
	handler := &stubHandler{}

	reconcile(t, c, sinkCache, handler, pod, now)
	if got := len(handler.snapshot()); got != 1 {
		t.Fatalf("handler calls after first OOM = %d, want 1", got)
	}

	// A genuinely new OOM on the same container: RestartCount bumped and
	// TerminatedAt later, exactly the pair AlreadyResolved/MarkResolved key
	// on (see resolvedEventKey's doc).
	pod.Status.ContainerStatuses[0].RestartCount = 1
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = metav1.NewTime(now.Add(time.Minute))
	// Pod is in the fake client's default status-subresource set, so a plain
	// Update would silently discard this Status change — Status().Update is
	// required.
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	reconcile(t, c, sinkCache, handler, pod, now.Add(2*time.Minute))

	if got := len(handler.snapshot()); got != 2 {
		t.Fatalf("handler calls after second (new) OOM = %d, want 2 — a new RestartCount/TerminatedAt must not be suppressed by the earlier mark", got)
	}
	rec := handler.snapshot()[1].Record
	if rec.RestartCount != 1 {
		t.Errorf("second handler call RestartCount = %d, want 1 (the newer kill)", rec.RestartCount)
	}
	if !rec.TerminatedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("second handler call TerminatedAt = %v, want %v", rec.TerminatedAt, now.Add(time.Minute))
	}
}

// TestReconcile_NewOOMSameSecondDifferentRestartStillFiresHandler isolates
// the RestartCount half of resolvedEventKey specifically. metav1.Time only
// round-trips to whole-second precision through the API (Kubernetes objects
// serialize FinishedAt as RFC3339), so two OOM kills of the same container
// within the same wall-clock second can carry an IDENTICAL TerminatedAt and
// differ only by RestartCount. If resolvedEventKey (or the Sink built on it)
// ever dropped RestartCount from its identity — keying only on
// PodUID+Container+TerminatedAt — this exact scenario would collide: the
// second kill would be reported as "already resolved" and silently dropped,
// even though it is a genuinely new termination. TestReconcile_
// NewOOMAfterResolvedStillFiresHandler bumps both fields together (the
// common case) and would NOT catch that specific regression, because
// TerminatedAt alone would still distinguish the two events.
func TestReconcile_NewOOMSameSecondDifferentRestartStillFiresHandler(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finishedAt := now.Add(-time.Minute)
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "128Mi").
		statusOOM("main", finishedAt, 0).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sinkCache := NewCache(time.Hour)
	handler := &stubHandler{}

	reconcile(t, c, sinkCache, handler, pod, now)
	if got := len(handler.snapshot()); got != 1 {
		t.Fatalf("handler calls after first OOM = %d, want 1", got)
	}

	// Same TerminatedAt as the first kill, only RestartCount bumped.
	pod.Status.ContainerStatuses[0].RestartCount = 1
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	reconcile(t, c, sinkCache, handler, pod, now.Add(time.Minute))

	if got := len(handler.snapshot()); got != 2 {
		t.Errorf("handler calls after second (new) OOM = %d, want 2 — a bumped RestartCount alone must still be treated as a new termination", got)
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

// TestReconcile_OwnerNameOverride_BarePod verifies a bare pod (no controller
// owner) with a valid owner-name annotation is recorded under kind "Pod" with
// the annotation value as name, instead of being skipped as an orphan.
func TestReconcile_OwnerNameOverride_BarePod(t *testing.T) {
	pod := newPod("etl-daily-run-1").
		annotation(sustainv1alpha1.OwnerNameAnnotation, "etl-daily").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	wantKey := Key{Namespace: "default", OwnerKind: "Pod", OwnerName: "etl-daily", Container: "main"}
	if calls[0].Key != wantKey {
		t.Errorf("key = %#v, want %#v", calls[0].Key, wantKey)
	}
}

// TestReconcile_OwnerNameOverride_OwnedPod verifies a pod with a real
// controller owner and a valid owner-name annotation is recorded under the
// real kind but the overridden name.
func TestReconcile_OwnerNameOverride_OwnedPod(t *testing.T) {
	pod := newPod("app-blue-xyz").
		owner("StatefulSet", "app-blue").
		annotation(sustainv1alpha1.OwnerNameAnnotation, "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	wantKey := Key{Namespace: "default", OwnerKind: "StatefulSet", OwnerName: "app", Container: "main"}
	if calls[0].Key != wantKey {
		t.Errorf("key = %#v, want %#v", calls[0].Key, wantKey)
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

// TestContainerMemLimitBytes_PrefersAppliedLimitOverDesiredSpec pins the
// anchor to the limit the kernel actually killed at.
//
// pod.Spec carries the DESIRED limit, which the recommender itself rewrites on
// every in-place resize. Reading the anchor from there feeds the OOM memory
// floor its own previous output: floor = limit * 1.20 (bump) * 1.15 (headroom)
// and the new limit = 1.50 * request, so each kill multiplies the
// recommendation by ~2.07x. Observed end to end on a kind cluster: 120Mi grew
// to 19630Mi in six seconds, ending at a 59GiB limit on an 8GiB node.
func TestContainerMemLimitBytes_PrefersAppliedLimitOverDesiredSpec(t *testing.T) {
	// Spec was already bumped to 512Mi by a previous recommendation; the
	// kubelet only ever applied 128Mi, which is what the container died at.
	pod := newPod("p").
		container("main", "512Mi").
		appliedResources("main", "128Mi").
		build()

	if got, want := containerMemLimitBytes(pod, "main"), int64(128*1024*1024); got != want {
		t.Errorf("containerMemLimitBytes = %d, want %d (applied limit, not desired spec)", got, want)
	}
}

// TestContainerMemLimitBytes_FallsBackToSpecWhenStatusEmpty covers clusters or
// moments where the kubelet has not populated ContainerStatus.Resources.
func TestContainerMemLimitBytes_FallsBackToSpecWhenStatusEmpty(t *testing.T) {
	pod := newPod("p").container("main", "64Mi").build()

	if got, want := containerMemLimitBytes(pod, "main"), int64(64*1024*1024); got != want {
		t.Errorf("containerMemLimitBytes = %d, want %d", got, want)
	}
}

// TestReconcile_OOMLimitAnchorsOnAppliedLimit is the regression guard for the
// runaway: the recorded anchor must not be the request the recommender just
// wrote, or the floor compounds against itself on every kill.
func TestReconcile_OOMLimitAnchorsOnAppliedLimit(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-30 * time.Second)

	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "40635Mi").
		statusOOM("main", finished, 12).
		appliedResources("main", "180Mi").
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
	if got, want := calls[0].Record.OOMLimitBytes, int64(180*1024*1024); got != want {
		t.Errorf("OOMLimitBytes = %d, want %d (applied limit, not the infeasible 40635Mi spec)", got, want)
	}
}

// TestContainerMemLimitBytes_AppliedStatusWithoutMemoryLimitWins covers a
// resize that REMOVES the memory limit (RemoveMemoryLimit). Once the kubelet
// has applied that, there is no limit for the kernel to kill at, so the stale
// spec value must not be resurrected as a bump anchor.
func TestContainerMemLimitBytes_AppliedStatusWithoutMemoryLimitWins(t *testing.T) {
	pod := newPod("p").container("main", "256Mi").build()
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:      "main",
		Resources: &corev1.ResourceRequirements{Limits: corev1.ResourceList{}},
	})

	if got := containerMemLimitBytes(pod, "main"); got != 0 {
		t.Errorf("containerMemLimitBytes = %d, want 0 (applied state has no memory limit)", got)
	}
}

func TestContainerMemLimitBytes_Missing(t *testing.T) {
	pod := newPod("p").container("main", "64Mi").build()
	if got := containerMemLimitBytes(pod, "other"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// TestReconcile_NamespaceLevelOptIn_RecordsOOM is the regression this task
// exists for: a pod whose workload opted in via its Namespace carries no
// policy annotation of its own, so the old annotation-only check dropped its
// OOM kill entirely — costing both the immediate re-reconcile and the
// memory-floor bump.
func TestReconcile_NamespaceLevelOptIn_RecordsOOM(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "default",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "ns-policy"},
	}}
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", now.Add(-time.Minute), 1).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, ns).Build()
	sink := &stubSink{}
	handler := &stubHandler{}

	reconcile(t, c, sink, handler, pod, now)

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Record.PolicyName != "ns-policy" {
		t.Errorf("PolicyName = %q, want %q", calls[0].Record.PolicyName, "ns-policy")
	}
	wantKey := Key{Namespace: "default", OwnerKind: "StatefulSet", OwnerName: "app", Container: "main"}
	if calls[0].Key != wantKey {
		t.Errorf("key = %#v, want %#v", calls[0].Key, wantKey)
	}
	if len(handler.snapshot()) != 1 {
		t.Errorf("handler calls = %d, want 1", len(handler.snapshot()))
	}
}

// TestReconcile_WorkloadLevelOptIn_RecordsOOM covers the same path via the
// Deployment's (here StatefulSet's) own metadata.annotations.
func TestReconcile_WorkloadLevelOptIn_RecordsOOM(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "app",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "workload-policy"},
	}}
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", now.Add(-time.Minute), 1).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, sts).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, now)

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Record.PolicyName != "workload-policy" {
		t.Errorf("PolicyName = %q, want %q", calls[0].Record.PolicyName, "workload-policy")
	}
}

// TestReconcile_OptOutBeatsNamespaceOptIn verifies the escape hatch: a
// workload that opts out records nothing even though its namespace opts in.
func TestReconcile_OptOutBeatsNamespaceOptIn(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "default",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "ns-policy"},
	}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "app",
		Annotations: map[string]string{sustainv1alpha1.OptOutAnnotation: "true"},
	}}
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	sch := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, sts, ns).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0", got)
	}
}

// TestReconcile_UnmanagedPod_RecordsNothing pins that a pod resolving to no
// policy at any level is still dropped — the load-bearing resync check.
func TestReconcile_UnmanagedPod_RecordsNothing(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	sch := newScheme(t)
	// The StatefulSet owner is deliberately absent from the fake client, and
	// the Namespace carries no annotations either — nothing at any level opts
	// this pod in.
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, ns).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, time.Now())

	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0", got)
	}
}

// TestReconcile_OwnerAnnotationsReadError_ReturnsError pins the fix for a
// review finding: a non-NotFound failure reading the owner object's
// annotations must be returned as an error, not silently degraded to "no
// annotations at that level". Degrading would resolve to the pod's own
// annotation alone, which is empty for exactly the workload- and
// namespace-level opt-ins this task exists to serve — silently un-managing
// them for as long as the read keeps failing. Returning the error instead
// makes controller-runtime requeue with backoff, matching the precedent in
// internal/controller/namespace_annotations.go's identical Namespace read.
func TestReconcile_OwnerAnnotationsReadError_ReturnsError(t *testing.T) {
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	sch := newScheme(t)
	boom := apierrors.NewInternalError(errFakeRead)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	sink := &stubSink{}

	w := &Watcher{Client: c, Sink: sink, Now: time.Now}
	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
	})
	if err == nil {
		t.Fatal("Reconcile returned nil error for a non-NotFound owner read failure; want an error so controller-runtime requeues")
	}
	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0 — the OOM must not be recorded under a degraded (pod-only) resolution", got)
	}
}

// TestReconcile_NamespaceAnnotationsReadError_ReturnsError is the Namespace-read
// counterpart of TestReconcile_OwnerAnnotationsReadError_ReturnsError.
func TestReconcile_NamespaceAnnotationsReadError_ReturnsError(t *testing.T) {
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	sch := newScheme(t)
	boom := apierrors.NewInternalError(errFakeRead)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	sink := &stubSink{}

	w := &Watcher{Client: c, Sink: sink, Now: time.Now}
	_, err := w.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
	})
	if err == nil {
		t.Fatal("Reconcile returned nil error for a non-NotFound namespace read failure; want an error so controller-runtime requeues")
	}
	if got := len(sink.snapshot()); got != 0 {
		t.Errorf("sink calls = %d, want 0 — the OOM must not be recorded under a degraded (pod-only) resolution", got)
	}
}

// TestReconcile_AlreadyResolvedOOMSkipsOwnerAndNamespaceGets pins the fix for
// a review finding: hasFreshOOM's signal
// (LastTerminationState.Terminated.Reason == OOMKilled) never clears on its
// own, so a pod that has ever been OOM-killed re-triggers Reconcile on every
// later status write too — readiness flips, IP changes, anything — not just
// the actual kill. Before this fix, every one of those re-runs re-did the
// full owner+Namespace walk even though nothing about the kill had changed
// (same RestartCount + TerminatedAt), because Reconcile had no way to know
// that without doing the very walk the check exists to save. Uses the real
// Cache (not stubSink) so this exercises the actual AlreadyResolved/
// MarkResolved wiring end to end, not a test double's approximation of it.
func TestReconcile_AlreadyResolvedOOMSkipsOwnerAndNamespaceGets(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "default",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "ns-policy"},
	}}
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", now.Add(-time.Minute), 1).
		build()

	sch := newScheme(t)
	var stsGets, nsGets int
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, ns).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *appsv1.StatefulSet:
					stsGets++
				case *corev1.Namespace:
					nsGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	sinkCache := NewCache(time.Hour)
	handler := &stubHandler{}

	reconcile(t, c, sinkCache, handler, pod, now)
	if got := sinkCache.Size(); got != 1 {
		t.Fatalf("cache size after first reconcile = %d, want 1", got)
	}
	if stsGets == 0 || nsGets == 0 {
		t.Fatalf("first reconcile: stsGets=%d nsGets=%d, want both > 0 (the walk must happen at least once)", stsGets, nsGets)
	}
	baseSts, baseNs := stsGets, nsGets

	// A repeat status event for the exact same pod object — same
	// RestartCount, same TerminatedAt — simulating an unrelated status write
	// (readiness flip, IP change) on a pod that has not had a new OOM.
	reconcile(t, c, sinkCache, handler, pod, now.Add(time.Second))

	if stsGets != baseSts {
		t.Errorf("second reconcile issued %d StatefulSet Gets beyond the baseline %d; the already-resolved OOM must not re-run the owner walk", stsGets-baseSts, baseSts)
	}
	if nsGets != baseNs {
		t.Errorf("second reconcile issued %d Namespace Gets beyond the baseline %d; the already-resolved OOM must not re-run the Namespace read", nsGets-baseNs, baseNs)
	}
	if got := len(handler.snapshot()); got != 1 {
		t.Errorf("handler calls = %d, want 1 (no new notification for a repeat of an already-resolved event)", got)
	}
}

// TestReconcile_PodTemplateOptIn_NoOwnerOrNamespaceReads pins the fix for a
// review finding: Reconcile used to resolve all three annotation levels
// eagerly, fetching the owner object and the Namespace even when the pod's
// own annotation already decides the outcome. That coupled every existing
// pod-template-only workload to reads it never needed — a transient owner or
// Namespace read failure would drop and requeue an OOM signal for a workload
// untouched by this branch's workload/namespace opt-in feature. The walk must
// be lazy: zero owner Gets and zero Namespace Gets when the pod template
// decides.
func TestReconcile_PodTemplateOptIn_NoOwnerOrNamespaceReads(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", now.Add(-time.Minute), 1).
		build()

	sch := newScheme(t)
	var ownerGets, nsGets int
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch obj.(type) {
				case *appsv1.StatefulSet:
					ownerGets++
				case *corev1.Namespace:
					nsGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, now)

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Record.PolicyName != "my-policy" {
		t.Errorf("PolicyName = %q, want %q", calls[0].Record.PolicyName, "my-policy")
	}
	if ownerGets != 0 {
		t.Errorf("owner (StatefulSet) Gets = %d, want 0 — the pod template already decided", ownerGets)
	}
	if nsGets != 0 {
		t.Errorf("Namespace Gets = %d, want 0 — the pod template already decided", nsGets)
	}
}

// TestReconcile_WorkloadLevelOptIn_NoNamespaceReads is the second step of the
// same laziness: when the pod template is silent but the owning workload's
// own annotation decides, the owner Get still has to happen (it is the only
// way to read that level), but the Namespace read must not.
func TestReconcile_WorkloadLevelOptIn_NoNamespaceReads(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "default",
		Name:        "app",
		Annotations: map[string]string{sustainv1alpha1.PolicyAnnotation: "workload-policy"},
	}}
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", now.Add(-time.Minute), 1).
		build()

	sch := newScheme(t)
	var nsGets int
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod, sts).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok {
					nsGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	sink := &stubSink{}

	reconcile(t, c, sink, nil, pod, now)

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("sink calls = %d, want 1", len(calls))
	}
	if calls[0].Record.PolicyName != "workload-policy" {
		t.Errorf("PolicyName = %q, want %q", calls[0].Record.PolicyName, "workload-policy")
	}
	if nsGets != 0 {
		t.Errorf("Namespace Gets = %d, want 0 — the workload level already decided", nsGets)
	}
}

// TestReconcile_DegradedOwnerResolution_DoesNotMarkResolved pins the fix for a
// review finding: when ResolvePodOwner's Get fails, Reconcile falls back to
// the immediate controller ref (immediateController) rather than dropping the
// OOM — but when the pod template alone decides the policy (the common
// opt-in path), ownerAnnotations is never called on that pass, so the failed
// ResolvePodOwner Get is never retried. If MarkResolved were still called for
// this termination, AlreadyResolved would suppress the owner walk on every
// later status write for up to the cache TTL, even after the apiserver
// recovers — permanently pinning the OOM under the degraded ReplicaSet bucket
// instead of self-healing to the real Deployment owner. A second reconcile of
// the same pod (same RestartCount/TerminatedAt) must therefore still retry
// the ResolvePodOwner Get.
func TestReconcile_DegradedOwnerResolution_DoesNotMarkResolved(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// The pod template carries the policy annotation (newPod's default), so
	// this pass never reaches ownerAnnotations — the only place that would
	// otherwise retry the ReplicaSet Get within the same pass.
	pod := newPod("app-xyz").
		owner("ReplicaSet", "app-rs").
		container("main", "128Mi").
		statusOOM("main", now.Add(-time.Minute), 0).
		build()

	sch := newScheme(t)
	var rsGets int
	// The ReplicaSet is deliberately never preloaded into the fake client, so
	// every Get for it fails with NotFound — the same failure
	// TestReconcile_ReplicaSetMissingFallsBackToRS exercises, just counted
	// here across two reconciles instead of one.
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(pod).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.ReplicaSet); ok {
					rsGets++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	sinkCache := NewCache(time.Hour)

	reconcile(t, c, sinkCache, nil, pod, now)
	if rsGets != 1 {
		t.Fatalf("rsGets after first reconcile = %d, want 1", rsGets)
	}
	if sinkCache.Size() != 1 {
		t.Fatalf("cache size after first reconcile = %d, want 1", sinkCache.Size())
	}

	// Same RestartCount and TerminatedAt as the first pass — simulating an
	// unrelated status write (readiness flip, IP change) that re-triggers
	// Reconcile without a new OOM kill.
	reconcile(t, c, sinkCache, nil, pod, now.Add(time.Second))

	if rsGets != 2 {
		t.Errorf("rsGets after second reconcile = %d, want 2 — a degraded owner resolution must not be marked resolved, so the owner walk must retry", rsGets)
	}
}

// TestPredicate_AdmitsOOMKilledPodWithoutAnnotation pins the broadened event
// filter: a pod with no policy annotation but an OOMKilled container status
// must reach Reconcile, or namespace-level opt-ins never get an event at all.
func TestPredicate_AdmitsOOMKilledPodWithoutAnnotation(t *testing.T) {
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	pred := admitPodEventPredicate()
	if !pred.Create(event.CreateEvent{Object: pod}) {
		t.Error("CreateFunc dropped an OOM-killed pod with no policy annotation")
	}
	if !pred.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: pod}) {
		t.Error("UpdateFunc dropped an OOM-killed pod with no policy annotation")
	}
	if !pred.Generic(event.GenericEvent{Object: pod}) {
		t.Error("GenericFunc dropped an OOM-killed pod with no policy annotation")
	}
}

// TestPredicate_DropsOrdinaryPodWithoutAnnotation pins that the filter still
// drops the common case — an unannotated pod with no OOM status. Without this
// the watcher wakes on every pod transition in the cluster.
func TestPredicate_DropsOrdinaryPodWithoutAnnotation(t *testing.T) {
	pod := newPod("app-xyz").
		noAnnotation().
		owner("StatefulSet", "app").
		container("main", "256Mi").
		build()

	pred := admitPodEventPredicate()
	if pred.Create(event.CreateEvent{Object: pod}) {
		t.Error("CreateFunc admitted an ordinary pod with no policy annotation and no OOM status")
	}
	if pred.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: pod}) {
		t.Error("UpdateFunc admitted an ordinary pod with no policy annotation and no OOM status")
	}
	if pred.Generic(event.GenericEvent{Object: pod}) {
		t.Error("GenericFunc admitted an ordinary pod with no policy annotation and no OOM status")
	}
}

// TestPredicate_AdmitsAnnotatedPodWithoutOOM pins the original arm of the
// filter, which the broadened predicate must not have regressed: a pod
// carrying the policy annotation is admitted even with no OOM status at all.
func TestPredicate_AdmitsAnnotatedPodWithoutOOM(t *testing.T) {
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "256Mi").
		build()

	pred := admitPodEventPredicate()
	if !pred.Create(event.CreateEvent{Object: pod}) {
		t.Error("CreateFunc dropped an annotated pod with no OOM status")
	}
	if !pred.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: pod}) {
		t.Error("UpdateFunc dropped an annotated pod with no OOM status")
	}
	if !pred.Generic(event.GenericEvent{Object: pod}) {
		t.Error("GenericFunc dropped an annotated pod with no OOM status")
	}
}

// TestPredicate_DeleteAlwaysDropped pins that Delete events are always
// dropped regardless of annotation or OOM status: an OOM kill never surfaces
// as a Pod delete (the pod restarts in place), so chasing one would only
// race with the GC.
func TestPredicate_DeleteAlwaysDropped(t *testing.T) {
	pod := newPod("app-xyz").
		owner("StatefulSet", "app").
		container("main", "256Mi").
		statusOOM("main", time.Now(), 1).
		build()

	pred := admitPodEventPredicate()
	if pred.Delete(event.DeleteEvent{Object: pod}) {
		t.Error("DeleteFunc admitted a delete event for an annotated, OOM-killed pod")
	}
}
