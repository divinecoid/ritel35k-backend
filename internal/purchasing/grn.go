package purchasing

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
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/pagination"
)

type GRNLineInput struct {
	POItemID  *uuid.UUID       `json:"po_item_id"`
	VariantID uuid.UUID        `json:"variant_id"`
	Qty       decimal.Decimal  `json:"qty"`
	UnitCost  *decimal.Decimal `json:"unit_cost"` // default: PO price (with PO) or the variant's purchase price
}

type GRNInput struct {
	POID         *uuid.UUID     `json:"po_id"`
	WarehouseID  uuid.UUID      `json:"warehouse_id"` // required without a PO
	SupplierID   uuid.UUID      `json:"supplier_id"`  // required without a PO
	SupplierRef  string         `json:"supplier_ref" validate:"max=100"`
	ReceivedDate *time.Time     `json:"received_date"`
	Note         string         `json:"note" validate:"max=300"`
	Lines        []GRNLineInput `json:"lines" validate:"required,min=1,max=500,dive"`
}

type GRNLine struct {
	gen.ListGRNItemsRow
	LineTotal decimal.Decimal `json:"line_total"`
}

type GRNDetail struct {
	gen.GetGRNRow
	Lines []GRNLine `json:"lines"`
}

func (s *Service) GetGRN(ctx context.Context, id uuid.UUID) (*GRNDetail, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	r, err := q.GetGRN(ctx, gen.GetGRNParams{ID: id, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !viewable(p, r.Path, PermView, PermReceive, PermReturn)) {
		return nil, apperr.NotFound("goods receipt")
	}
	if err != nil {
		return nil, err
	}
	items, err := q.ListGRNItems(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &GRNDetail{GetGRNRow: r, Lines: make([]GRNLine, len(items))}
	for i, it := range items {
		d.Lines[i] = GRNLine{ListGRNItemsRow: it, LineTotal: it.Qty.Mul(it.UnitCost)}
	}
	return d, nil
}

func (s *Service) ListGRNs(ctx context.Context, storeID, poID *uuid.UUID, pg pagination.Params) ([]gen.ListGRNsRow, error) {
	p := access.MustPrincipal(ctx)
	all, paths := mergedScope(p, PermView, PermReceive, PermReturn).SQL()
	return s.tx.Queries().ListGRNs(ctx, gen.ListGRNsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, StoreID: storeID, PoID: poID, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
}

// Receive posts a goods receipt: stock goes up at the receiving cost (moving average is recomputed) and,
// when against a PO, received quantities are capped by what was ordered (plus the configured tolerance).
// A GRN is immutable; mistakes are corrected with a purchase return.
func (s *Service) Receive(ctx context.Context, in GRNInput) (*GRNDetail, error) {
	p := access.MustPrincipal(ctx)
	var id uuid.UUID
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		var (
			po       *gen.PurchaseOrder
			poItems  = map[uuid.UUID]gen.PurchaseOrderItem{}
			byVar    = map[uuid.UUID]gen.PurchaseOrderItem{}
			whID     = in.WarehouseID
			supplier = in.SupplierID
		)
		if in.POID != nil {
			locked, err := q.GetPOForUpdate(ctx, gen.GetPOForUpdateParams{ID: *in.POID, CompanyID: p.CompanyID})
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("purchase order")
			}
			if err != nil {
				return err
			}
			if locked.Status != "approved" && locked.Status != "partially_received" {
				return apperr.Conflict("PO_NOT_RECEIVABLE", "Only approved purchase orders can be received (status: "+locked.Status+")")
			}
			po, whID, supplier = &locked, locked.WarehouseID, locked.SupplierID
			items, err := q.ListPOItemsForUpdate(ctx, locked.ID)
			if err != nil {
				return err
			}
			for _, it := range items {
				poItems[it.ID] = it
				byVar[it.VariantID] = it
			}
		} else if in.WarehouseID == uuid.Nil || in.SupplierID == uuid.Nil {
			return apperr.Validation(map[string]string{"warehouse_id": "warehouse_id and supplier_id are required without a purchase order"})
		}
		wh, _, err := inventory.WarehouseFor(ctx, q, p, whID, PermReceive)
		if err != nil {
			return err
		}
		if _, err := q.GetSupplier(ctx, gen.GetSupplierParams{ID: supplier, CompanyID: p.CompanyID}); err != nil {
			return apperr.Validation(map[string]string{"supplier_id": "unknown supplier"})
		}
		unit, err := settingsUnit(ctx, q, p, wh.StoreID)
		if err != nil {
			return err
		}
		snap, err := settings.LoadSnapshot(ctx, q, p.CompanyID, unit)
		if err != nil {
			return err
		}
		tolerance := snap.Decimal("purchasing.over_receipt_percent")

		type line struct {
			item  *gen.PurchaseOrderItem
			input GRNLineInput
			cost  decimal.Decimal
		}
		var lines []line
		seen := map[uuid.UUID]bool{}
		total := decimal.Zero
		for _, l := range in.Lines {
			if !l.Qty.IsPositive() {
				return apperr.Validation(map[string]string{"qty": "must be positive"})
			}
			ln := line{input: l}
			if po != nil {
				var it gen.PurchaseOrderItem
				var ok bool
				if l.POItemID != nil {
					it, ok = poItems[*l.POItemID]
				} else {
					it, ok = byVar[l.VariantID]
				}
				if !ok {
					return apperr.Validation(map[string]string{"lines": "line is not part of this purchase order"})
				}
				ln.item, ln.input.VariantID, ln.cost = &it, it.VariantID, it.UnitPrice
				max := it.QtyOrdered.Mul(hundredPlus(tolerance)).Div(decimal.NewFromInt(100))
				if it.QtyReceived.Add(l.Qty).GreaterThan(max) {
					return apperr.Conflict("RECEIPT_EXCEEDS_ORDER", "Received quantity exceeds the ordered quantity").WithDetails(map[string]string{
						"variant_id": it.VariantID.String(), "ordered": it.QtyOrdered.String(), "already_received": it.QtyReceived.String()})
				}
			} else {
				v, err := q.GetVariant(ctx, gen.GetVariantParams{ID: l.VariantID, CompanyID: p.CompanyID})
				if err != nil {
					return apperr.Validation(map[string]string{"lines": "unknown variant"})
				}
				ln.cost = v.PurchasePrice
			}
			if seen[ln.input.VariantID] {
				return apperr.Validation(map[string]string{"lines": "each product may appear once per receipt"})
			}
			seen[ln.input.VariantID] = true
			if l.UnitCost != nil {
				ln.cost = *l.UnitCost
			}
			if ln.cost.IsNegative() {
				return apperr.Validation(map[string]string{"unit_cost": "must not be negative"})
			}
			total = total.Add(l.Qty.Mul(ln.cost))
			lines = append(lines, ln)
		}

		doc, err := numbering.ForWarehouse(ctx, q, wh, "goods_receipt", s.now())
		if err != nil {
			return err
		}
		id, _ = uuid.NewV7()
		date := s.now()
		if in.ReceivedDate != nil {
			date = *in.ReceivedDate
		}
		var poID *uuid.UUID
		if po != nil {
			poID = &po.ID
		}
		if _, err := q.InsertGRN(ctx, gen.InsertGRNParams{ID: id, CompanyID: p.CompanyID, StoreID: wh.StoreID, WarehouseID: wh.ID, SupplierID: supplier, PoID: poID,
			DocumentNumber: doc, SupplierRef: in.SupplierRef, ReceivedDate: dateOnly(date), Note: in.Note, TotalCost: total.Round(2), CreatedBy: p.UserID}); err != nil {
			return err
		}
		sort.Slice(lines, func(i, j int) bool { return lines[i].input.VariantID.String() < lines[j].input.VariantID.String() })
		for n, l := range lines {
			iid, _ := uuid.NewV7()
			var poItemID *uuid.UUID
			if l.item != nil {
				poItemID = &l.item.ID
			}
			if _, err := q.InsertGRNItem(ctx, gen.InsertGRNItemParams{ID: iid, GrnID: id, LineNo: int32(n + 1), PoItemID: poItemID, VariantID: l.input.VariantID, Qty: l.input.Qty, UnitCost: l.cost}); err != nil {
				return err
			}
			if _, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: wh.StoreID, WarehouseID: wh.ID, VariantID: l.input.VariantID,
				Type: inventory.Purchase, Qty: l.input.Qty, UnitCost: l.cost, RefType: "goods_receipt", RefID: &id, DocumentNo: doc, Note: in.SupplierRef, By: &p.UserID, AllowNegative: true}); err != nil {
				return err
			}
			if l.item != nil {
				if err := q.AddPOItemReceived(ctx, gen.AddPOItemReceivedParams{ID: l.item.ID, QtyReceived: l.input.Qty}); err != nil {
					return err
				}
			}
		}
		if po != nil {
			full := true
			after, err := q.ListPOItems(ctx, po.ID)
			if err != nil {
				return err
			}
			for _, it := range after {
				if it.QtyReceived.LessThan(it.QtyOrdered) {
					full = false
				}
			}
			status := "partially_received"
			if full {
				status = "received"
			}
			if err := q.SetPOStatus(ctx, gen.SetPOStatusParams{ID: po.ID, Status: status}); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: wh.StoreID, Action: "GOODS_RECEIVED", Entity: "goods_receipt", EntityID: id.String(),
			New: map[string]any{"document_number": doc, "total": total.Round(2).String(), "po_id": poID, "lines": len(lines)}}); err != nil {
			return err
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventGoodsReceived, "goods_receipt", id, map[string]any{"document_number": doc, "warehouse_id": wh.ID,
			"store_id": wh.StoreID, "supplier_id": supplier, "po_id": poID, "total": total.Round(2).String()})
	})
	if err != nil {
		return nil, err
	}
	return s.GetGRN(ctx, id)
}

