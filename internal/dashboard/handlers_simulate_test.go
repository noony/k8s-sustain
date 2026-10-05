package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/noony/k8s-sustain/internal/recommender/recommendertest"
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
// like "ReplicaSet" are bounced before any expensive work happens. "Pod" is a
// supported kind (see TestHandleSimulate_AcceptsPod) — it identifies a
// bare-pod identity formed via api/v1alpha1.OwnerNameAnnotation.
func TestHandleSimulate_RejectsInvalidOwnerKind(t *testing.T) {
	srv := &Server{Logger: testLogger(t)}
	for _, kind := range []string{"ReplicaSet"} {
		t.Run(kind, func(t *testing.T) {
			body := mustJSON(t, simulateRequest{Namespace: "default", OwnerKind: kind, OwnerName: "x"})
			req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
			rec := httptest.NewRecorder()
			srv.handleSimulate(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "ownerKind") {
				t.Errorf("expected 'ownerKind' in body, got %s", rec.Body.String())
			}
		})
	}
}

// TestHandleSimulate_AcceptsJob pins Job as a simulatable kind — it is in
// supportedWorkloadKinds and handled by the simulation pipeline, so the
// validator must not bounce it with a 400.
func TestHandleSimulate_AcceptsJob(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}
	body := mustJSON(t, simulateRequest{Namespace: "default", OwnerKind: "Job", OwnerName: "oneshot"})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleSimulate_AcceptsRollout pins Argo Rollout as a simulatable kind —
// the controller right-sizes Rollouts, so the dashboard validator must not
// bounce it with a 400.
func TestHandleSimulate_AcceptsRollout(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}
	body := mustJSON(t, simulateRequest{Namespace: "default", OwnerKind: "Rollout", OwnerName: "web"})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleSimulate_AcceptsPod pins "Pod" as a simulatable kind — bare pods
// opted in via api/v1alpha1.OwnerNameAnnotation are a supported workload
// identity, so the validator must not bounce it with a 400. OwnerName here is
// the owner-name annotation value, not a real pod name; no pods need to exist
// for the request to validate and reach the simulation pipeline.
func TestHandleSimulate_AcceptsPod(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}
	body := mustJSON(t, simulateRequest{Namespace: "default", OwnerKind: "Pod", OwnerName: "etl-daily"})
	req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
	rec := httptest.NewRecorder()
	srv.handleSimulate(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
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

// An override the Policy CRD would reject is a 400 naming its field, and a
// starting Policy that does not exist is a 404.
func TestHandleSimulate_RejectsWhatItCannotStartFrom(t *testing.T) {
	srv := &Server{
		K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
		PromClient: &fakePromClient{},
		Inputs:     recommendertest.NewStaticInputs(),
		Logger:     testLogger(t),
	}
	cases := []struct {
		name       string
		req        simulateRequest
		wantStatus int
		wantField  string
	}{
		{
			name: "invalid override",
			req: simulateRequest{
				Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
				Override: json.RawMessage(`{"resourcesConfigs":{"memory":{"requests":{"minAllowed":"2Gi","maxAllowed":"1Gi"}}}}`),
			},
			wantStatus: http.StatusBadRequest,
			wantField:  "resourcesConfigs.memory.requests",
		},
		{
			name:       "unknown starting Policy",
			req:        simulateRequest{Namespace: "default", OwnerKind: "Deployment", OwnerName: "web", Policy: "ghost"},
			wantStatus: http.StatusNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleSimulate(rec, httptest.NewRequest(http.MethodPost, "/api/simulate", mustJSON(t, tc.req)))

			var body struct {
				Error struct {
					Field string `json:"field"`
				} `json:"error"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != tc.wantStatus || body.Error.Field != tc.wantField {
				t.Errorf("status %d with field %q, want %d with %q; body=%s", rec.Code, body.Error.Field, tc.wantStatus, tc.wantField, rec.Body)
			}
		})
	}
}

// TestSimulateRejectsInvalidAbsoluteRange covers the POST /api/simulate
// validation of fromTs/toTs pairs added in validateAbsoluteRange.
func TestSimulateRejectsInvalidAbsoluteRange(t *testing.T) {
	t.Run("from_equals_to", func(t *testing.T) {
		srv := &Server{Logger: testLogger(t)}
		body := mustJSON(t, simulateRequest{
			Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
			FromTs: 1718000000, ToTs: 1718000000,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
		rec := httptest.NewRecorder()
		srv.handleSimulate(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("from==to: status = %d, want 400", rec.Code)
		}
	})

	t.Run("from_after_to", func(t *testing.T) {
		srv := &Server{Logger: testLogger(t)}
		body := mustJSON(t, simulateRequest{
			Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
			FromTs: 1718003600, ToTs: 1718000000,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
		rec := httptest.NewRecorder()
		srv.handleSimulate(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("from>to: status = %d, want 400", rec.Code)
		}
	})

	t.Run("only_from_set", func(t *testing.T) {
		srv := &Server{Logger: testLogger(t)}
		body := mustJSON(t, simulateRequest{
			Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
			FromTs: 1718000000,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
		rec := httptest.NewRecorder()
		srv.handleSimulate(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("only fromTs: status = %d, want 400", rec.Code)
		}
	})

	t.Run("only_to_set", func(t *testing.T) {
		srv := &Server{Logger: testLogger(t)}
		body := mustJSON(t, simulateRequest{
			Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
			ToTs: 1718003600,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
		rec := httptest.NewRecorder()
		srv.handleSimulate(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("only toTs: status = %d, want 400", rec.Code)
		}
	})

	t.Run("valid_range_passes_validation", func(t *testing.T) {
		srv := &Server{
			K8sClient:  fake.NewClientBuilder().WithScheme(Scheme()).Build(),
			PromClient: &fakePromClient{},
			Inputs:     recommendertest.NewStaticInputs(),
			Logger:     testLogger(t),
		}
		body := mustJSON(t, simulateRequest{
			Namespace: "default", OwnerKind: "Deployment", OwnerName: "web",
			FromTs: 1718000000, ToTs: 1718003600,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/simulate", body)
		rec := httptest.NewRecorder()
		srv.handleSimulate(rec, req)
		if rec.Code == http.StatusBadRequest {
			t.Errorf("valid range: got 400, body=%s", rec.Body.String())
		}
	})
}

func mustJSON(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}
