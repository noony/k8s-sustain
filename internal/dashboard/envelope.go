package dashboard

import (
	"net/http"

	"github.com/noony/k8s-sustain/internal/httpx"
)

// The dashboard wraps every response in the shared httpx envelope. The
// lower-case helpers below are kept as one-line shims so handler files don't
// each have to import internal/httpx directly.

const (
	ErrCodeBadRequest         = httpx.ErrCodeBadRequest
	ErrCodeNotFound           = httpx.ErrCodeNotFound
	ErrCodeMethodNotAllowed   = httpx.ErrCodeMethodNotAllowed
	ErrCodeInternal           = httpx.ErrCodeInternal
	ErrCodeServiceUnavailable = httpx.ErrCodeServiceUnavailable
)

type (
	Envelope      = httpx.Envelope
	Meta          = httpx.Meta
	ErrorEnvelope = httpx.ErrorEnvelope
	APIError      = httpx.APIError
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	httpx.WriteJSON(w, status, data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteError(w, status, msg)
}

func writeFieldError(w http.ResponseWriter, status int, msg, field string) {
	httpx.WriteFieldError(w, status, msg, field)
}
