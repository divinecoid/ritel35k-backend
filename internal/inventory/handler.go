package inventory

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/inventory", func(r chi.Router) {
		r.With(g.Perm(access.InventoryView)).Get("/balances", h.balances)
		r.With(g.Perm(access.InventoryView)).Get("/movements", h.movements)
		r.With(g.Perm(access.InventoryAdjust)).Post("/adjustments", h.adjust)
	})
}

func (h *Handler) balances(w http.ResponseWriter, r *http.Request) {
	sid, e1 := httpx.QueryUUID(r, "store_id")
	wid, e2 := httpx.QueryUUID(r, "warehouse_id")
	vid, e3 := httpx.QueryUUID(r, "variant_id")
	for _, e := range []error{e1, e2, e3} {
		if e != nil {
			h.Fail(w, r, e)
			return
		}
	}
	pg := pagination.FromRequest(r)
	rows, meta, err := h.svc.Balances(r.Context(), BalanceFilter{StoreID: sid, WarehouseID: wid, VariantID: vid, UnderPath: r.URL.Query().Get("under_path"),
		Q: r.URL.Query().Get("q"), LowOnly: r.URL.Query().Get("low") == "true"}, pg)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, rows, meta)
}

func parseTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, apperr.BadRequest("time must be RFC3339")
	}
	return &t, nil
}

func (h *Handler) movements(w http.ResponseWriter, r *http.Request) {
	sid, e1 := httpx.QueryUUID(r, "store_id")
	vid, e2 := httpx.QueryUUID(r, "variant_id")
	from, e3 := parseTime(r.URL.Query().Get("from"))
	to, e4 := parseTime(r.URL.Query().Get("to"))
	for _, e := range []error{e1, e2, e3, e4} {
		if e != nil {
			h.Fail(w, r, e)
			return
		}
	}
	rows, err := h.svc.Movements(r.Context(), MovementFilter{StoreID: sid, VariantID: vid, Type: r.URL.Query().Get("type"), From: from, To: to}, pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, rows)
}

type adjustRequest struct {
	StoreID     uuid.UUID  `json:"store_id" validate:"required"`
	WarehouseID *uuid.UUID `json:"warehouse_id"`
	Type        string     `json:"type" validate:"required,oneof=OPENING ADJUSTMENT DAMAGE"`
	Reason      string     `json:"reason" validate:"required,max=200"`
	Lines       []struct {
		VariantID uuid.UUID       `json:"variant_id" validate:"required"`
		QtyDelta  decimal.Decimal `json:"qty_delta"`
		UnitCost  decimal.Decimal `json:"unit_cost"`
	} `json:"lines" validate:"required,min=1,max=500,dive"`
}

func (h *Handler) adjust(w http.ResponseWriter, r *http.Request) {
	var req adjustRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	in := AdjustInput{StoreID: req.StoreID, WarehouseID: req.WarehouseID, Type: req.Type, Reason: req.Reason}
	for _, l := range req.Lines {
		in.Lines = append(in.Lines, AdjustLine{VariantID: l.VariantID, QtyDelta: l.QtyDelta, UnitCost: l.UnitCost})
	}
	out, err := h.svc.Adjust(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}
