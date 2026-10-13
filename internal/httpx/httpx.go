// Package httpx holds small helpers shared by every module's handler.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/validator"
)

const maxBody = 1 << 20

// Bind strictly decodes a JSON body into dst and validates it.
func Bind(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return apperr.BadRequest("request body is required")
		}
		return apperr.BadRequest("invalid JSON body: " + err.Error())
	}
	return validator.Struct(dst)
}

func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, apperr.BadRequest("invalid " + name)
	}
	return id, nil
}

// QueryUUID returns nil when the param is absent.
func QueryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	s := strings.TrimSpace(r.URL.Query().Get(name))
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, apperr.BadRequest("invalid " + name)
	}
	return &id, nil
}

// Base is embedded by module handlers for uniform error writing.
type Base struct{ Log *zerolog.Logger }

func (b Base) Fail(w http.ResponseWriter, r *http.Request, err error) {
	response.Error(w, r, b.Log, err)
}
