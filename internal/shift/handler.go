package shift

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
	r.Route("/pos/shifts", func(r chi.Router) {
		r.With(g.Perm("pos.shift.manage")).Post("/open", h.open)
		r.With(g.Perm("pos.shift.manage")).Get("/current", h.current)
		r.With(g.AnyPerm("pos.sale.view", "pos.shift.manage")).Get("/", h.list)
		r.With(g.AnyPerm("pos.sale.view", "pos.shift.manage")).Get("/{id}", h.get)
		r.With(g.Perm("pos.shift.manage")).Post("/{id}/cash-movements", h.cash)
		r.With(g.Perm("pos.shift.manage")).Post("/{id}/close", h.close)
	})
}

type openRequest struct {
	TerminalID  uuid.UUID       `json:"terminal_id" validate:"required"`
	OpeningCash decimal.Decimal `json:"opening_cash"`
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request) {
	var req openRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Open(r.Context(), req.TerminalID, req.OpeningCash)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

func (h *Handler) current(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Current(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out) // null when no open shift
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

type cashRequest struct {
	Type   string          `json:"type" validate:"required,oneof=CASH_IN CASH_OUT"`
	Amount decimal.Decimal `json:"amount"`
	Reason string          `json:"reason" validate:"required,max=200"`
}

func (h *Handler) cash(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req cashRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.CashMovement(r.Context(), id, req.Type, req.Amount, req.Reason)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type closeRequest struct {
	CountedCash decimal.Decimal `json:"counted_cash"`
	Note        string          `json:"note" validate:"max=300"`
}

func (h *Handler) close(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req closeRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Close(r.Context(), id, req.CountedCash, req.Note)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
