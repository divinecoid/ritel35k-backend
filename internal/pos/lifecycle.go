package pos

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	customerpkg "github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/inventory"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/settings"
	"github.com/ritel35k/backend/internal/shift"
	"github.com/ritel35k/backend/pkg/apperr"
)

// Void cancels a sale made in a shift that is still open: stock goes back at the cost it left at,
// and the sale drops out of the shift's cash totals. Later corrections use returns.
// Needs pos.transaction.void, or a supervisor PIN holding it.
func (s *Service) Void(ctx context.Context, saleID uuid.UUID, reason string, approval *ApprovalInput) (*SaleDetail, error) {
	p := access.MustPrincipal(ctx)
	rq := s.tx.Queries()
	pre, err := rq.GetSale(ctx, gen.GetSaleParams{ID: saleID, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(PermCreate, pre.Path)) {
		return nil, apperr.NotFound("sale")
	}
	if err != nil {
		return nil, err
	}
	approvedBy, err := s.requireApproval(ctx, p, pre.StoreID, pre.Path, PermVoid, approval)
	if err != nil {
		return nil, err
	}

	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		sale, err := q.GetSaleForUpdate(ctx, gen.GetSaleForUpdateParams{ID: saleID, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if sale.Status != "completed" {
			return apperr.Conflict("ALREADY_VOIDED", "Sale is already voided")
		}
		sh, err := q.GetShiftForUpdate(ctx, gen.GetShiftForUpdateParams{ID: sale.ShiftID, CompanyID: p.CompanyID})
		if err != nil {
			return err
		}
		if sh.Status != "open" {
			return apperr.Conflict("VOID_NOT_ALLOWED", "The shift of this sale is closed; use a return instead")
		}
		rets, err := q.ListReturnsBySale(ctx, saleID)
		if err != nil {
			return err
		}
		if len(rets) > 0 {
			return apperr.Conflict("VOID_NOT_ALLOWED", "Sale has returns; it cannot be voided")
		}
		if _, err := q.VoidSale(ctx, gen.VoidSaleParams{ID: saleID, VoidReason: reason, VoidedBy: &p.UserID}); err != nil {
			return err
		}
		items, err := q.ListSaleItems(ctx, saleID)
		if err != nil {
			return err
		}
		sort.Slice(items, func(i, j int) bool { return items[i].VariantID.String() < items[j].VariantID.String() })
		for _, it := range items {
			if _, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: &sale.StoreID, WarehouseID: it.WarehouseID, VariantID: it.VariantID,
				Type: inventory.SaleVoid, Qty: it.Qty, UnitCost: it.UnitCost, RefType: "sale", RefID: &saleID, DocumentNo: sale.DocumentNumber, Note: reason,
				By: &p.UserID, AllowNegative: true}); err != nil {
				return err
			}
		}
		if _, err := q.RestoreTagsOfSale(ctx, gen.RestoreTagsOfSaleParams{CompanyID: p.CompanyID, SaleID: &saleID}); err != nil { // RFID: voided items are back on the shelf
			return err
		}
		if sale.CustomerID != nil {
			if err := customerpkg.ReverseSale(ctx, q, p.CompanyID, *sale.CustomerID, saleID, &p.UserID); err != nil {
				return err
			}
			if err := q.ReduceCustomerSpend(ctx, gen.ReduceCustomerSpendParams{ID: *sale.CustomerID, LifetimeSpend: sale.GrandTotal}); err != nil {
				return err
			}
		}
		if rd, err := q.ActiveRedemptionForSale(ctx, saleID); err == nil {
			if err := q.ReverseRedemption(ctx, rd.ID); err != nil {
				return err
			}
			if err := q.ReleaseVoucher(ctx, rd.VoucherID); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{StoreID: &sale.StoreID, Action: "TRANSACTION_VOIDED", Entity: "sale", EntityID: saleID.String(),
			New: map[string]any{"document_number": sale.DocumentNumber, "reason": reason, "approved_by": approvedBy, "grand_total": sale.GrandTotal.String()}}); err != nil {
			return err
		}
		return outbox.Publish(ctx, q, p.CompanyID, EventSaleVoided, "sale", saleID, map[string]any{"store_id": sale.StoreID, "cashier_id": sale.CashierID,
			"document_number": sale.DocumentNumber, "grand_total": sale.GrandTotal.String(), "voided_by": p.UserID, "approved_by": approvedBy})
	})
	if err != nil {
		return nil, err
	}
	return s.detail(ctx, rq, p, saleID)
}

