package notification

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

// List returns the caller's own notifications, newest first.
func (s *Service) List(ctx context.Context, unreadOnly bool, pg pagination.Params) ([]gen.ListNotificationsRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	rows, err := q.ListNotifications(ctx, gen.ListNotificationsParams{UserID: p.UserID, UnreadOnly: unreadOnly, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountNotifications(ctx, gen.CountNotificationsParams{UserID: p.UserID, UnreadOnly: unreadOnly})
	return rows, pagination.NewMeta(pg, total), err
}

func (s *Service) Unread(ctx context.Context) (int64, error) {
	p := access.MustPrincipal(ctx)
	return s.tx.Queries().CountNotifications(ctx, gen.CountNotificationsParams{UserID: p.UserID, UnreadOnly: true})
}

func (s *Service) MarkRead(ctx context.Context, id uuid.UUID) error {
	p := access.MustPrincipal(ctx)
	return s.tx.Queries().MarkNotificationRead(ctx, gen.MarkNotificationReadParams{ID: id, UserID: p.UserID})
}

func (s *Service) MarkAllRead(ctx context.Context) error {
	p := access.MustPrincipal(ctx)
	return s.tx.Queries().MarkAllNotificationsRead(ctx, p.UserID)
}

type Handler struct {
	httpx.Base
	svc *Service
}

func NewHandler(b httpx.Base, s *Service) *Handler { return &Handler{Base: b, svc: s} }

// Notifications belong to the signed-in user, so no permission is needed beyond authentication.
func (h *Handler) Routes(r chi.Router, _ *middleware.Guard) {
	r.Route("/notifications", func(r chi.Router) {
		r.Get("/", h.list)
		r.Get("/unread-count", h.unread)
		r.Post("/read-all", h.readAll)
		r.Post("/{id}/read", h.read)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	rows, meta, err := h.svc.List(r.Context(), r.URL.Query().Get("unread") == "true", pagination.FromRequest(r))
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OKMeta(w, rows, meta)
}

func (h *Handler) unread(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.Unread(r.Context())
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	response.OK(w, map[string]int64{"unread": n})
}

func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.MarkAllRead(r.Context()); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		h.Fail(w, r, err)
		return
	}
	if err := h.svc.MarkRead(r.Context(), id); err != nil {
		h.Fail(w, r, err)
		return
	}
	response.NoContent(w)
}
