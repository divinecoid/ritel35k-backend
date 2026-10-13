// Package security holds the loss-prevention record: RFID alarms (unauthorized exit, mismatch, unknown tag) and rule
// hits raised from sales events (excessive voids, large refunds, high discounts, cash variance, reader offline).
// Events are written by other modules through Record; people work them (acknowledge → resolve / false alarm) here.
package security

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

const (
	PermView    = "security.view"
	PermResolve = "security.resolve"
)

// Event types.
const (
	UnauthorizedExit = "UNAUTHORIZED_EXIT"
	UnknownTag       = "UNKNOWN_TAG"
	RFIDMismatch     = "RFID_MISMATCH"
	ExcessiveVoids   = "EXCESSIVE_VOIDS"
	LargeRefund      = "LARGE_REFUND"
	HighDiscount     = "HIGH_DISCOUNT"
	CashVariance     = "CASH_VARIANCE"
	ReaderOffline    = "READER_OFFLINE"
)

var Types = []string{UnauthorizedExit, UnknownTag, RFIDMismatch, ExcessiveVoids, LargeRefund, HighDiscount, CashVariance, ReaderOffline}

type Event struct {
	CompanyID  uuid.UUID
	StoreID    uuid.UUID
	ReaderID   *uuid.UUID
	Type       string
	Severity   string // info | warning | critical
	Title      string
	EPC        string
	VariantID  *uuid.UUID
	SaleID     *uuid.UUID
	UserID     *uuid.UUID
	Detail     any
	Dedupe     string // same key → recorded once
	OccurredAt time.Time
}

