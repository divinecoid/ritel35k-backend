// Package response writes the standard API envelope.
package response

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog"

	"github.com/ritel35k/backend/internal/platform/reqmeta"
	"github.com/ritel35k/backend/pkg/apperr"
)

type envelope struct {
	Success bool     `json:"success"`
	Data    any      `json:"data,omitempty"`
	Meta    any      `json:"meta,omitempty"`
	Error   *errBody `json:"error,omitempty"`
}

type errBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Details   any    `json:"details,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func OK(w http.ResponseWriter, data any) {
	write(w, http.StatusOK, envelope{Success: true, Data: data})
}
func Created(w http.ResponseWriter, data any) {
	write(w, http.StatusCreated, envelope{Success: true, Data: data})
}

func OKMeta(w http.ResponseWriter, data, meta any) {
	write(w, http.StatusOK, envelope{Success: true, Data: data, Meta: meta})
}

func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error maps any error to the envelope. Unknown errors become 500 INTERNAL and are logged.
func Error(w http.ResponseWriter, r *http.Request, log *zerolog.Logger, err error) {
	ae, ok := apperr.As(err)
	if !ok {
		ae = apperr.Internal(err)
	}
	rid := reqmeta.From(r.Context()).RequestID
	if ae.Status >= 500 && log != nil {
		log.Error().Err(err).Str("request_id", rid).Str("path", r.URL.Path).Msg("request failed")
	}
	write(w, ae.Status, envelope{Error: &errBody{Code: ae.Code, Message: ae.Message, Details: ae.Details, RequestID: rid}})
}
