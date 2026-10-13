package transfer

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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
	anyOf := g.AnyPerm(PermTransfer, PermView, PermApprove)
	r.Route("/stock-transfers", func(r chi.Router) {
		r.With(anyOf).Get("/", h.list)
		r.With(anyOf).Get("/{id}", h.get)
		r.With(g.Perm(PermTransfer)).Post("/", h.create)
		r.With(g.Perm(PermTransfer)).Post("/{id}/submit", h.simple(func(r *http.Request, id uuid.UUID) (*Detail, error) { return h.svc.Submit(r.Context(), id) }))
		r.With(g.Perm(PermApprove)).Post("/{id}/approve", h.simple(func(r *http.Request, id uuid.UUID) (*Detail, error) { return h.svc.Approve(r.Context(), id) }))
		r.With(g.Perm(PermTransfer)).Post("/{id}/ship", h.ship)
		r.With(g.Perm(PermTransfer)).Post("/{id}/receive", h.receive)
		r.With(g.AnyPerm(PermTransfer, PermApprove)).Post("/{id}/cancel", h.cancel)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.List(r.Context(), r.URL.Query().Get("status"), pagination.FromRequest(r))
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

func (h *Handler) simple(fn func(r *http.Request, id uuid.UUID) (*Detail, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := httpx.PathUUID(r, "id")
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		out, err := fn(r, id)
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		response.OK(w, out)
	}
}

type shipRequest struct {
	Lines []ShipLine `json:"lines" validate:"dive"`
}

func (h *Handler) ship(w http.ResponseWriter, r *http.Request) {
	var req shipRequest
	if r.ContentLength != 0 {
		if err := httpx.Bind(r, &req); err != nil {
			h.Fail(w, r, err)
			return
		}
	}
	h.simple(func(r *http.Request, id uuid.UUID) (*Detail, error) { return h.svc.Ship(r.Context(), id, req.Lines) })(w, r)
}

type receiveRequest struct {
	Lines []ReceiveLine `json:"lines" validate:"dive"`
}

func (h *Handler) receive(w http.ResponseWriter, r *http.Request) {
	var req receiveRequest
	if r.ContentLength != 0 {
		if err := httpx.Bind(r, &req); err != nil {
			h.Fail(w, r, err)
			return
		}
	}
	h.simple(func(r *http.Request, id uuid.UUID) (*Detail, error) { return h.svc.Receive(r.Context(), id, req.Lines) })(w, r)
}

type cancelRequest struct {
	Reason string `json:"reason" validate:"required,max=200"`
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	h.simple(func(r *http.Request, id uuid.UUID) (*Detail, error) { return h.svc.Cancel(r.Context(), id, req.Reason) })(w, r)
}
