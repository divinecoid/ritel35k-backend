// Package inventory owns stock: an append-only movement ledger plus a cached balance per
// (warehouse, variant), updated atomically in the caller's transaction.
package inventory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/pkg/apperr"
)

const (
	Opening    = "OPENING"
	Purchase   = "PURCHASE"
	Sale       = "SALE"
	SaleVoid   = "SALE_VOID"
	SaleReturn = "SALE_RETURN"
	Adjustment = "ADJUSTMENT"
	Damage     = "DAMAGE"
)

type Movement struct {
	CompanyID     uuid.UUID
	StoreID       *uuid.UUID
	WarehouseID   uuid.UUID
	VariantID     uuid.UUID
	Type          string
	Qty           decimal.Decimal // signed: + in, - out
	UnitCost      decimal.Decimal // cost of inbound stock; ignored for outbound (the moving average is used)
	RefType       string
	RefID         *uuid.UUID
	DocumentNo    string
	Note          string
	By            *uuid.UUID
	AllowNegative bool
	At            *time.Time // when it physically happened (offline sales synced later); nil = now
}

type Result struct {
	OnHand  decimal.Decimal
	AvgCost decimal.Decimal // moving average after the movement (== COGS unit cost for outbound)
}

// Apply records one movement and updates the balance in the same transaction (q must be tx-bound).
// Concurrent callers on the same item serialize on the balance row lock; the check
// "on_hand + delta >= 0" is part of the UPDATE, so overselling is impossible without allow_negative.
func Apply(ctx context.Context, q *gen.Queries, m Movement) (Result, error) {
	if m.Qty.IsZero() {
		return Result{}, apperr.Validation(map[string]string{"qty": "must not be zero"})
	}
	cost := m.UnitCost
	if m.Qty.IsNegative() || cost.IsNegative() {
		cost = decimal.Zero
	}
	bal, err := q.ApplyBalanceDelta(ctx, gen.ApplyBalanceDeltaParams{
		Delta: m.Qty, UnitCost: cost, WarehouseID: m.WarehouseID, VariantID: m.VariantID, AllowNegative: m.AllowNegative,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if m.Qty.IsNegative() && !m.AllowNegative {
			return Result{}, insufficient(ctx, q, m)
		}
		bal, err = q.InsertBalance(ctx, gen.InsertBalanceParams{
			CompanyID: m.CompanyID, StoreID: m.StoreID, WarehouseID: m.WarehouseID, VariantID: m.VariantID, Delta: m.Qty, UnitCost: cost,
		})
		if errors.Is(err, pgx.ErrNoRows) { // lost a creation race: the row exists now
			bal, err = q.ApplyBalanceDelta(ctx, gen.ApplyBalanceDeltaParams{
				Delta: m.Qty, UnitCost: cost, WarehouseID: m.WarehouseID, VariantID: m.VariantID, AllowNegative: m.AllowNegative,
			})
		}
	}
	if err != nil {
		return Result{}, err
	}
	unitCost := bal.AvgCost
	if m.Qty.IsPositive() && m.UnitCost.IsPositive() {
		unitCost = m.UnitCost
	}
	id, _ := uuid.NewV7()
	if err := q.InsertMovement(ctx, gen.InsertMovementParams{
		ID: id, CompanyID: m.CompanyID, StoreID: m.StoreID, WarehouseID: m.WarehouseID, VariantID: m.VariantID, MovementType: m.Type,
		QtyDelta: m.Qty, UnitCost: unitCost, BalanceAfter: bal.OnHand, RefType: m.RefType, RefID: m.RefID, DocumentNumber: m.DocumentNo,
		Note: m.Note, CreatedBy: m.By, At: m.At,
	}); err != nil {
		return Result{}, err
	}
	return Result{OnHand: bal.OnHand, AvgCost: bal.AvgCost}, nil
}

func insufficient(ctx context.Context, q *gen.Queries, m Movement) error {
	have := decimal.Zero
	// Best-effort figure for the error message; the failed UPDATE changed nothing.
	rows, err := q.ListBalances(ctx, gen.ListBalancesParams{
		CompanyID: m.CompanyID, AllScope: true, ScopePaths: []string{}, WarehouseID: &m.WarehouseID, VariantID: &m.VariantID, PageLimit: 1,
	})
	if err == nil && len(rows) > 0 {
		have = rows[0].OnHand
	}
	return apperr.Conflict("INSUFFICIENT_STOCK", "Stock is not sufficient").WithDetails(map[string]any{
		"variant_id": m.VariantID, "available": have.String(), "requested": m.Qty.Neg().String(),
	})
}
