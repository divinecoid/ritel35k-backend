// Package transfer moves stock between warehouses (store ↔ store, central ↔ store) with an
// in-transit stage: draft → submitted → approved → shipped → received.
package transfer

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
	"github.com/ritel35k/backend/internal/inventory"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/platform/tx"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

const (
	PermTransfer = "inventory.transfer"
	PermApprove  = "inventory.transfer.approve"
	PermView     = "inventory.view"

	EventShipped  = "StockTransferShipped"
	EventReceived = "StockTransferReceived"
)

type Service struct {
	tx  *tx.Manager
	now func() time.Time
}

func NewService(t *tx.Manager) *Service { return &Service{tx: t, now: time.Now} }

type LineInput struct {
	VariantID uuid.UUID       `json:"variant_id" validate:"required"`
	Qty       decimal.Decimal `json:"qty"`
}

type Input struct {
	FromWarehouseID uuid.UUID   `json:"from_warehouse_id" validate:"required"`
	ToWarehouseID   uuid.UUID   `json:"to_warehouse_id" validate:"required"`
	Note            string      `json:"note" validate:"max=300"`
	Lines           []LineInput `json:"lines" validate:"required,min=1,max=500,dive"`
}

type Detail struct {
	gen.GetTransferRow
	Lines []gen.ListTransferItemsRow `json:"lines"`
}

func canSee(p *access.Principal, fromPath, toPath string) bool {
	return inventory.CanAt(p, PermTransfer, fromPath) || inventory.CanAt(p, PermTransfer, toPath) ||
		inventory.CanAt(p, PermView, fromPath) || inventory.CanAt(p, PermView, toPath) || inventory.CanAt(p, PermApprove, fromPath)
}