type ReturnItemInput struct {
	SaleItemID uuid.UUID       `json:"sale_item_id" validate:"required"`
	Qty        decimal.Decimal `json:"qty"`
	Restock    *bool           `json:"restock"` // default true (item back on the shelf)
}

type ReturnInput struct {
	Reason       string            `json:"reason" validate:"required,max=200"`
	RefundMethod string            `json:"refund_method" validate:"required,max=20"`
	Items        []ReturnItemInput `json:"items" validate:"required,min=1,dive"`
	Approval     *ApprovalInput    `json:"approval"`
}

type ReturnResult struct {
	DocumentNumber string          `json:"document_number"`
	RefundAmount   decimal.Decimal `json:"refund_amount"`
	Sale           *SaleDetail     `json:"sale"`
}

// Return refunds part or all of a completed sale. Quantities are capped by what was sold minus what was
// already returned. Refunds above pos.refund_approval_threshold need a manager PIN. A CASH refund needs
// the caller's open shift and reduces its expected cash.
func (s *Service) Return(ctx context.Context, saleID uuid.UUID, in ReturnInput) (*ReturnResult, error) {
	p := access.MustPrincipal(ctx)
	rq := s.tx.Queries()
	pre, err := rq.GetSale(ctx, gen.GetSaleParams{ID: saleID, CompanyID: p.CompanyID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !p.Grants.Can(PermRefund, pre.Path)) {
		return nil, apperr.NotFound("sale")
	}
	if err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(in.RefundMethod))

	var out ReturnResult
	var approvedBy *uuid.UUID
	for attempt := 0; attempt < 2; attempt++ { // attempt 2 = after approval was obtained
		err = s.tx.Do(ctx, func(q *gen.Queries) error {
			sale, err := q.GetSaleForUpdate(ctx, gen.GetSaleForUpdateParams{ID: saleID, CompanyID: p.CompanyID})
			if err != nil {
				return err
			}
			if sale.Status != "completed" {
				return apperr.Conflict("SALE_VOIDED", "Cannot return a voided sale")
			}
			snap, err := loadSnapshotForStore(ctx, q, p, sale.StoreID)
			if err != nil {
				return err
			}
			if !contains(snap.Strings("payment.methods"), method) {
				return apperr.New(422, "PAYMENT_METHOD_DISABLED", "Refund method not enabled: "+method)
			}
			items, err := q.ListSaleItems(ctx, saleID)
			if err != nil {
				return err
			}
			byID := map[uuid.UUID]gen.SaleItem{}
			for _, it := range items {
				byID[it.ID] = it
			}
			prev, err := q.ListReturnItemsBySale(ctx, saleID)
			if err != nil {
				return err
			}
			retQty, retAmt := map[uuid.UUID]decimal.Decimal{}, map[uuid.UUID]decimal.Decimal{}
			for _, r := range prev {
				retQty[r.SaleItemID] = retQty[r.SaleItemID].Add(r.Qty)
				retAmt[r.SaleItemID] = retAmt[r.SaleItemID].Add(r.Amount)
			}

			type line struct {
				item    gen.SaleItem
				qty     decimal.Decimal
				amount  decimal.Decimal
				restock bool
			}
			var lines []line
			total := decimal.Zero
			seen := map[uuid.UUID]bool{}
			for _, ri := range in.Items {
				it, ok := byID[ri.SaleItemID]
				if !ok || seen[ri.SaleItemID] {
					return apperr.Validation(map[string]string{"items": "unknown or duplicate sale item"})
				}
				seen[ri.SaleItemID] = true
				if !ri.Qty.IsPositive() {
					return apperr.Validation(map[string]string{"qty": "must be positive"})
				}
				remaining := it.Qty.Sub(retQty[it.ID])
				if ri.Qty.GreaterThan(remaining) {
					return apperr.Conflict("RETURN_EXCEEDS_SOLD", "Return quantity exceeds what is left to return").WithDetails(map[string]string{
						"sku": it.Sku, "returnable": remaining.String()})
				}
				var amount decimal.Decimal
				if ri.Qty.Equal(remaining) { // last piece: settle the exact remainder, no rounding drift
					amount = it.LineTotal.Sub(retAmt[it.ID])
				} else {
					amount = it.LineTotal.Mul(ri.Qty).Div(it.Qty).Round(0)
				}
				restock := ri.Restock == nil || *ri.Restock
				lines = append(lines, line{item: it, qty: ri.Qty, amount: amount, restock: restock})
				total = total.Add(amount)
			}

			threshold := snap.Decimal("pos.refund_approval_threshold")
			if total.GreaterThan(threshold) && !p.Grants.Can(PermRefundApprove, pre.Path) && approvedBy == nil {
				if in.Approval == nil || in.Approval.ApproverEmail == "" || in.Approval.PIN == "" {
					return apperr.New(403, "APPROVAL_REQUIRED", "Manager approval required for this refund amount").WithDetails(map[string]string{"permission": PermRefundApprove})
				}
				return errNeedApprovalCheck
			}

			var sh *gen.CashierShift
			if method == "CASH" {
				cur, err := shift.RequireOpen(ctx, q, p.UserID)
				if err != nil {
					return err
				}
				if cur.StoreID != sale.StoreID {
					return apperr.Conflict("SHIFT_STORE_MISMATCH", "Your open shift is in another store")
				}
				sh = &cur
			}

			doc, err := numbering.Next(ctx, q, p.CompanyID, sale.StoreID, "sale_return", s.now())
			if err != nil {
				return err
			}
			rid, _ := uuid.NewV7()
			var shiftID *uuid.UUID
			if sh != nil {
				shiftID = &sh.ID
			}
			if _, err := q.InsertReturn(ctx, gen.InsertReturnParams{ID: rid, CompanyID: p.CompanyID, StoreID: sale.StoreID, SaleID: saleID, ShiftID: shiftID,
				DocumentNumber: doc, Reason: in.Reason, RefundMethod: method, RefundAmount: total, ApprovedBy: approvedBy, CreatedBy: p.UserID}); err != nil {
				return err
			}
			sort.Slice(lines, func(i, j int) bool { return lines[i].item.VariantID.String() < lines[j].item.VariantID.String() })
			for _, l := range lines {
				iid, _ := uuid.NewV7()
				if _, err := q.InsertReturnItem(ctx, gen.InsertReturnItemParams{ID: iid, ReturnID: rid, SaleItemID: l.item.ID, Qty: l.qty, Amount: l.amount, Restock: l.restock, Reason: in.Reason}); err != nil {
					return err
				}
				if err := returnTags(ctx, q, p.CompanyID, l.item.ID, l.qty, l.restock); err != nil {
					return err
				}
				if l.restock {
					if _, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: &sale.StoreID, WarehouseID: l.item.WarehouseID, VariantID: l.item.VariantID,
						Type: inventory.SaleReturn, Qty: l.qty, UnitCost: l.item.UnitCost, RefType: "sale_return", RefID: &rid, DocumentNo: doc, Note: in.Reason, By: &p.UserID, AllowNegative: true}); err != nil {
						return err
					}
				}
			}
			if sale.CustomerID != nil && sale.GrandTotal.IsPositive() {
				// take back the share of earned points that matches what has now been refunded (never more than was earned)
				already, err := q.ReversedEarnForSale(ctx, &saleID)
				if err != nil {
					return err
				}
				target := decimal.NewFromInt(sale.PointsEarned).Mul(sale.RefundedTotal.Add(total)).Div(sale.GrandTotal).Floor().IntPart()
				if now := target - already; now > 0 {
					if err := customerpkg.ReverseEarnPartial(ctx, q, p.CompanyID, *sale.CustomerID, saleID, now, "return "+doc, &p.UserID); err != nil {
						return err
					}
				}
				if err := q.ReduceCustomerSpend(ctx, gen.ReduceCustomerSpendParams{ID: *sale.CustomerID, LifetimeSpend: total}); err != nil {
					return err
				}
			}
			if err := q.AddSaleRefunded(ctx, gen.AddSaleRefundedParams{ID: saleID, RefundedTotal: total}); err != nil {
				return err
			}
			if sh != nil {
				mid, _ := uuid.NewV7()
				if _, err := q.InsertCashMovement(ctx, gen.InsertCashMovementParams{ID: mid, CompanyID: p.CompanyID, StoreID: sale.StoreID, ShiftID: sh.ID, Type: "REFUND",
					Amount: total, Reason: "Return " + doc, RefType: "sale_return", RefID: &rid, CreatedBy: p.UserID}); err != nil {
					return err
				}
			}
			if err := audit.Record(ctx, q, audit.Entry{StoreID: &sale.StoreID, Action: "REFUND_CREATED", Entity: "sale_return", EntityID: rid.String(),
				New: map[string]any{"sale": sale.DocumentNumber, "document_number": doc, "amount": total.String(), "method": method, "reason": in.Reason, "approved_by": approvedBy}}); err != nil {
				return err
			}
			if err := outbox.Publish(ctx, q, p.CompanyID, EventSaleRefunded, "sale", saleID, map[string]any{"store_id": sale.StoreID, "return_id": rid, "document_number": doc,
				"amount": total.String(), "method": method}); err != nil {
				return err
			}
			out.DocumentNumber, out.RefundAmount = doc, total
			return nil
		})
		if errors.Is(err, errNeedApprovalCheck) {
			id, aerr := s.requireApproval(ctx, p, pre.StoreID, pre.Path, PermRefundApprove, in.Approval)
			if aerr != nil {
				return nil, aerr
			}
			approvedBy = id
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}
	d, err := s.detail(ctx, rq, p, saleID)
	if err != nil {
		return nil, err
	}
	out.Sale = d
	return &out, nil
}

