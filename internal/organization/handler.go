package organization

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
	r.Route("/organization", func(r chi.Router) {
		r.With(g.Perm(access.OrganizationView)).Get("/scope-tree", h.scopeTree)
		r.With(g.Perm(access.OrganizationView)).Get("/stores", h.listStores)
		r.With(g.Perm(access.OrganizationView)).Get("/stores/{id}", h.getStore)
		r.With(g.Perm(access.OrganizationStoreCreate)).Post("/stores", h.createStore)
		r.With(g.Perm(access.OrganizationStoreUpdate)).Patch("/stores/{id}", h.updateStore)
		r.With(g.Perm(access.OrganizationUnitManage)).Post("/units", h.createUnit)
	})
}

func (h *Handler) scopeTree(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ScopeTree(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) listStores(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListStores(r.Context(), ListStoresFilter{Status: r.URL.Query().Get("status"), UnderPath: r.URL.Query().Get("under_path")})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) getStore(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.GetStore(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type createStoreRequest struct {
	Code           string                     `json:"code" validate:"required"`
	Name           string                     `json:"name" validate:"required,max=120"`
	ParentUnitID   *uuid.UUID                 `json:"parent_org_unit_id"`
	Address        json.RawMessage            `json:"address"`
	Timezone       string                     `json:"timezone"`
	OperatingHours json.RawMessage            `json:"operating_hours"`
	Settings       map[string]json.RawMessage `json:"settings"`
}

func (h *Handler) createStore(w http.ResponseWriter, r *http.Request) {
	var req createStoreRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.ProvisionStore(r.Context(), ProvisionInput{
		Code: req.Code, Name: req.Name, ParentUnitID: req.ParentUnitID, Address: req.Address, Timezone: req.Timezone,
		OperatingHours: req.OperatingHours, Settings: req.Settings,
	})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type updateStoreRequest struct {
	Name           *string         `json:"name" validate:"omitempty,max=120"`
	Address        json.RawMessage `json:"address"`
	Timezone       *string         `json:"timezone"`
	OperatingHours json.RawMessage `json:"operating_hours"`
	Status         *string         `json:"status" validate:"omitempty,oneof=active inactive closed"`
}

func (h *Handler) updateStore(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req updateStoreRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.UpdateStore(r.Context(), id, UpdateStoreInput{Name: req.Name, Address: req.Address, Timezone: req.Timezone, OperatingHours: req.OperatingHours, Status: req.Status})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type createUnitRequest struct {
	ParentID uuid.UUID `json:"parent_id" validate:"required"`
	Type     string    `json:"type" validate:"required,oneof=business_unit region"`
	Code     string    `json:"code" validate:"required"`
	Name     string    `json:"name" validate:"required,max=120"`
}

func (h *Handler) createUnit(w http.ResponseWriter, r *http.Request) {
	var req createUnitRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.CreateUnit(r.Context(), CreateUnitInput{ParentID: req.ParentID, Type: req.Type, Code: req.Code, Name: req.Name})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

var _ = apperr.BadRequest
