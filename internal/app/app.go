// Package app is the composition root: it wires modules together and builds the HTTP router.
package app

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/ritel35k/backend/internal/approval"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/auth"
	"github.com/ritel35k/backend/internal/auth/token"
	"github.com/ritel35k/backend/internal/catalog"
	"github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/middleware"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/inventory"
	"github.com/ritel35k/backend/internal/notification"
	"github.com/ritel35k/backend/internal/organization"
	"github.com/ritel35k/backend/internal/platform/config"
	"github.com/ritel35k/backend/internal/platform/eventbus"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/pos"
	"github.com/ritel35k/backend/internal/pricing"
	"github.com/ritel35k/backend/internal/product"
	"github.com/ritel35k/backend/internal/promotion"
	"github.com/ritel35k/backend/internal/purchasing"
	"github.com/ritel35k/backend/internal/report"
	"github.com/ritel35k/backend/internal/rfid"
	"github.com/ritel35k/backend/internal/role"
	"github.com/ritel35k/backend/internal/security"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/internal/shift"
	"github.com/ritel35k/backend/internal/stockcount"
	"github.com/ritel35k/backend/internal/stream"
	"github.com/ritel35k/backend/internal/summary"
	"github.com/ritel35k/backend/internal/supplier"
	"github.com/ritel35k/backend/internal/transfer"
	"github.com/ritel35k/backend/internal/user"
	"github.com/ritel35k/backend/internal/voucher"
	"github.com/ritel35k/backend/internal/warehouse"
)

type App struct {
	Cfg  config.Config
	Pool *pgxpool.Pool
	Log  zerolog.Logger
	Tx   *tx.Manager
	Auth *auth.Service
	Bus  *eventbus.Bus
	Hub  *stream.Hub
}

func New(cfg config.Config, pool *pgxpool.Pool, log zerolog.Logger) *App {
	t := tx.NewManager(pool)
	tm := token.NewManager(cfg.JWTSecret, cfg.AccessTTL)
	a := &App{Cfg: cfg, Pool: pool, Log: log, Tx: t, Auth: auth.NewService(t, tm, cfg.RefreshTTL), Bus: eventbus.New(), Hub: stream.NewHub(log)}
	a.subscribe()
	return a
}

// subscribe registers outbox consumers. Handlers must be idempotent.
func (a *App) subscribe() {
	summary.Register(a.Bus, a.Tx)
	notification.Register(a.Bus, a.Tx)
	a.Bus.Subscribe(organization.EventStoreProvisioned, "log.store_provisioned", func(_ context.Context, e eventbus.Event) error {
		a.Log.Info().Int64("event_id", e.ID).Str("store", e.AggregateID.String()).Msg("store provisioned")
		return nil
	})
}

func (a *App) Relay() *outbox.Relay { return outbox.NewRelay(a.Tx, a.Bus, a.Log) }

func (a *App) Handler() http.Handler {
	log := &a.Log
	base := httpx.Base{Log: log}
	tm := token.NewManager(a.Cfg.JWTSecret, a.Cfg.AccessTTL)
	guard := &middleware.Guard{Tokens: tm, Loader: a.Auth, Log: log}

	r := chi.NewRouter()
	r.Use(middleware.RequestID(a.Cfg.TrustProxy), middleware.Recoverer(log), middleware.AccessLog(log), middleware.SecureHeaders)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: a.Cfg.CORSOrigins, AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "X-Request-ID", "X-Device-ID", "Idempotency-Key"},
		ExposedHeaders: []string{"X-Request-ID"}, MaxAge: 600,
	}))

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) { response.OK(w, map[string]string{"status": "ok"}) })
	r.Get("/ready", func(w http.ResponseWriter, req *http.Request) {
		if err := a.Pool.Ping(req.Context()); err != nil {
			response.Error(w, req, log, err)
			return
		}
		response.OK(w, map[string]string{"status": "ready"})
	})

	authH := auth.NewHandler(base, a.Auth)
	r.Route("/api/v1", func(r chi.Router) {
		// store devices (RFID readers/gates) authenticate with their own token, never a user session
		rfid.NewDevice(base, a.Tx).Routes(r)
		r.Group(func(r chi.Router) {
			r.Use(middleware.NewIPRateLimit(a.Cfg.AuthRatePerMin, a.Cfg.AuthRateBurst, a.Cfg.TrustProxy, log).Middleware)
			authH.PublicRoutes(r)
		})
		r.Group(func(r chi.Router) {
			r.Use(guard.Authenticate)
			authH.Routes(r, guard)
			organization.NewHandler(base, organization.NewService(a.Tx)).Routes(r, guard)
			settings.NewHandler(base, settings.NewService(a.Tx)).Routes(r, guard)
			role.NewHandler(base, role.NewService(a.Tx, a.Auth)).Routes(r, guard)
			user.NewHandler(base, user.NewService(a.Tx, a.Auth)).Routes(r, guard)
			warehouse.NewHandler(base, warehouse.NewService(a.Tx)).Routes(r, guard)
			catalog.NewHandler(base, catalog.NewService(a.Tx)).Routes(r, guard)
			product.NewHandler(base, product.NewService(a.Tx)).Routes(r, guard)
			supplier.NewHandler(base, supplier.NewService(a.Tx)).Routes(r, guard)
			audit.NewHandler(base, a.Tx).Routes(r, guard)
			inventory.NewHandler(base, inventory.NewService(a.Tx)).Routes(r, guard)
			pricing.NewHandler(base, pricing.NewService(a.Tx)).Routes(r, guard)
			shift.NewHandler(base, shift.NewService(a.Tx)).Routes(r, guard)
			pos.NewHandler(base, pos.NewService(a.Tx, approver{a.Auth})).Routes(r, guard)
			purchasing.NewHandler(base, purchasing.NewService(a.Tx)).Routes(r, guard)
			transfer.NewHandler(base, transfer.NewService(a.Tx)).Routes(r, guard)
			stockcount.NewHandler(base, stockcount.NewService(a.Tx)).Routes(r, guard)
			customer.NewHandler(base, customer.NewService(a.Tx)).Routes(r, guard)
			promotion.NewHandler(base, promotion.NewService(a.Tx)).Routes(r, guard)
			voucher.NewHandler(base, voucher.NewService(a.Tx)).Routes(r, guard)
			report.NewHandler(base, report.NewService(a.Tx)).Routes(r, guard)
			notification.NewHandler(base, notification.NewService(a.Tx)).Routes(r, guard)
			approval.NewHandler(base, approval.NewService(a.Tx)).Routes(r, guard)
			stream.NewHandler(a.Hub).Routes(r, guard)
			rfid.NewHandler(base, rfid.NewService(a.Tx)).Routes(r, guard)
			security.NewHandler(base, security.NewService(a.Tx), security.NewOverviewer(a.Tx)).Routes(r, guard)
		})
	})
	return r
}

// approver adapts auth.Service to pos.Approver so the POS does not depend on the auth package.
type approver struct{ a *auth.Service }

func (x approver) Approve(ctx context.Context, email, pin, permission string, storeID uuid.UUID) (uuid.UUID, error) {
	ap, err := x.a.ApproveWithPIN(ctx, email, pin, permission, storeID)
	if err != nil {
		return uuid.Nil, err
	}
	return ap.ApproverID, nil
}
