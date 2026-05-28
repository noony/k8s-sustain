package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEventTimestampPrefersFreshestField(t *testing.T) {
	first := metav1.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	last := metav1.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	eventTime := metav1.NewMicroTime(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	observed := metav1.NewMicroTime(time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC))

	cases := []struct {
		name string
		ev   corev1.Event
		want time.Time
	}{
		{
			name: "eventTime only (events.k8s.io)",
			ev:   corev1.Event{EventTime: eventTime},
			want: eventTime.Time,
		},
		{
			name: "series last observed wins",
			ev:   corev1.Event{EventTime: eventTime, Series: &corev1.EventSeries{LastObservedTime: observed}},
			want: observed.Time,
		},
		{
			name: "legacy lastTimestamp",
			ev:   corev1.Event{FirstTimestamp: first, LastTimestamp: last},
			want: last.Time,
		},
		{
			name: "falls back to creationTimestamp",
			ev:   corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: first}},
			want: first.Time,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventTimestamp(tc.ev); !got.Equal(tc.want) {
				t.Fatalf("eventTimestamp = %v, want %v", got, tc.want)
			}
		})
	}
}

// newActivityClient builds a fake client with the "source" field index the
// activity handler relies on, mirroring the API server's built-in field
// selector support for Events.
func newActivityClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(Scheme()).
		WithIndex(&corev1.Event{}, "source", func(o client.Object) []string {
			return []string{o.(*corev1.Event).Source.Component}
		}).
		WithObjects(objs...).
		Build()
}

func TestHandleSummaryActivityReturnsRecentEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "web", Namespace: "default"},
		Reason:         "Recycled",
		Message:        "controller recycled web",
		Source:         corev1.EventSource{Component: "k8s-sustain"},
	}
	c := newActivityClient(ev)
	srv := &Server{K8sClient: c, Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	srv.handleSummaryActivity(rec, httptest.NewRequest(http.MethodGet, "/api/summary/activity?limit=20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &got)
	if len(got.Items) != 1 || got.Items[0]["reason"] != "Recycled" {
		t.Fatalf("unexpected items: %+v", got.Items)
	}
}

func TestHandleSummaryActivityFiltersForeignEvents(t *testing.T) {
	sustain := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ours", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "web", Namespace: "default"},
		Reason:         "Recycled",
		Source:         corev1.EventSource{Component: "k8s-sustain"},
	}
	foreign := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "theirs", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-abc", Namespace: "default"},
		Reason:         "Scheduled",
		Source:         corev1.EventSource{Component: "default-scheduler"},
	}
	c := newActivityClient(sustain, foreign)
	srv := &Server{K8sClient: c, Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	srv.handleSummaryActivity(rec, httptest.NewRequest(http.MethodGet, "/api/summary/activity?limit=20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	decodeEnvelopeData(t, rec.Body, &got)
	if len(got.Items) != 1 || got.Items[0]["reason"] != "Recycled" {
		t.Fatalf("expected only the k8s-sustain event, got: %+v", got.Items)
	}
}
