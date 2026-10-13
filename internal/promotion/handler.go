package promotion

import (
	"net/http"

	"github.com/go-chi/chi/v5"

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
	r.Route("/promotions", func(r chi.Router) {
		r.With(g.Perm(Perm)).Get("/", h.list)
		r.With(g.Perm(Perm)).Get("/{id}", h.get)
		r.With(g.Perm(Perm)).Post("/", h.create)
		r.With(g.Perm(Perm)).Put("/{id}", h.update)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.List(r.Context(), r.URL.Query().Get("status"), r.URL.Query().Get("q"))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
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

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var in Input
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Update(r.Context(), id, in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
