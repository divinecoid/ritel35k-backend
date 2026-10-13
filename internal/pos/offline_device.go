package pos

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/httpx"
	"github.com/ritel35k/backend/internal/httpx/response"
	"github.com/ritel35k/backend/internal/platform/reqmeta"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/posoffline"
)

// Device is the terminal-facing API for the edge agent: it authenticates with the terminal's own token (trm_…),
// never a user session, and can only read its store's catalogue and replay its own commands.
type Device struct {
	httpx.Base
	tx  *tx.Manager
	svc *Service
}

func NewDevice(b httpx.Base, t *tx.Manager, s *Service) *Device {
	return &Device{Base: b, tx: t, svc: s}
}

type terminalCtx struct{}

func terminalFrom(ctx context.Context) gen.GetTerminalBySyncTokenRow {
	return ctx.Value(terminalCtx{}).(gen.GetTerminalBySyncTokenRow)
}

func (d *Device) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(tok, SyncTokenPrefix) || len(tok) > 100 {
			d.Fail(w, r, apperr.Unauthenticated("terminal token required"))
			return
		}
		t, err := d.tx.Queries().GetTerminalBySyncToken(r.Context(), HashSyncToken(tok))
		if err != nil {
			d.Fail(w, r, apperr.Unauthenticated("unknown terminal token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), terminalCtx{}, t)))
	})
}

// Mount adds /pos/snapshot and /pos/sync under the caller's /edge router.
func (d *Device) Mount(r chi.Router) {
	r.Route("/pos", func(r chi.Router) {
		r.Use(d.Auth)
		r.Get("/snapshot", d.snapshot)
		r.Post("/sync", d.sync)
	})
}

// snapshot returns the catalogue for the terminal's store; ?have=<version> answers "unchanged" without the payload.
func (d *Device) snapshot(w http.ResponseWriter, r *http.Request) {
	t := terminalFrom(r.Context())
	if t.Status != "active" || t.StoreStatus != "active" {
		d.Fail(w, r, apperr.Conflict("TERMINAL_INACTIVE", "Terminal or store is not active"))
		return
	}
	snap, err := BuildSnapshot(r.Context(), d.tx.Queries(), t.CompanyID, t.ID, time.Now())
	if err != nil {
		d.Fail(w, r, err)
		return
	}
	if have := r.URL.Query().Get("have"); have != "" && have == snap.Version {
		response.OK(w, map[string]any{"unchanged": true, "version": snap.Version})
		return
	}
	_ = d.tx.Queries().TouchTerminalSync(r.Context(), gen.TouchTerminalSyncParams{TerminalID: t.ID, Ip: reqmeta.From(r.Context()).IP, SnapshotVersion: snap.Version, Pending: -1})
	response.OK(w, snap)
}

func (d *Device) sync(w http.ResponseWriter, r *http.Request) {
	t := terminalFrom(r.Context())
	var req posoffline.SyncRequest
	if err := httpx.Bind(r, &req); err != nil {
		d.Fail(w, r, err)
		return
	}
	if len(req.Commands) > 200 {
		d.Fail(w, r, apperr.Validation(map[string]string{"commands": "at most 200 per request"}))
		return
	}
	results := d.svc.ApplyCommands(r.Context(), t.CompanyID, t.ID, req.Commands)
	_ = d.tx.Queries().TouchTerminalSync(r.Context(), gen.TouchTerminalSyncParams{TerminalID: t.ID, Ip: reqmeta.From(r.Context()).IP, Version: req.AgentVersion, Pending: int32(max(req.Pending-len(results), 0))})
	response.OK(w, posoffline.SyncResponse{Results: results, ServerTime: time.Now().UTC()})
}