// Record writes an event on the caller's transaction. created=false means the dedupe key already existed.
func Record(ctx context.Context, q *gen.Queries, e Event) (id uuid.UUID, created bool, err error) {
	if e.Severity == "" {
		e.Severity = "warning"
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	detail := []byte("{}")
	if e.Detail != nil {
		if b, merr := json.Marshal(e.Detail); merr == nil {
			detail = b
		}
	}
	id, _ = uuid.NewV7()
	got, err := q.InsertSecurityEvent(ctx, gen.InsertSecurityEventParams{ID: id, CompanyID: e.CompanyID, StoreID: e.StoreID, ReaderID: e.ReaderID, Type: e.Type,
		Severity: e.Severity, Title: e.Title, Epc: e.EPC, VariantID: e.VariantID, SaleID: e.SaleID, UserID: e.UserID, Detail: detail, DedupeKey: e.Dedupe, OccurredAt: e.OccurredAt})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return got, true, nil
}

type Service struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewService(t *tx.Manager) *Service { return &Service{tx: t, now: time.Now} }

// dayLocation is how a calendar day in a filter maps to instants. Stores default to Asia/Jakarta.
func dayLocation() *time.Location {
	if l, err := time.LoadLocation("Asia/Jakarta"); err == nil {
		return l
	}
	return time.FixedZone("WIB", 7*3600)
}

type Filter struct {
	StoreID   *uuid.UUID
	UnderPath string
	Status    string // open | acknowledged | resolved | false_alarm | active (= open+acknowledged)
	Type      string
	Severity  string
	From, To  time.Time // inclusive calendar days
}

func (f Filter) window() (time.Time, time.Time) {
	loc := dayLocation()
	from := time.Date(f.From.Year(), f.From.Month(), f.From.Day(), 0, 0, 0, 0, loc)
	to := time.Date(f.To.Year(), f.To.Month(), f.To.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return from, to
}

func (s *Service) List(ctx context.Context, f Filter, pg pagination.Params) ([]gen.ListSecurityEventsRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	sc := p.Grants.Scope(PermView)
	if sc.Empty() {
		return nil, pagination.Meta{}, apperr.Forbidden("no access to security events")
	}
	all, paths := sc.SQL()
	from, to := f.window()
	q := s.tx.Queries()
	rows, err := q.ListSecurityEvents(ctx, gen.ListSecurityEventsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath,
		Status: f.Status, Type: f.Type, Severity: f.Severity, FromTime: from, ToTime: to, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountSecurityEvents(ctx, gen.CountSecurityEventsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, UnderPath: f.UnderPath,
		Status: f.Status, Type: f.Type, Severity: f.Severity, FromTime: from, ToTime: to})
	return rows, pagination.NewMeta(pg, total), err
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*gen.GetSecurityEventRow, error) {
	p := access.MustPrincipal(ctx)
	e, err := s.tx.Queries().GetSecurityEvent(ctx, gen.GetSecurityEventParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(PermView, e.Path)) {
		return nil, apperr.NotFound("security event")
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// SetStatus moves an event through its workflow. Closing (resolved / false_alarm) needs a note: loss prevention
// must be able to say afterwards why an alarm was dismissed.
func (s *Service) SetStatus(ctx context.Context, id uuid.UUID, status, note string) (*gen.GetSecurityEventRow, error) {
	p := access.MustPrincipal(ctx)
	note = strings.TrimSpace(note)
	if (status == "resolved" || status == "false_alarm") && len(note) < 3 {
		return nil, apperr.Validation(map[string]string{"note": "explain how this was resolved"})
	}
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		e, err := q.GetSecurityEvent(ctx, gen.GetSecurityEventParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(PermResolve, e.Path)) {
			return apperr.NotFound("security event")
		}
		if err != nil {
			return err
		}
		if e.Status == "resolved" || e.Status == "false_alarm" {
			return apperr.Conflict("ALREADY_CLOSED", "This event is already closed")
		}
		if status == "acknowledged" && e.Status == "acknowledged" {
			return apperr.Conflict("ALREADY_ACKNOWLEDGED", "Already acknowledged")
		}
		if _, err := q.UpdateSecurityEventStatus(ctx, gen.UpdateSecurityEventStatusParams{ID: id, CompanyID: p.CompanyID, Status: status, By: p.UserID, Note: note}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: &e.StoreID, Action: "SECURITY_EVENT_" + strings.ToUpper(status), Entity: "security_event", EntityID: id.String(),
			Old: map[string]any{"status": e.Status}, New: map[string]any{"status": status, "note": note, "type": e.Type}})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

type Handler struct {
	httpx.Base
	svc *Service
	ov  *Overviewer
}

func NewHandler(b httpx.Base, s *Service, ov *Overviewer) *Handler {
	return &Handler{Base: b, svc: s, ov: ov}
}

func (h *Handler) Routes(r chi.Router, g *middleware.Guard) {
	r.Route("/security", func(r chi.Router) {
		r.With(g.Perm(PermView)).Get("/overview", h.overview)
		r.With(g.Perm(PermView)).Get("/events", h.list)
		r.With(g.Perm(PermView)).Get("/events/{id}", h.get)
		r.With(g.Perm(PermResolve)).Post("/events/{id}/status", h.setStatus)
	})
}

func parseDay(s string, def time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return t, apperr.BadRequest("dates must be YYYY-MM-DD")
	}
	return t, nil
}

func (h *Handler) filter(r *http.Request) (Filter, error) {
	q := r.URL.Query()
	now := time.Now().In(dayLocation())
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var f Filter
	var err error
	if f.To, err = parseDay(q.Get("to"), today); err != nil {
		return f, err
	}
	if f.From, err = parseDay(q.Get("from"), f.To.AddDate(0, 0, -6)); err != nil {
		return f, err
	}
	if f.From.After(f.To) || f.To.Sub(f.From) > 400*24*time.Hour {
		return f, apperr.BadRequest("invalid date range")
	}
	if f.UnderPath = q.Get("under_path"); f.UnderPath != "" && !pathOK(f.UnderPath) {
		return f, apperr.BadRequest("invalid under_path")
	}
	f.Status, f.Type, f.Severity = q.Get("status"), q.Get("type"), q.Get("severity")
	switch f.Status {
	case "", "active", "open", "acknowledged", "resolved", "false_alarm":
	default:
		return f, apperr.BadRequest("invalid status")
	}
	if f.StoreID, err = httpx.QueryUUID(r, "store_id"); err != nil {
		return f, err
	}
	return f, nil
}

func pathOK(s string) bool {
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return false
		}
		for _, c := range seg {
			if !(c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				return false
			}
		}
	}
	return true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	f, err := h.filter(r)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	rows, meta, err := h.svc.List(r.Context(), f, pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, rows, meta)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	e, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, e)
}

type statusRequest struct {
	Status string `json:"status" validate:"required,oneof=acknowledged resolved false_alarm"`
	Note   string `json:"note" validate:"max=500"`
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	var req statusRequest
	if err := httpx.Bind(r, &req); err != nil {
		h.Fail(w, r, err)
		return
	}
	e, err := h.svc.SetStatus(r.Context(), id, req.Status, req.Note)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, e)
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	f, err := h.filter(r)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	out, err := h.ov.Overview(r.Context(), f)
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, out)
}