var errNeedApprovalCheck = errors.New("verify approval then retry")

func loadSnapshotForStore(ctx context.Context, q *gen.Queries, p *access.Principal, storeID uuid.UUID) (settings.Snapshot, error) {
	st, err := q.GetStore(ctx, gen.GetStoreParams{ID: storeID, CompanyID: p.CompanyID})
	if err != nil {
		return nil, err
	}
	return settings.LoadSnapshot(ctx, q, p.CompanyID, st.OrgUnitID)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// returnTags puts the RFID tags of a returned sale line back: restocked items become RETURNED (sellable again),
// items returned without restock (defective) are written off as DAMAGED so they can never be sold or pass the gate as new.
func returnTags(ctx context.Context, q *gen.Queries, companyID, saleItemID uuid.UUID, qty decimal.Decimal, restock bool) error {
	n := int32(qty.IntPart())
	if n <= 0 {
		return nil
	}
	epcs, err := q.ListUnreturnedSaleItemTags(ctx, gen.ListUnreturnedSaleItemTagsParams{SaleItemID: saleItemID, RowLimit: n})
	if err != nil || len(epcs) == 0 {
		return err
	}
	if err := q.MarkSaleItemTagsReturned(ctx, gen.MarkSaleItemTagsReturnedParams{SaleItemID: saleItemID, Epcs: epcs}); err != nil {
		return err
	}
	if restock {
		_, err = q.ReturnTags(ctx, gen.ReturnTagsParams{CompanyID: companyID, Epcs: epcs})
	} else {
		_, err = q.DamageTags(ctx, gen.DamageTagsParams{CompanyID: companyID, Epcs: epcs})
	}
	return err
}
