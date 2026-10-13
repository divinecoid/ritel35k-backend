// Package shift manages cashier shifts: opening float, cash in/out, closing count and variance.
package shift

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

const (
	EventShiftClosed = "ShiftClosed"
	managerPerm      = "pos.refund.approve" // manager-level: may act on other cashiers' shifts
)

type Totals struct {
	CashSales    decimal.Decimal `json:"cash_sales"`
	NoncashSales decimal.Decimal `json:"noncash_sales"`
	GrossSales   decimal.Decimal `json:"gross_sales"`
	SaleCount    int64           `json:"sale_count"`
	VoidCount    int64           `json:"void_count"`
	CashIn       decimal.Decimal `json:"cash_in"`
	CashOut      decimal.Decimal `json:"cash_out"`
	CashRefunds  decimal.Decimal `json:"cash_refunds"`
	Opening      decimal.Decimal `json:"opening_cash"`
	Expected     decimal.Decimal `json:"expected_cash"`
}

type View struct {
	gen.CashierShift
	Path      string             `json:"-"`
	Totals    Totals             `json:"totals"`
	Movements []gen.CashMovement `json:"movements,omitempty"`
}

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

func totals(ctx context.Context, q *gen.Queries, sh gen.CashierShift) (Totals, error) {
	r, err := q.ShiftTotals(ctx, sh.ID)
	if err != nil {
		return Totals{}, err
	}
	t := Totals{CashSales: r.CashSales, NoncashSales: r.NoncashSales, GrossSales: r.GrossSales, SaleCount: r.SaleCount, VoidCount: r.VoidCount,
		CashIn: r.CashIn, CashOut: r.CashOut, CashRefunds: r.CashRefunds, Opening: sh.OpeningCash}
	t.Expected = t.Opening.Add(t.CashSales).Add(t.CashIn).Sub(t.CashOut).Sub(t.CashRefunds)
	return t, nil
}

// RequireOpen returns the caller's open shift or a SHIFT_REQUIRED error (used by the POS).
func RequireOpen(ctx context.Context, q *gen.Queries, cashierID uuid.UUID) (gen.CashierShift, error) {
	sh, err := q.GetOpenShiftByCashier(ctx, cashierID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sh, apperr.Conflict("SHIFT_REQUIRED", "An open shift is required (cash sales and cash refunds go through your drawer)")
	}
	return sh, err
}

