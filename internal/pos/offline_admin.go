package pos

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/security"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

const SyncTokenPrefix = "trm_"

func newSyncToken() (token, hash, hint string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = SyncTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashSyncToken(token), token[len(token)-4:]
}

func HashSyncToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// IssueSyncToken creates (or rotates) the credential the terminal's edge agent syncs with. Shown once.
func (s *Service) IssueSyncToken(ctx context.Context, terminalID uuid.UUID) (token, hint string, err error) {
	p := access.MustPrincipal(ctx)
	token, hash, hint := newSyncToken()
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		t, err := q.GetTerminal(ctx, gen.GetTerminalParams{ID: terminalID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(PermTerminal, t.Path)) {
			return apperr.NotFound("terminal")
		}
		if err != nil {
			return err
		}
		if err := q.UpsertTerminalSyncToken(ctx, gen.UpsertTerminalSyncTokenParams{TerminalID: terminalID, CompanyID: p.CompanyID, TokenHash: hash, TokenHint: hint}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: &t.StoreID, Action: "TERMINAL_SYNC_TOKEN_ISSUED", Entity: "terminal", EntityID: terminalID.String(), New: map[string]any{"hint": hint}})
	})
	if err != nil {
		return "", "", err
	}
	return token, hint, nil
}

// SyncStatus is when each terminal's edge last synced and how much it still holds back (visible per terminal scope).
func (s *Service) SyncStatus(ctx context.Context, storeID *uuid.UUID) ([]gen.ListTerminalSyncStatusRow, error) {
	p := access.MustPrincipal(ctx)
	terms, err := s.Terminals(ctx, storeID)
	if err != nil {
		return nil, err
	}
	visible := map[uuid.UUID]bool{}
	for _, t := range terms {
		visible[t.ID] = true
	}
	rows, err := s.tx.Queries().ListTerminalSyncStatus(ctx, p.CompanyID)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if visible[r.TerminalID] {
			out = append(out, r)
		}
	}
	return out, nil
}

type ExceptionFilter struct {
	StoreID *uuid.UUID
	Status  string
	Type    string
}

func (s *Service) Exceptions(ctx context.Context, f ExceptionFilter, pg pagination.Params) ([]gen.ListOfflineExceptionsRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	sc := p.Grants.Scope(security.PermView)
	if sc.Empty() {
		return nil, pagination.Meta{}, apperr.Forbidden("no access to offline exceptions")
	}
	all, paths := sc.SQL()
	q := s.tx.Queries()
	rows, err := q.ListOfflineExceptions(ctx, gen.ListOfflineExceptionsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, Status: f.Status, Type: f.Type,
		PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountOfflineExceptions(ctx, gen.CountOfflineExceptionsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID, Status: f.Status, Type: f.Type})
	return rows, pagination.NewMeta(pg, total), err
}

// ReviewException closes an exception as reviewed (accepted as is) or dismissed (false alarm), with a note.
func (s *Service) ReviewException(ctx context.Context, id uuid.UUID, status, note string) error {
	p := access.MustPrincipal(ctx)
	if status != "reviewed" && status != "dismissed" {
		return apperr.Validation(map[string]string{"status": "reviewed or dismissed"})
	}
	return s.tx.Do(ctx, func(q *gen.Queries) error {
		e, err := q.GetOfflineException(ctx, gen.GetOfflineExceptionParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(security.PermResolve, e.Path)) {
			return apperr.NotFound("exception")
		}
		if err != nil {
			return err
		}
		if e.Status != "open" {
			return apperr.Conflict("ALREADY_REVIEWED", "Exception was already reviewed")
		}
		if err := q.ReviewOfflineException(ctx, gen.ReviewOfflineExceptionParams{ID: id, Status: status, ReviewedBy: &p.UserID, ReviewNote: note}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: &e.StoreID, Action: "OFFLINE_EXCEPTION_REVIEWED", Entity: "offline_exception", EntityID: id.String(), New: map[string]any{"status": status, "note": note}})
	})
}
