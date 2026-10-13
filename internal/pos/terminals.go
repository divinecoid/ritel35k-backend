package pos

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/pkg/apperr"
)

func (s *Service) Terminals(ctx context.Context, storeID *uuid.UUID) ([]gen.ListTerminalsRow, error) {
	p := access.MustPrincipal(ctx)
	// anyone who can sell, view sales or manage terminals sees the terminals of their stores
	sc := access.ScopeFilter{}
	for _, perm := range []string{PermCreate, PermView, PermTerminal} {
		g := p.Grants.Scope(perm)
		sc.All = sc.All || g.All
		sc.Paths = append(sc.Paths, g.Paths...)
	}
	all, paths := sc.SQL()
	return s.tx.Queries().ListTerminals(ctx, gen.ListTerminalsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: storeID})
}

func (s *Service) CreateTerminal(ctx context.Context, storeID uuid.UUID, code, name string) (*gen.PosTerminal, error) {
	p := access.MustPrincipal(ctx)
	var out gen.PosTerminal
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		st, err := q.GetStore(ctx, gen.GetStoreParams{ID: storeID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("store")
		}
		if err != nil {
			return err
		}
		if !p.Grants.Can(PermTerminal, st.Path) {
			return apperr.Forbidden("not allowed to manage terminals in this store")
		}
		id, _ := uuid.NewV7()
		out, err = q.CreateTerminal(ctx, gen.CreateTerminalParams{ID: id, CompanyID: p.CompanyID, StoreID: storeID, Code: code, Name: name, CreatedBy: &p.UserID})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return apperr.Conflict("TERMINAL_CODE_TAKEN", "Terminal code already used in this store")
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: &storeID, Action: "TERMINAL_CREATED", Entity: "terminal", EntityID: id.String(), New: out})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) UpdateTerminal(ctx context.Context, id uuid.UUID, name, status *string) (*gen.PosTerminal, error) {
	p := access.MustPrincipal(ctx)
	var out gen.PosTerminal
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		t, err := q.GetTerminal(ctx, gen.GetTerminalParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(PermTerminal, t.Path)) {
			return apperr.NotFound("terminal")
		}
		if err != nil {
			return err
		}
		out, err = q.UpdateTerminal(ctx, gen.UpdateTerminalParams{ID: id, CompanyID: p.CompanyID, Name: name, Status: status})
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: &t.StoreID, Action: "TERMINAL_UPDATED", Entity: "terminal", EntityID: id.String(), New: out})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
