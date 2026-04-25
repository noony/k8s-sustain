package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func metricsRegistry() interface {
	Gather() ([]*dto.MetricFamily, error)
} {
	return registryForTest
}

func TestHandlerRecordsRequestDuration(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Handler().ServeHTTP(rec, req)

	mfs, _ := metricsRegistry().Gather()
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "k8s_sustain_dashboard_request_duration_seconds" {
			found = true
		}
	}
	if !found {
		t.Fatal("dashboard duration histogram not registered")
	}
}
