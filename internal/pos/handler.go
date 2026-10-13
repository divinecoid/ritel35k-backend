package pos

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/security"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/pos", func(r chi.Router) {
		r.With(g.AnyPerm(PermCreate, PermView, PermTerminal)).Get("/terminals", h.terminals)
		r.With(g.Perm(PermTerminal)).Post("/terminals", h.createTerminal)
		r.With(g.Perm(PermTerminal)).Patch("/terminals/{id}", h.updateTerminal)

		r.With(g.Perm(PermTerminal)).Post("/terminals/{id}/sync-token", h.syncToken)
		r.With(g.AnyPerm(PermCreate, PermView, PermTerminal)).Get("/sync-status", h.syncStatus)
		r.With(g.Perm(security.PermView)).Get("/exceptions", h.exceptions)
		r.With(g.Perm(security.PermResolve)).Post("/exceptions/{id}/review", h.reviewException)

		r.With(g.Perm(PermCreate)).Post("/quote", h.quote)
		r.With(g.Perm(PermCreate)).Post("/sales", h.checkout)
		r.With(g.AnyPerm(PermView, PermCreate)).Get("/sales", h.list)
		r.With(g.AnyPerm(PermView, PermCreate)).Get("/sales/{id}", h.get)
		r.With(g.AnyPerm(PermView, PermCreate)).Get("/sales/{id}/receipt", h.receipt)
		r.With(g.Perm(PermCreate)).Post("/sales/{id}/void", h.void)
		r.With(g.Perm(PermRefund)).Post("/sales/{id}/returns", h.ret)
	})
}

func (h *Handler) terminals(w http.ResponseWriter, r *http.Request) {
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Terminals(r.Context(), sid)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

type terminalRequest struct {
	StoreID uuid.UUID `json:"store_id" validate:"required"`
	Code    string    `json:"code" validate:"required,min=2,max=20"`
	Name    string    `json:"name" validate:"required,max=80"`
}

func (h *Handler) createTerminal(w http.ResponseWriter, r *http.Request) {
	var req terminalRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.CreateTerminal(r.Context(), req.StoreID, req.Code, req.Name)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

type terminalUpdate struct {
	Name   *string `json:"name" validate:"omitempty,max=80"`
	Status *string `json:"status" validate:"omitempty,oneof=active inactive"`
}

func (h *Handler) updateTerminal(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req terminalUpdate
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.UpdateTerminal(r.Context(), id, req.Name, req.Status)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) quote(w http.ResponseWriter, r *http.Request) {
	var req CartInput
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Quote(r.Context(), req)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) checkout(w http.ResponseWriter, r *http.Request) {
	var req CheckoutInput
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, replay, err := h.svc.Checkout(r.Context(), req)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
		response.OK(w, out)
		return
	}
	response.Created(w, out)
}

func parseDate(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, apperr.BadRequest("date must be YYYY-MM-DD")
	}
	return &t, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	sid, e1 := httpx.QueryUUID(r, "store_id")
	shid, e2 := httpx.QueryUUID(r, "shift_id")
	from, e3 := parseDate(r.URL.Query().Get("from"))
	to, e4 := parseDate(r.URL.Query().Get("to"))
	for _, e := range []error{e1, e2, e3, e4} {
		if e != nil {
			h.Fail(w, r, e)
			return
		}
	}
	out, meta, err := h.svc.List(r.Context(), SaleFilter{StoreID: sid, ShiftID: shid, UnderPath: r.URL.Query().Get("under_path"), Status: r.URL.Query().Get("status"),
		Q: r.URL.Query().Get("q"), From: from, To: to}, pagination.FromRequest(r))
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

func (h *Handler) receipt(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	rc, err := h.svc.Receipt(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if r.URL.Query().Get("format") == "text" {
		width := 32
		if r.URL.Query().Get("width") == "42" {
			width = 42
		} else if r.URL.Query().Get("width") == "48" {
			width = 48
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(rc.Text(width)))
		return
	}
	response.OK(w, rc)
}

type voidRequest struct {
	Reason   string         `json:"reason" validate:"required,max=200"`
	Approval *ApprovalInput `json:"approval"`
}

func (h *Handler) void(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req voidRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Void(r.Context(), id, req.Reason, req.Approval)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) ret(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req ReturnInput
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.Return(r.Context(), id, req)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, out)
}

func (h *Handler) syncToken(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	tok, hint, err := h.svc.IssueSyncToken(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.Created(w, map[string]string{"token": tok, "hint": hint})
}

func (h *Handler) syncStatus(w http.ResponseWriter, r *http.Request) {
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.svc.SyncStatus(r.Context(), sid)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}

func (h *Handler) exceptions(w http.ResponseWriter, r *http.Request) {
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, meta, err := h.svc.Exceptions(r.Context(), ExceptionFilter{StoreID: sid, Status: r.URL.Query().Get("status"), Type: r.URL.Query().Get("type")}, pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, out, meta)
}

type reviewRequest struct {
	Status string `json:"status" validate:"required,oneof=reviewed dismissed"`
	Note   string `json:"note" validate:"max=500"`
}

func (h *Handler) reviewException(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req reviewRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.ReviewException(r.Context(), id, req.Status, req.Note); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]string{"status": req.Status})
}
