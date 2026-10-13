package settings

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/pkg/apperr"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/settings", func(r chi.Router) {
		r.With(g.Perm(access.SettingsView)).Get("/registry", h.registry)
		r.With(g.Perm(access.SettingsView)).Get("/effective", h.effective)
		r.With(g.Perm(access.SettingsManage)).Put("/", h.set)
		r.With(g.Perm(access.SettingsManage)).Delete("/", h.unset)
	})
}

func (h *Handler) registry(w http.ResponseWriter, r *http.Request) { response.OK(w, Registry) }

func (h *Handler) effective(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.QueryUUID(r, "org_unit_id")
	if err != nil || id == nil {
		h.Fail(w, r, apperr.BadRequest("org_unit_id is required"))
		return
	}
	out, err := h.svc.Effective(r.Context(), *id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type setRequest struct {
	OrgUnitID string          `json:"org_unit_id" validate:"required,uuid"`
	Key       string          `json:"key" validate:"required"`
	Value     json.RawMessage `json:"value" validate:"required"`
}

func (h *Handler) set(w http.ResponseWriter, r *http.Request) {
	var req setRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.Set(r.Context(), mustUUID(req.OrgUnitID), req.Key, req.Value); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

type unsetRequest struct {
	OrgUnitID string `json:"org_unit_id" validate:"required,uuid"`
	Key       string `json:"key" validate:"required"`
}

func (h *Handler) unset(w http.ResponseWriter, r *http.Request) {
	var req unsetRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.Unset(r.Context(), mustUUID(req.OrgUnitID), req.Key); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

func mustUUID(s string) uuid.UUID { id, _ := uuid.Parse(s); return id }
