package user

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/users", func(r chi.Router) {
		r.With(g.Perm(access.UserView)).Get("/", h.list)
		r.With(g.Perm(access.UserView)).Get("/{id}", h.get)
		r.With(g.Perm(access.UserManage)).Post("/", h.create)
		r.With(g.Perm(access.UserManage)).Patch("/{id}", h.update)
		r.With(g.Perm(access.UserManage)).Put("/{id}/assignments", h.setAssignments)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	out, meta, err := h.svc.List(r.Context(), r.URL.Query().Get("q"), pg)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, out, meta)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type createRequest struct {
	Email       string       `json:"email" validate:"required,email"`
	Name        string       `json:"name" validate:"required,max=120"`
	Password    string       `json:"password" validate:"required,min=8"`
	PIN         string       `json:"pin" validate:"omitempty,numeric,min=4,max=8"`
	Assignments []Assignment `json:"assignments" validate:"dive"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), CreateInput{Email: req.Email, Name: req.Name, Password: req.Password, PIN: req.PIN, Assignments: req.Assignments})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type updateRequest struct {
	Name   *string `json:"name" validate:"omitempty,max=120"`
	Status *string `json:"status" validate:"omitempty,oneof=active inactive locked"`
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
	out, err := h.svc.Update(r.Context(), id, req.Name, req.Status)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type assignmentsRequest struct {
	Assignments []Assignment `json:"assignments" validate:"required,dive"`
}

func (h *Handler) setAssignments(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req assignmentsRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.SetAssignments(r.Context(), id, req.Assignments)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
