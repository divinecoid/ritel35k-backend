package inventory

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type Service struct{ tx *tx.Manager }

func NewService(t *tx.Manager) *Service { return &Service{tx: t} }

type BalanceFilter struct {
	StoreID     *uuid.UUID
	WarehouseID *uuid.UUID
	VariantID   *uuid.UUID
	UnderPath   string
	Q           string
	LowOnly     bool
}

func (s *Service) Balances(ctx context.Context, f BalanceFilter, pg pagination.Params) ([]gen.ListBalancesRow, pagination.Meta, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope(access.InventoryView).SQL()
	q := s.tx.Queries()
	rows, err := q.ListBalances(ctx, gen.ListBalancesParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID,
		WarehouseID: f.WarehouseID, VariantID: f.VariantID, UnderPath: f.UnderPath, Q: f.Q, LowOnly: f.LowOnly, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
	if err != nil {
		return nil, pagination.Meta{}, err
	}
	total, err := q.CountBalances(ctx, gen.CountBalancesParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID,
		WarehouseID: f.WarehouseID, VariantID: f.VariantID, UnderPath: f.UnderPath, Q: f.Q, LowOnly: f.LowOnly})
	return rows, pagination.NewMeta(pg, total), err
}

type MovementFilter struct {
	StoreID   *uuid.UUID
	VariantID *uuid.UUID
	Type      string
	From, To  *time.Time
}

func (s *Service) Movements(ctx context.Context, f MovementFilter, pg pagination.Params) ([]gen.ListMovementsRow, error) {
	p := access.MustPrincipal(ctx)
	all, paths := p.Grants.Scope(access.InventoryView).SQL()
	return s.tx.Queries().ListMovements(ctx, gen.ListMovementsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: f.StoreID,
		VariantID: f.VariantID, MovementType: f.Type, FromTime: f.From, ToTime: f.To, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
}

type AdjustLine struct {
	VariantID uuid.UUID
	QtyDelta  decimal.Decimal // signed
	UnitCost  decimal.Decimal // for inbound lines
}

type AdjustInput struct {
	StoreID     uuid.UUID
	WarehouseID *uuid.UUID // default: the store's default warehouse
	Type        string     // OPENING | ADJUSTMENT | DAMAGE
	Reason      string
	Lines       []AdjustLine
}

type AdjustResult struct {
	DocumentNumber string             `json:"document_number"`
	Lines          []AdjustLineResult `json:"lines"`
}

type AdjustLineResult struct {
	VariantID uuid.UUID       `json:"variant_id"`
	OnHand    decimal.Decimal `json:"on_hand"`
	AvgCost   decimal.Decimal `json:"avg_cost"`
}

// Adjust changes stock by hand (opening balances, corrections, damage). One document, one transaction.
func (s *Service) Adjust(ctx context.Context, in AdjustInput) (*AdjustResult, error) {
	p := access.MustPrincipal(ctx)
	if len(in.Lines) == 0 {
		return nil, apperr.Validation(map[string]string{"lines": "at least one line"})
	}
	seen := map[uuid.UUID]bool{}
	for _, l := range in.Lines {
		if seen[l.VariantID] {
			return nil, apperr.Validation(map[string]string{"lines": "duplicate variant in one document"})
		}
		seen[l.VariantID] = true
		if l.QtyDelta.IsZero() {
			return nil, apperr.Validation(map[string]string{"qty_delta": "must not be zero"})
		}
		if in.Type == Damage && l.QtyDelta.IsPositive() {
			return nil, apperr.Validation(map[string]string{"qty_delta": "damage must be negative"})
		}
	}
	lines := append([]AdjustLine(nil), in.Lines...)
	sort.Slice(lines, func(i, j int) bool { return lines[i].VariantID.String() < lines[j].VariantID.String() }) // stable lock order

	var out AdjustResult
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		st, err := q.GetStore(ctx, gen.GetStoreParams{ID: in.StoreID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("store")
		}
		if err != nil {
			return err
		}
		if !p.Grants.Can(access.InventoryAdjust, st.Path) {
			return apperr.Forbidden("not allowed to adjust stock in this store")
		}
		var wh gen.Warehouse
		if in.WarehouseID != nil {
			wh, err = q.GetWarehouse(ctx, gen.GetWarehouseParams{ID: *in.WarehouseID, CompanyID: p.CompanyID})
		} else {
			wh, err = q.GetDefaultWarehouse(ctx, &in.StoreID)
		}
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (wh.StoreID == nil || *wh.StoreID != in.StoreID)) {
			return apperr.Validation(map[string]string{"warehouse_id": "unknown warehouse for this store"})
		}
		if err != nil {
			return err
		}
		doc, err := numbering.Next(ctx, q, p.CompanyID, in.StoreID, "stock_adjustment", time.Now())
		if err != nil {
			return err
		}
		out.DocumentNumber = doc
		docID, _ := uuid.NewV7()
		for _, l := range lines {
			if _, err := q.GetVariant(ctx, gen.GetVariantParams{ID: l.VariantID, CompanyID: p.CompanyID}); err != nil {
				return apperr.Validation(map[string]string{"variant_id": "unknown variant"})
			}
			res, err := Apply(ctx, q, Movement{CompanyID: p.CompanyID, StoreID: &in.StoreID, WarehouseID: wh.ID, VariantID: l.VariantID, Type: in.Type,
				Qty: l.QtyDelta, UnitCost: l.UnitCost, RefType: "stock_adjustment", RefID: &docID, DocumentNo: doc, Note: in.Reason, By: &p.UserID, AllowNegative: false})
			if err != nil {
				return err
			}
			out.Lines = append(out.Lines, AdjustLineResult{VariantID: l.VariantID, OnHand: res.OnHand, AvgCost: res.AvgCost})
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: &in.StoreID, Action: "STOCK_ADJUSTED", Entity: "stock_adjustment", EntityID: doc,
			New: map[string]any{"type": in.Type, "reason": in.Reason, "lines": in.Lines}})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

var _ = settings.Registry
