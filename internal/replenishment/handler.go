package replenishment

import (
	"net/http"
	"strconv"

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
	r.With(g.Perm(PermView)).Get("/replenishment", h.suggest)
	r.With(g.Perm("purchase_order.create")).Post("/replenishment/draft-po", h.draft)
}

func (h *Handler) suggest(w http.ResponseWriter, r *http.Request) {
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	atoi := func(k string) int { n, _ := strconv.Atoi(r.URL.Query().Get(k)); return n }
	out, err := h.svc.Suggest(r.Context(), Filter{StoreID: sid, Params: Params{WindowDays: atoi("window_days"), CoverDays: atoi("cover_days")}})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) draft(w http.ResponseWriter, r *http.Request) {
	var in DraftInput
	if err := httpx.Bind(r, &in); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.DraftPOs(r.Context(), in)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}