func (s *Service) Open(ctx context.Context, terminalID uuid.UUID, openingCash decimal.Decimal) (*View, error) {
	p := access.MustPrincipal(ctx)
	if openingCash.IsNegative() {
		return nil, apperr.Validation(map[string]string{"opening_cash": "must not be negative"})
	}
	var out View
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		t, err := q.GetTerminal(ctx, gen.GetTerminalParams{ID: terminalID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("terminal")
		}
		if err != nil {
			return err
		}
		if !p.Grants.Can("pos.shift.manage", t.Path) {
			return apperr.Forbidden("not allowed to open a shift in this store")
		}
		if t.Status != "active" {
			return apperr.Conflict("TERMINAL_INACTIVE", "Terminal is inactive")
		}
		if _, err := q.GetOpenShiftByCashier(ctx, p.UserID); err == nil {
			return apperr.Conflict("SHIFT_ALREADY_OPEN", "You already have an open shift")
		}
		id, _ := uuid.NewV7()
		sh, err := q.CreateShift(ctx, gen.CreateShiftParams{ID: id, CompanyID: p.CompanyID, StoreID: t.StoreID, TerminalID: t.ID, CashierID: p.UserID, OpeningCash: openingCash})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			if pe.ConstraintName == "cashier_shifts_one_open_terminal" {
				return apperr.Conflict("TERMINAL_IN_USE", "This terminal already has an open shift")
			}
			return apperr.Conflict("SHIFT_ALREADY_OPEN", "You already have an open shift")
		}
		if err != nil {
			return err
		}
		out = View{CashierShift: sh, Totals: Totals{Opening: sh.OpeningCash, Expected: sh.OpeningCash}}
		return audit.Record(ctx, q, audit.Entry{StoreID: &t.StoreID, Action: "SHIFT_OPENED", Entity: "shift", EntityID: id.String(), New: map[string]any{"opening_cash": openingCash.String(), "terminal": t.Code}})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Current returns the caller's open shift with live totals, or nil.
func (s *Service) Current(ctx context.Context) (*View, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	sh, err := q.GetOpenShiftByCashier(ctx, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t, err := totals(ctx, q, sh)
	if err != nil {
		return nil, err
	}
	return &View{CashierShift: sh, Totals: t}, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*View, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	r, err := q.GetShift(ctx, gen.GetShiftParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("shift")
	}
	if err != nil {
		return nil, err
	}
	if !p.Grants.Can("pos.sale.view", r.Path) && r.CashierID != p.UserID {
		return nil, apperr.NotFound("shift")
	}
	sh := shiftOf(r)
	t, err := totals(ctx, q, sh)
	if err != nil {
		return nil, err
	}
	mv, err := q.ListCashMovements(ctx, id)
	if err != nil {
		return nil, err
	}
	return &View{CashierShift: sh, Totals: t, Movements: mv}, nil
}

func shiftOf(r gen.GetShiftRow) gen.CashierShift {
	return gen.CashierShift{ID: r.ID, CompanyID: r.CompanyID, StoreID: r.StoreID, TerminalID: r.TerminalID, CashierID: r.CashierID, Status: r.Status,
		OpeningCash: r.OpeningCash, OpenedAt: r.OpenedAt, ClosedAt: r.ClosedAt, ClosedBy: r.ClosedBy, ExpectedCash: r.ExpectedCash,
		CountedCash: r.CountedCash, Variance: r.Variance, Note: r.Note}
}

// actable loads a shift the caller may act on: their own, or any in scope for managers.
func actable(ctx context.Context, q *gen.Queries, p *access.Principal, id uuid.UUID) (gen.GetShiftRow, error) {
	r, err := q.GetShift(ctx, gen.GetShiftParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, apperr.NotFound("shift")
	}
	if err != nil {
		return r, err
	}
	if r.CashierID != p.UserID && !p.Grants.Can(managerPerm, r.Path) {
		return r, apperr.NotFound("shift")
	}
	return r, nil
}

func (s *Service) CashMovement(ctx context.Context, shiftID uuid.UUID, typ string, amount decimal.Decimal, reason string) (*gen.CashMovement, error) {
	p := access.MustPrincipal(ctx)
	if !amount.IsPositive() {
		return nil, apperr.Validation(map[string]string{"amount": "must be positive"})
	}
	if typ != "CASH_IN" && typ != "CASH_OUT" {
		return nil, apperr.Validation(map[string]string{"type": "CASH_IN or CASH_OUT"})
	}
	var out gen.CashMovement
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		r, err := actable(ctx, q, p, shiftID)
		if err != nil {
			return err
		}
		sh, err := q.GetShiftForUpdate(ctx, gen.GetShiftForUpdateParams{ID: shiftID, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if sh.Status != "open" {
			return apperr.Conflict("SHIFT_CLOSED", "Shift is closed")
		}
		id, _ := uuid.NewV7()
		out, err = q.InsertCashMovement(ctx, gen.InsertCashMovementParams{ID: id, CompanyID: p.CompanyID, StoreID: r.StoreID, ShiftID: shiftID, Type: typ,
			Amount: amount, Reason: reason, CreatedBy: p.UserID})
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: &r.StoreID, Action: "CASH_" + map[string]string{"CASH_IN": "IN", "CASH_OUT": "OUT"}[typ], Entity: "shift", EntityID: shiftID.String(),
			New: map[string]any{"amount": amount.String(), "reason": reason}})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) Close(ctx context.Context, shiftID uuid.UUID, counted decimal.Decimal, note string) (*View, error) {
	p := access.MustPrincipal(ctx)
	if counted.IsNegative() {
		return nil, apperr.Validation(map[string]string{"counted_cash": "must not be negative"})
	}
	var out View
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		if _, err := actable(ctx, q, p, shiftID); err != nil {
			return err
		}
		locked, err := q.GetShiftForUpdate(ctx, gen.GetShiftForUpdateParams{ID: shiftID, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if locked.Status != "open" {
			return apperr.Conflict("SHIFT_CLOSED", "Shift is already closed")
		}
		v, err := CloseCore(ctx, q, locked, p.UserID, counted, note, time.Now())
		if err != nil {
			return err
		}
		out = *v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) List(ctx context.Context, storeID *uuid.UUID, status string, pg pagination.Params) ([]gen.ListShiftsRow, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope("pos.sale.view").SQL()
	return s.tx.Queries().ListShifts(ctx, gen.ListShiftsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: storeID, Status: status,
		PageLimit: pg.Limit(), PageOffset: pg.Offset()})
}

// CloseCore closes a locked, open shift: expected cash from the sales on record, variance, audit and the ShiftClosed
// event. Used by the cashier's own close and by the offline sync (closedAt = when it really happened).
func CloseCore(ctx context.Context, q *gen.Queries, locked gen.CashierShift, by uuid.UUID, counted decimal.Decimal, note string, closedAt time.Time) (*View, error) {
	t, err := totals(ctx, q, locked)
	if err != nil {
		return nil, err
	}
	variance := counted.Sub(t.Expected)
	exp, cnt, vr := t.Expected, counted, variance
	closed, err := q.CloseShiftAt(ctx, gen.CloseShiftAtParams{ID: locked.ID, ClosedBy: &by, ExpectedCash: &exp, CountedCash: &cnt, Variance: &vr, Note: note, ClosedAt: &closedAt})
	if err != nil {
		return nil, err
	}
	out := View{CashierShift: closed, Totals: t}
	if !variance.IsZero() {
		if err := audit.Record(ctx, q, audit.Entry{CompanyID: locked.CompanyID, UserID: &by, StoreID: &locked.StoreID, Action: "SHIFT_VARIANCE", Entity: "shift", EntityID: locked.ID.String(),
			New: map[string]any{"expected": t.Expected.String(), "counted": counted.String(), "variance": variance.String()}}); err != nil {
			return nil, err
		}
	}
	if err := audit.Record(ctx, q, audit.Entry{CompanyID: locked.CompanyID, UserID: &by, StoreID: &locked.StoreID, Action: "SHIFT_CLOSED", Entity: "shift", EntityID: locked.ID.String(), New: out.Totals}); err != nil {
		return nil, err
	}
	if err := outbox.Publish(ctx, q, locked.CompanyID, EventShiftClosed, "shift", locked.ID, map[string]any{
		"store_id": locked.StoreID, "cashier_id": locked.CashierID, "expected": t.Expected.String(), "counted": counted.String(), "variance": variance.String(),
		"gross_sales": t.GrossSales.String(), "closed_at": closedAt}); err != nil {
		return nil, err
	}
	return &out, nil
}
