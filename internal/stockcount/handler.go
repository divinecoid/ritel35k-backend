package stockcount

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

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
	anyOf := g.AnyPerm(PermCount, PermView, PermApprove)
	r.Route("/stock-counts", func(r chi.Router) {
		r.With(anyOf).Get("/", h.list)
		r.With(anyOf).Get("/{id}", h.id(func(r *http.Request, id uuid.UUID) (any, error) { return h.svc.Get(r.Context(), id) }))
		r.With(g.Perm(PermCount)).Post("/", h.create)
		r.With(g.Perm(PermCount)).Put("/{id}/items", h.setItems)
		r.With(g.Perm(PermCount)).Post("/{id}/scan", h.scan)
		r.With(g.Perm(PermCount)).Post("/{id}/rfid", h.scanRFID)
		r.With(g.Perm(PermCount)).Post("/{id}/submit", h.submit)
		r.With(g.Perm(PermApprove)).Post("/{id}/approve", h.id(func(r *http.Request, id uuid.UUID) (any, error) { return h.svc.Approve(r.Context(), id) }))
		r.With(g.AnyPerm(PermCount, PermApprove)).Post("/{id}/cancel", h.id(func(r *http.Request, id uuid.UUID) (any, error) { return h.svc.Cancel(r.Context(), id) }))
	})
}

func (h *Handler) id(fn func(r *http.Request, id uuid.UUID) (any, error)) http.HandlerFunc {
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

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.List(r.Context(), sid, r.URL.Query().Get("status"), pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in CreateInput
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

type itemsRequest struct {
	Items []CountEntry `json:"items" validate:"required,min=1,dive"`
}

func (h *Handler) setItems(w http.ResponseWriter, r *http.Request) {
	var req itemsRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	h.id(func(r *http.Request, id uuid.UUID) (any, error) { return h.svc.SetCounts(r.Context(), id, req.Items) })(w, r)
}

type scanRequest struct {
	Barcode string          `json:"barcode" validate:"required,max=64"`
	Qty     decimal.Decimal `json:"qty"`
}

func (h *Handler) scan(w http.ResponseWriter, r *http.Request) {
	var req scanRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	h.id(func(r *http.Request, id uuid.UUID) (any, error) {
		return h.svc.Scan(r.Context(), id, req.Barcode, req.Qty)
	})(w, r)
}

type submitRequest struct {
	Uncounted string `json:"uncounted" validate:"omitempty,oneof=zero ignore"`
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	var req submitRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	h.id(func(r *http.Request, id uuid.UUID) (any, error) { return h.svc.Submit(r.Context(), id, req.Uncounted) })(w, r)
}