func (s *Service) load(ctx context.Context, q *gen.Queries, p *access.Principal, id uuid.UUID) (*Detail, error) {
	r, err := q.GetTransfer(ctx, gen.GetTransferParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !canSee(p, r.FromPath, r.ToPath)) {
		return nil, apperr.NotFound("stock transfer")
	}
	if err != nil {
		return nil, err
	}
	items, err := q.ListTransferItems(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Detail{GetTransferRow: r, Lines: items}, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Detail, error) {
	return s.load(ctx, s.tx.Queries(), access.MustPrincipal(ctx), id)
}

func (s *Service) List(ctx context.Context, status string, pg pagination.Params) ([]gen.ListTransfersRow, error) {
	p := access.MustPrincipal(ctx)
	sc := access.ScopeFilter{}
	for _, perm := range []string{PermTransfer, PermView, PermApprove} {
		g := p.Grants.Scope(perm)
		sc.All = sc.All || g.All
		sc.Paths = append(sc.Paths, g.Paths...)
	}
	all, paths := sc.SQL()
	return s.tx.Queries().ListTransfers(ctx, gen.ListTransfersParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, Status: status, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
}

func validateLines(in []LineInput) error {
	seen := map[uuid.UUID]bool{}
	for _, l := range in {
		if !l.Qty.IsPositive() || seen[l.VariantID] {
			return apperr.Validation(map[string]string{"lines": "qty must be positive and each product listed once"})
		}
		seen[l.VariantID] = true
	}
	return nil
}

// Create makes a draft. You need inventory.transfer at the ORIGIN; the destination may be any warehouse.
func (s *Service) Create(ctx context.Context, in Input) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	if in.FromWarehouseID == in.ToWarehouseID {
		return nil, apperr.Validation(map[string]string{"to_warehouse_id": "must differ from the origin"})
	}
	if err := validateLines(in.Lines); err != nil {
		return nil, err
	}
	var id uuid.UUID
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		from, _, err := inventory.WarehouseFor(ctx, q, p, in.FromWarehouseID, PermTransfer)
		if err != nil {
			return err
		}
		to, err := q.GetWarehouse(ctx, gen.GetWarehouseParams{ID: in.ToWarehouseID, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && to.Status != "active") {
			return apperr.Validation(map[string]string{"to_warehouse_id": "unknown or inactive warehouse"})
		}
		if err != nil {
			return err
		}
		for _, l := range in.Lines {
			if _, err := q.GetVariant(ctx, gen.GetVariantParams{ID: l.VariantID, CompanyID: p.CompanyID}); err != nil {
				return apperr.Validation(map[string]string{"lines": "unknown variant"})
			}
		}
		doc, err := numbering.ForWarehouse(ctx, q, from, "stock_transfer", s.now())
		if err != nil {
			return err
		}
		id, _ = uuid.NewV7()
		if _, err := q.InsertTransfer(ctx, gen.InsertTransferParams{ID: id, CompanyID: p.CompanyID, FromWarehouseID: from.ID, ToWarehouseID: to.ID, FromStoreID: from.StoreID,
			ToStoreID: to.StoreID, DocumentNumber: doc, Note: in.Note, CreatedBy: p.UserID}); err != nil {
			return err
		}
		for i, l := range in.Lines {
			iid, _ := uuid.NewV7()
			if _, err := q.InsertTransferItem(ctx, gen.InsertTransferItemParams{ID: iid, TransferID: id, LineNo: int32(i + 1), VariantID: l.VariantID, QtyRequested: l.Qty}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: from.StoreID, Action: "TRANSFER_CREATED", Entity: "stock_transfer", EntityID: id.String(), New: map[string]any{"document_number": doc, "to": to.Code}})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) step(ctx context.Context, id uuid.UUID, fn func(q *gen.Queries, p *access.Principal, t gen.StockTransfer, d *Detail) error) (*Detail, error) {
	p := access.MustPrincipal(ctx)
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		t, err := q.GetTransferForUpdate(ctx, gen.GetTransferForUpdateParams{ID: id, CompanyID: p.CompanyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.NotFound("stock transfer")
		}
		if err != nil {
			return err
		}
		d, err := s.load(ctx, q, p, id)
		if err != nil {
			return err
		}
		return fn(q, p, t, d)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Submit(ctx context.Context, id uuid.UUID) (*Detail, error) {
	return s.step(ctx, id, func(q *gen.Queries, p *access.Principal, t gen.StockTransfer, d *Detail) error {
		if !inventory.CanAt(p, PermTransfer, d.FromPath) {
			return apperr.Forbidden("not allowed")
		}
		if t.Status != "draft" {
			return apperr.Conflict("TRANSFER_BAD_STATE", "Only drafts can be submitted")
		}
		now := s.now()
		return q.SetTransferStatus(ctx, gen.SetTransferStatusParams{ID: id, Status: "submitted", SubmittedAt: &now})
	})
}

// Approve is done on the origin side (the party giving up the stock).
func (s *Service) Approve(ctx context.Context, id uuid.UUID) (*Detail, error) {
	return s.step(ctx, id, func(q *gen.Queries, p *access.Principal, t gen.StockTransfer, d *Detail) error {
		if !inventory.CanAt(p, PermApprove, d.FromPath) {
			return apperr.Forbidden("not allowed to approve transfers from this location")
		}
		if t.Status != "submitted" {
			return apperr.Conflict("TRANSFER_BAD_STATE", "Only submitted transfers can be approved")
		}
		now := s.now()
		if err := q.SetTransferStatus(ctx, gen.SetTransferStatusParams{ID: id, Status: "approved", ApprovedBy: &p.UserID, ApprovedAt: &now}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: t.FromStoreID, Action: "TRANSFER_APPROVED", Entity: "stock_transfer", EntityID: id.String()})
	})
}

type ShipLine struct {
	VariantID uuid.UUID       `json:"variant_id" validate:"required"`
	Qty       decimal.Decimal `json:"qty"`
}

// Ship takes the goods out of the origin (on_hand down, at the origin's moving-average cost) and puts
// them "in transit" at the destination. Quantities default to what was requested; you can ship less.
func (s *Service) Ship(ctx context.Context, id uuid.UUID, lines []ShipLine) (*Detail, error) {
	return s.step(ctx, id, func(q *gen.Queries, p *access.Principal, t gen.StockTransfer, d *Detail) error {
		if !inventory.CanAt(p, PermTransfer, d.FromPath) {
			return apperr.Forbidden("not allowed to ship from this location")
		}
		if t.Status != "approved" {
			return apperr.Conflict("TRANSFER_BAD_STATE", "Only approved transfers can be shipped")
		}
		override := map[uuid.UUID]decimal.Decimal{}
		for _, l := range lines {
			override[l.VariantID] = l.Qty
		}
		items := append([]gen.ListTransferItemsRow(nil), d.Lines...)
		sort.Slice(items, func(i, j int) bool { return items[i].VariantID.String() < items[j].VariantID.String() })
		shippedAny := false
		for _, it := range items {
			qty := it.QtyRequested
			if v, ok := override[it.VariantID]; ok {
				qty = v
			}
			if qty.IsNegative() || qty.GreaterThan(it.QtyRequested) {
				return apperr.Validation(map[string]string{"lines": "shipped quantity must be between 0 and the requested quantity"})
			}
			if qty.IsZero() {
				continue
			}
			res, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: t.FromStoreID, WarehouseID: t.FromWarehouseID, VariantID: it.VariantID,
				Type: inventory.TransferOut, Qty: qty.Neg(), RefType: "stock_transfer", RefID: &id, DocumentNo: t.DocumentNumber, By: &p.UserID, AllowNegative: false})
			if err != nil {
				return err
			}
			if err := q.ShipTransferItem(ctx, gen.ShipTransferItemParams{ID: it.ID, QtyShipped: qty, UnitCost: res.AvgCost}); err != nil {
				return err
			}
			if err := inventory.InTransit(ctx, q, p.CompanyID, t.ToStoreID, t.ToWarehouseID, it.VariantID, qty); err != nil {
				return err
			}
			shippedAny = true
		}
		if !shippedAny {
			return apperr.Validation(map[string]string{"lines": "nothing to ship"})
		}
		now := s.now()
		if err := q.SetTransferStatus(ctx, gen.SetTransferStatusParams{ID: id, Status: "shipped", ShippedBy: &p.UserID, ShippedAt: &now}); err != nil {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: t.FromStoreID, Action: "TRANSFER_SHIPPED", Entity: "stock_transfer", EntityID: id.String(), New: t.DocumentNumber}); err != nil {
			return err
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventShipped, "stock_transfer", id, map[string]any{"document_number": t.DocumentNumber, "from": t.FromWarehouseID, "to": t.ToWarehouseID})
	})
}

