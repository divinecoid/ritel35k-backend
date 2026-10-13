package customer

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

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
	view := g.AnyPerm(PermView, PermManage, "pos.transaction.create")
	r.Route("/customers", func(r chi.Router) {
		r.With(view).Get("/", h.list)
		r.With(view).Get("/lookup", h.lookup)
		r.With(view).Get("/{id}", h.get)
		r.With(g.AnyPerm(PermCreate, PermManage)).Post("/", h.create)
		r.With(g.Perm(PermManage)).Patch("/{id}", h.update)
		r.With(g.Perm(PermManage)).Post("/{id}/points", h.points)
	})
	r.With(g.AnyPerm(PermView, PermManage, "pricing.manage", "promotion.manage")).Get("/membership-levels", h.levels)
	r.With(g.Perm(PermManage)).Patch("/membership-levels/{id}", h.updateLevel)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	lvl, err := httpx.QueryUUID(r, "level_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, meta, err := h.svc.List(r.Context(), Filter{Q: r.URL.Query().Get("q"), LevelID: lvl, Status: r.URL.Query().Get("status")}, pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, out, meta)
}

func (h *Handler) lookup(w http.ResponseWriter, r *http.Request) {
	k := r.URL.Query().Get("key")
	if k == "" {
		h.Fail(w, r, apperr.BadRequest("key is required"))
		return
	}
	out, err := h.svc.Lookup(r.Context(), k)
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

type createRequest struct {
	Name      string     `json:"name" validate:"required,max=120"`
	Phone     string     `json:"phone" validate:"required,max=30"`
	Email     string     `json:"email" validate:"omitempty,email"`
	Birthday  *time.Time `json:"birthday"`
	Gender    string     `json:"gender" validate:"omitempty,oneof=M F"`
	Address   string     `json:"address" validate:"max=300"`
	Notes     string     `json:"notes" validate:"max=300"`
	HomeStore *uuid.UUID `json:"home_store_id"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Create(r.Context(), Input(req))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type updateRequest struct {
	Name     *string    `json:"name" validate:"omitempty,max=120"`
	Email    *string    `json:"email" validate:"omitempty,email"`
	Gender   *string    `json:"gender" validate:"omitempty,oneof=M F"`
	Address  *string    `json:"address" validate:"omitempty,max=300"`
	Notes    *string    `json:"notes" validate:"omitempty,max=300"`
	Status   *string    `json:"status" validate:"omitempty,oneof=active inactive"`
	Birthday *time.Time `json:"birthday"`
	LevelID  *uuid.UUID `json:"level_id"`
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
	out, err := h.svc.Update(r.Context(), id, UpdateInput{Name: req.Name, Email: req.Email, Gender: req.Gender, Address: req.Address, Notes: req.Notes, Status: req.Status, Birthday: req.Birthday, LevelID: req.LevelID})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type pointsRequest struct {
	Points int64  `json:"points"`
	Note   string `json:"note" validate:"required,max=200"`
}

func (h *Handler) points(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req pointsRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	bal, err := h.svc.AdjustPoints(r.Context(), id, req.Points, req.Note)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]int64{"points_balance": bal})
}

func (h *Handler) levels(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Levels(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type levelRequest struct {
	Name           *string          `json:"name" validate:"omitempty,max=60"`
	MinSpend       *decimal.Decimal `json:"min_spend"`
	EarnMultiplier *decimal.Decimal `json:"earn_multiplier"`
}

func (h *Handler) updateLevel(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req levelRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.UpdateLevel(r.Context(), id, req.Name, req.MinSpend, req.EarnMultiplier)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
