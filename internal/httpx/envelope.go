// Package httpx provides HTTP server building blocks shared by the dashboard
// and the admission webhook: a uniform JSON envelope, request-ID correlation,
// panic recovery, telemetry, body-size limiting, and a graceful-shutdown
// helper.
//
// The envelope shape and middleware semantics here match the dashboard's
// original implementation. Components that need a different response shape
// (the webhook's AdmissionReview) can opt out of the envelope while still
// reusing the middleware chain.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// RequestIDHeader is the canonical name of the request correlation header.
// Clients may supply their own value; the server generates one when absent
// and echoes it on every response so operators can grep logs for a single
// request across components.
const RequestIDHeader = "X-Request-Id"

type ctxKey int

const requestIDKey ctxKey = 1

// Envelope wraps every successful JSON response so the wire shape is uniform
// regardless of endpoint: clients always destructure `data` and may inspect
// `meta.requestId` when reporting issues.
type Envelope struct {
	Data any  `json:"data"`
	Meta Meta `json:"meta"`
}

// Meta carries per-request metadata that travels alongside the payload.
type Meta struct {
	RequestID string `json:"requestId,omitempty"`
}

// ErrorEnvelope is the canonical error response wrapper.
type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

// APIError encodes a machine-readable code, a human-readable message, and —
// for 400 validation failures — the offending request field.
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Field     string `json:"field,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

const (
	ErrCodeBadRequest         = "BAD_REQUEST"
	ErrCodeNotFound           = "NOT_FOUND"
	ErrCodeMethodNotAllowed   = "METHOD_NOT_ALLOWED"
	ErrCodeInternal           = "INTERNAL"
	ErrCodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// ErrorCodeForStatus maps an HTTP status to a stable machine-readable error
// code embedded in the JSON error envelope.
func ErrorCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return ErrCodeBadRequest
	case http.StatusNotFound:
		return ErrCodeNotFound
	case http.StatusMethodNotAllowed:
		return ErrCodeMethodNotAllowed
	case http.StatusServiceUnavailable:
		return ErrCodeServiceUnavailable
	default:
		return ErrCodeInternal
	}
}

// WriteJSON wraps data in an Envelope and writes it to w with the given
// status. The request ID is read from the response header (set earlier by
// WithRequestID) so handlers don't need to thread the request through this
// call.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	rid := w.Header().Get(RequestIDHeader)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{
		Data: data,
		Meta: Meta{RequestID: rid},
	})
}

// WriteError writes an ErrorEnvelope. Use WriteFieldError when the error can
// be attributed to a specific input field (rendered in the UI).
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteFieldError(w, status, msg, "")
}

func WriteFieldError(w http.ResponseWriter, status int, msg, field string) {
	rid := w.Header().Get(RequestIDHeader)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{
		Error: APIError{
			Code:      ErrorCodeForStatus(status),
			Message:   msg,
			Field:     field,
			RequestID: rid,
		},
	})
}

// NewRequestID returns a 24-char hex string with enough entropy to be
// effectively collision-free across a server's lifetime.
func NewRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RequestIDFromContext returns the request ID stashed by WithRequestID, or
// an empty string when the middleware didn't run.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}
