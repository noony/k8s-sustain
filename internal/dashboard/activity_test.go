package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestHandleSummaryActivityReturnsRecentEvents(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "web", Namespace: "default"},
		Reason:         "Recycled",
		Message:        "controller recycled web",
		Source:         corev1.EventSource{Component: "k8s-sustain"},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(ev).Build()
	srv := &Server{K8sClient: c, Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	srv.handleSummaryActivity(rec, httptest.NewRequest(http.MethodGet, "/api/summary/activity?limit=20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0]["reason"] != "Recycled" {
		t.Fatalf("unexpected items: %+v", got.Items)
	}
}