func hundredPlus(pct decimal.Decimal) decimal.Decimal { return decimal.NewFromInt(100).Add(pct) }

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ---- purchase returns ---------------------------------------------------------------------

type ReturnLineInput struct {
	VariantID uuid.UUID        `json:"variant_id" validate:"required"`
	Qty       decimal.Decimal  `json:"qty"`
	UnitCost  *decimal.Decimal `json:"unit_cost"` // default: the GRN cost, else current moving average
}

type ReturnInput struct {
	GRNID       *uuid.UUID        `json:"grn_id"`
	WarehouseID uuid.UUID         `json:"warehouse_id"`
	SupplierID  uuid.UUID         `json:"supplier_id"`
	Reason      string            `json:"reason" validate:"required,max=200"`
	Lines       []ReturnLineInput `json:"lines" validate:"required,min=1,max=500,dive"`
}

type ReturnResult struct {
	ID             uuid.UUID       `json:"id"`
	DocumentNumber string          `json:"document_number"`
	TotalCost      decimal.Decimal `json:"total_cost"`
}

// Return sends goods back to a supplier: stock goes down (never below zero). When tied to a GRN the
// quantities are limited to what that receipt brought in minus earlier returns.
func (s *Service) Return(ctx context.Context, in ReturnInput) (*ReturnResult, error) {
	p := access.MustPrincipal(ctx)
	var out ReturnResult
	err := s.tx.Do(ctx, func(q *gen.Queries) error {
		whID, supplier := in.WarehouseID, in.SupplierID
		grnCost := map[uuid.UUID]decimal.Decimal{}
		grnLeft := map[uuid.UUID]decimal.Decimal{}
		if in.GRNID != nil {
			g, err := q.GetGRN(ctx, gen.GetGRNParams{ID: *in.GRNID, CompanyID: p.CompanyID})
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.NotFound("goods receipt")
			}
			if err != nil {
				return err
			}
			whID, supplier = g.WarehouseID, g.SupplierID
			items, err := q.ListGRNItems(ctx, g.ID)
			if err != nil {
				return err
			}
			for _, it := range items {
				grnCost[it.VariantID], grnLeft[it.VariantID] = it.UnitCost, it.Qty
			}
			ret, err := q.GRNReturnedQty(ctx, &g.ID)
			if err != nil {
				return err
			}
			for _, r := range ret {
				grnLeft[r.VariantID] = grnLeft[r.VariantID].Sub(r.Qty)
			}
		} else if whID == uuid.Nil || supplier == uuid.Nil {
			return apperr.Validation(map[string]string{"warehouse_id": "warehouse_id and supplier_id are required without a goods receipt"})
		}
		wh, _, err := inventory.WarehouseFor(ctx, q, p, whID, PermReturn)
		if err != nil {
			return err
		}
		lines := append([]ReturnLineInput(nil), in.Lines...)
		sort.Slice(lines, func(i, j int) bool { return lines[i].VariantID.String() < lines[j].VariantID.String() })
		doc, err := numbering.ForWarehouse(ctx, q, wh, "purchase_return", s.now())
		if err != nil {
			return err
		}
		id, _ := uuid.NewV7()
		total := decimal.Zero
		type done struct {
			l    ReturnLineInput
			cost decimal.Decimal
		}
		var applied []done
		seen := map[uuid.UUID]bool{}
		for _, l := range lines {
			if !l.Qty.IsPositive() || seen[l.VariantID] {
				return apperr.Validation(map[string]string{"lines": "qty must be positive and each product listed once"})
			}
			seen[l.VariantID] = true
			if in.GRNID != nil {
				left, ok := grnLeft[l.VariantID]
				if !ok {
					return apperr.Validation(map[string]string{"lines": "product was not part of this goods receipt"})
				}
				if l.Qty.GreaterThan(left) {
					return apperr.Conflict("RETURN_EXCEEDS_RECEIVED", "Return quantity exceeds what is left of this receipt").WithDetails(map[string]string{"variant_id": l.VariantID.String(), "returnable": left.String()})
				}
			}
			res, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: wh.StoreID, WarehouseID: wh.ID, VariantID: l.VariantID, Type: inventory.PurchaseReturn,
				Qty: l.Qty.Neg(), RefType: "purchase_return", RefID: &id, DocumentNo: doc, Note: in.Reason, By: &p.UserID, AllowNegative: false})
			if err != nil {
				return err
			}
			cost := res.AvgCost
			if c, ok := grnCost[l.VariantID]; ok {
				cost = c
			}
			if l.UnitCost != nil {
				cost = *l.UnitCost
			}
			total = total.Add(l.Qty.Mul(cost))
			applied = append(applied, done{l, cost})
		}
		if _, err := q.InsertPurchaseReturn(ctx, gen.InsertPurchaseReturnParams{ID: id, CompanyID: p.CompanyID, StoreID: wh.StoreID, WarehouseID: wh.ID, SupplierID: supplier,
			GrnID: in.GRNID, DocumentNumber: doc, Reason: in.Reason, TotalCost: total.Round(2), CreatedBy: p.UserID}); err != nil {
			return err
		}
		for n, a := range applied {
			iid, _ := uuid.NewV7()
			if _, err := q.InsertPurchaseReturnItem(ctx, gen.InsertPurchaseReturnItemParams{ID: iid, ReturnID: id, LineNo: int32(n + 1), VariantID: a.l.VariantID, Qty: a.l.Qty, UnitCost: a.cost}); err != nil {
				return err
			}
		}
		out = ReturnResult{ID: id, DocumentNumber: doc, TotalCost: total.Round(2)}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: wh.StoreID, Action: "PURCHASE_RETURNED", Entity: "purchase_return", EntityID: id.String(), New: map[string]any{"document_number": doc, "total": total.Round(2).String(), "reason": in.Reason}}); err != nil {
			return err
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventPurchaseReturn, "purchase_return", id, map[string]any{"document_number": doc, "supplier_id": supplier, "total": total.Round(2).String()})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Service) ListReturns(ctx context.Context, pg pagination.Params) ([]gen.ListPurchaseReturnsRow, error) {
	p := access.MustPrincipal(ctx)
	all, paths := mergedScope(p, PermView, PermReturn).SQL()
	return s.tx.Queries().ListPurchaseReturns(ctx, gen.ListPurchaseReturnsParams{CompanyID: p.CompanyID, AllScope: all, ScopePaths: paths, PageLimit: pg.Limit(), PageOffset: pg.Offset()})
}