type ReceiveLine struct {
	VariantID uuid.UUID       `json:"variant_id" validate:"required"`
	Qty       decimal.Decimal `json:"qty"`
}

// Receive books the goods into the destination at the origin's cost. Receiving less than shipped is
// allowed: the shortage is recorded on the document and raised as an event/audit entry (potential loss).
func (s *Service) Receive(ctx context.Context, id uuid.UUID, lines []ReceiveLine) (*Detail, error) {
	return s.step(ctx, id, func(q *gen.Queries, p *access.Principal, t gen.StockTransfer, d *Detail) error {
		if !inventory.CanAt(p, PermTransfer, d.ToPath) {
			return apperr.Forbidden("not allowed to receive at this location")
		}
		if t.Status != "shipped" {
			return apperr.Conflict("TRANSFER_BAD_STATE", "Only shipped transfers can be received")
		}
		override := map[uuid.UUID]decimal.Decimal{}
		for _, l := range lines {
			override[l.VariantID] = l.Qty
		}
		items := append([]gen.ListTransferItemsRow(nil), d.Lines...)
		sort.Slice(items, func(i, j int) bool { return items[i].VariantID.String() < items[j].VariantID.String() })
		short := decimal.Zero
		for _, it := range items {
			if it.QtyShipped.IsZero() {
				continue
			}
			qty := it.QtyShipped
			if v, ok := override[it.VariantID]; ok {
				qty = v
			}
			if qty.IsNegative() || qty.GreaterThan(it.QtyShipped) {
				return apperr.Validation(map[string]string{"lines": "received quantity must be between 0 and the shipped quantity"})
			}
			if err := inventory.InTransit(ctx, q, p.CompanyID, t.ToStoreID, t.ToWarehouseID, it.VariantID, it.QtyShipped.Neg()); err != nil {
				return err
			}
			if qty.IsPositive() {
				if _, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: t.ToStoreID, WarehouseID: t.ToWarehouseID, VariantID: it.VariantID,
					Type: inventory.TransferIn, Qty: qty, UnitCost: it.UnitCost, RefType: "stock_transfer", RefID: &id, DocumentNo: t.DocumentNumber, By: &p.UserID, AllowNegative: true}); err != nil {
					return err
				}
			}
			if err := q.ReceiveTransferItem(ctx, gen.ReceiveTransferItemParams{ID: it.ID, QtyReceived: qty}); err != nil {
				return err
			}
			short = short.Add(it.QtyShipped.Sub(qty).Mul(it.UnitCost))
		}
		now := s.now()
		if err := q.SetTransferStatus(ctx, gen.SetTransferStatusParams{ID: id, Status: "received", ReceivedBy: &p.UserID, ReceivedAt: &now}); err != nil {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: t.ToStoreID, Action: "TRANSFER_RECEIVED", Entity: "stock_transfer", EntityID: id.String(), New: t.DocumentNumber}); err != nil {
			return err
		}
		if short.IsPositive() {
			if err := audit.Record(ctx, q, audit.Entry{StoreID: t.FromStoreID, Action: "TRANSFER_SHORTAGE", Entity: "stock_transfer", EntityID: id.String(),
				New: map[string]any{"document_number": t.DocumentNumber, "value": short.Round(2).String()}}); err != nil {
				return err
			}
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventReceived, "stock_transfer", id, map[string]any{"document_number": t.DocumentNumber, "shortage_value": short.Round(2).String()})
	})
}

// Cancel is possible until goods have shipped.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID, reason string) (*Detail, error) {
	return s.step(ctx, id, func(q *gen.Queries, p *access.Principal, t gen.StockTransfer, d *Detail) error {
		if !inventory.CanAt(p, PermTransfer, d.FromPath) && !inventory.CanAt(p, PermApprove, d.FromPath) {
			return apperr.Forbidden("not allowed")
		}
		if t.Status == "shipped" || t.Status == "received" || t.Status == "cancelled" {
			return apperr.Conflict("TRANSFER_BAD_STATE", "This transfer can no longer be cancelled")
		}
		if err := q.SetTransferStatus(ctx, gen.SetTransferStatusParams{ID: id, Status: "cancelled", CancelReason: &reason}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{StoreID: t.FromStoreID, Action: "TRANSFER_CANCELLED", Entity: "stock_transfer", EntityID: id.String(), New: reason})
	})
}
