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

// TestHandleSimulate_RejectsMalformedQuantity guards against the previous
// resource.MustParse panic that would surface as an unhelpful HTTP 500
// (after the runtime's panic recovery). Now must return a clean 400.
func TestHandleSimulate_RejectsMalformedQuantity(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	bad := "not a quantity"
	body := mustJSON(t, simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
		CPU: simulateResourceConfig{MinAllowed: &bad},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (malformed Quantity must not panic into a 500)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "minAllowed") {
		t.Errorf("expected 'minAllowed' in body, got %s", rec.Body.String())
	}
}

// TestHandleSimulate_RejectsBadDuration verifies user-supplied window/step
// strings are validated against the Prometheus duration grammar before they
// flow into Prometheus queries (where they'd cause a late, less actionable
// error — and could be used to trigger pathologically expensive queries).
func TestHandleSimulate_RejectsBadDuration(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	body := mustJSON(t, simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
		Window: "drop tables; --",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "window") {
		t.Errorf("expected 'window' in body, got %s", rec.Body.String())
	}
}

// TestHandleSimulate_RejectsOutOfRangePercentile catches the no-validation
// hole on percentile / headroom — both should match the CRD's accepted ranges.
func TestHandleSimulate_RejectsOutOfRangePercentile(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	bad := int32(150)
	body := mustJSON(t, simulateRequest{
		Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
		CPU: simulateResourceConfig{Percentile: &bad},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "percentile") {
		t.Errorf("expected 'percentile' in body, got %s", rec.Body.String())
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
