package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleSummaryTrendReturnsCPUAndMemorySeries(t *testing.T) {
	srv := &Server{PromClient: &fakePromClient{}, Logger: testLogger(t)}
	srv.summaryCache = NewCache(2, 60*time.Second)
	req := httptest.NewRequest(http.MethodGet, "/api/summary/trend?window=30d", nil)
	rec := httptest.NewRecorder()
	srv.handleSummaryTrend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"cpu":`) || !strings.Contains(body, `"memory":`) {
		t.Fatalf("unexpected body: %s", body)
	}
}
