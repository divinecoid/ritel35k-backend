package audit

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Handler struct {
	httpx.Base
	tx *tx.Manager
}

func NewHandler(b httpx.Base, t *tx.Manager) *Handler { return &Handler{Base: b, tx: t} }

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.With(g.Perm(access.AuditView)).Get("/audit-logs", h.list)
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

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p := access.MustPrincipal(r.Context())
	sid, err := httpx.QueryUUID(r, "store_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	uid, err := httpx.QueryUUID(r, "user_id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	from, err := parseTime(r.URL.Query().Get("from"))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	to, err := parseTime(r.URL.Query().Get("to"))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	pg := pagination.FromRequest(r)
	all, paths := p.Grants.Scope(access.AuditView).SQL()
	rows, err := h.tx.Queries().ListAuditLogs(r.Context(), gen.ListAuditLogsParams{
		CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, Action: r.URL.Query().Get("action"), Entity: r.URL.Query().Get("entity"),
		UserID: uid, StoreID: sid, FromTime: from, ToTime: to, PageLimit: pg.Limit(), PageOffset: pg.Offset(),
	})
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, rows)
}
