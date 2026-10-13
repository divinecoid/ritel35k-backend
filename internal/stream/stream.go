// Package stream pushes "something changed" signals to browsers over SSE.
// Any process (API or worker) raises a signal with Notify inside its transaction (Postgres NOTIFY, delivered on commit);
// every API instance LISTENs and fans the signal out to the connected users of that company. Signals carry no data, so
// a client simply refetches through the normal, permission-checked endpoints.
package stream

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/httpx/middleware"
)

const Channel = "ritel_events"

const (
	KindDashboard    = "dashboard"
	KindNotification = "notification"
)

type Event struct {
	Kind      string     `json:"k"`
	CompanyID uuid.UUID  `json:"c"`
	UserID    *uuid.UUID `json:"u,omitempty"` // nil = everyone in the company
}

// Notify queues a signal on the caller's transaction; listeners receive it after commit.
func Notify(ctx context.Context, q *gen.Queries, e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return q.PgNotify(ctx, gen.PgNotifyParams{Channel: Channel, Payload: string(b)})
}

type sub struct {
	company, user uuid.UUID
	ch            chan string
}

type Hub struct {
	log  zerolog.Logger
	mu   sync.Mutex
	subs map[*sub]struct{}
}

func NewHub(log zerolog.Logger) *Hub { return &Hub{log: log, subs: map[*sub]struct{}{}} }

func (h *Hub) subscribe(company, user uuid.UUID) (*sub, func()) {
	s := &sub{company: company, user: user, ch: make(chan string, 8)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s, func() { h.mu.Lock(); delete(h.subs, s); h.mu.Unlock() }
}

func (h *Hub) dispatch(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.company != e.CompanyID || (e.UserID != nil && *e.UserID != s.user) {
			continue
		}
		select {
		case s.ch <- e.Kind:
		default: // a slow client already has a pending signal; signals are idempotent so dropping is fine
		}
	}
}

// Run holds one pooled connection in LISTEN mode and reconnects with backoff until ctx ends.
func (h *Hub) Run(ctx context.Context, pool *pgxpool.Pool) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := h.listen(ctx, pool)
		if ctx.Err() != nil {
			return
		}
		h.log.Error().Err(err).Dur("retry_in", backoff).Msg("stream listener lost")
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (h *Hub) listen(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var e Event
		if json.Unmarshal([]byte(n.Payload), &e) == nil {
			h.dispatch(e)
		}
	}
}

type Handler struct{ hub *Hub }

func NewHandler(h *Hub) *Handler { return &Handler{hub: h} }

func (h *Handler) Routes(r chi.Router, _ *middleware.Guard) {
	r.Get("/stream/events", h.events)
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	p := access.MustPrincipal(r.Context())
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // the server's WriteTimeout must not cut a long-lived stream
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("retry: 3000\n: connected\n\n"))
	if rc.Flush() != nil {
		return
	}
	s, cancel := h.hub.subscribe(p.CompanyID, p.UserID)
	defer cancel()
	beat := time.NewTicker(25 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case k := <-s.ch:
			_, _ = w.Write([]byte("event: " + k + "\ndata: {}\n\n"))
		case <-beat.C:
			_, _ = w.Write([]byte(": ping\n\n"))
		}
		if rc.Flush() != nil {
			return
		}
	}
}
