package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// requestIDHeader is the canonical name of the request correlation header.
// Clients may supply their own value; the server generates one when absent
// and echoes it on every response so operators can grep logs for a single
// request across components.
const requestIDHeader = "X-Request-Id"

type ctxKey int

const requestIDKey ctxKey = 1

// Envelope wraps every successful JSON response so the wire shape is
// uniform regardless of endpoint: clients always destructure `data` and may
// inspect `meta.requestId` when reporting issues.
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

func errorCodeForStatus(status int) string {
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

// writeJSON wraps data in an Envelope and writes it to w with the given
// status. The request ID is read from the response header (set earlier by
// the withRequestID middleware) so handlers don't need to thread the
// request through this call.
func writeJSON(w http.ResponseWriter, status int, data any) {
	rid := w.Header().Get(requestIDHeader)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Envelope{
		Data: data,
		Meta: Meta{RequestID: rid},
	})
}

// writeError writes an ErrorEnvelope. Use writeFieldError when the error
// can be attributed to a specific input field (rendered in the UI).
func writeError(w http.ResponseWriter, status int, msg string) {
	writeFieldError(w, status, msg, "")
}

func writeFieldError(w http.ResponseWriter, status int, msg, field string) {
	rid := w.Header().Get(requestIDHeader)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{
		Error: APIError{
			Code:      errorCodeForStatus(status),
			Message:   msg,
			Field:     field,
			RequestID: rid,
		},
	})
}

// newRequestID returns a 24-char hex string with enough entropy to be
// effectively collision-free across the dashboard's lifetime.
func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// requestIDFromContext returns the request ID stashed by withRequestID, or
// an empty string when the middleware didn't run (test paths that invoke a
// handler directly).
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}
