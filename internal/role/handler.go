package role

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.With(g.Perm(access.RoleView)).Get("/roles", h.list)
	r.With(g.Perm(access.RoleView)).Get("/permissions", h.permissions)
	r.With(g.Perm(access.RoleManage)).Post("/roles", h.create)
	r.With(g.Perm(access.RoleManage)).Put("/roles/{id}", h.update)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.List(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) permissions(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Permissions(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type createRequest struct {
	Code        string   `json:"code" validate:"required"`
	Name        string   `json:"name" validate:"required,max=80"`
	Permissions []string `json:"permissions" validate:"required"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), req.Code, req.Name, req.Permissions)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type updateRequest struct {
	Name        *string  `json:"name" validate:"omitempty,max=80"`
	Permissions []string `json:"permissions" validate:"required"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req updateRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.SetPermissions(r.Context(), id, req.Name, req.Permissions); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}
