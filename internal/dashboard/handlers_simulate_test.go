package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleSimulate_RejectsNonPost verifies the handler rejects GET/PUT/etc.
// with 405 instead of trying to decode an empty body.
func TestHandleSimulate_RejectsNonPost(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleSimulate(rec, httptest.NewRequest(method, "/api/simulate", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: status = %d, want 405", method, rec.Code)
			}
		})
	}
}

func TestHandleSimulate_RejectsBadJSON(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleSimulate_RejectsMissingNamespace(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	body := mustJSON(t, simulateRequest{OwnerKind: "Deployment", OwnerName: "web"})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "namespace") {
		t.Errorf("expected 'namespace' in body, got %s", rec.Body.String())
	}
}

func TestHandleSimulate_RejectsMissingOwnerName(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	body := mustJSON(t, simulateRequest{Namespace: "default", OwnerKind: "Deployment"})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ownerName") {
		t.Errorf("expected 'ownerName' in body, got %s", rec.Body.String())
	}
}

// TestHandleSimulate_RejectsInvalidOwnerKind verifies that unsupported kinds
// like "Rollout" or "Pod" are bounced before any expensive work happens.
func TestHandleSimulate_RejectsInvalidOwnerKind(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	body := mustJSON(t, simulateRequest{Namespace: "default", OwnerKind: "Pod", OwnerName: "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ownerKind") {
		t.Errorf("expected 'ownerKind' in body, got %s", rec.Body.String())
	}
}

func mustJSON(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}
