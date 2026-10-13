package pos

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/internal/audit"
	customerpkg "github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/inventory"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/outbox"
	"github.com/ritel35k/backend/internal/shift"
	"github.com/ritel35k/backend/internal/voucher"
	"github.com/ritel35k/backend/pkg/apperr"
	"github.com/ritel35k/backend/pkg/posoffline"
)

type PaymentInput struct {
	Method    string          `json:"method" validate:"required,max=20"`
	Amount    decimal.Decimal `json:"amount"` // tendered (cash handed over) or charged (non-cash)
	Reference string          `json:"reference" validate:"max=100"`
	Points    int64           `json:"points"` // method POINTS: loyalty points to redeem (amount is derived from the settings)
}

type CheckoutInput struct {
	CartInput
	ClientTxnID uuid.UUID      `json:"client_txn_id" validate:"required"`
	Payments    []PaymentInput `json:"payments" validate:"max=10,dive"`
}

type applied struct {
	method    string
	amount    decimal.Decimal // applied to the bill
	tendered  decimal.Decimal
	reference string
}

// allocatePayments validates the tender against the bill and computes change (shared with the offline till).
func allocatePayments(grand decimal.Decimal, pays []PaymentInput, enabled []string) ([]applied, decimal.Decimal, error) {
	in := make([]posoffline.PayInput, len(pays))
	for i, p := range pays {
		in[i] = posoffline.PayInput{Method: p.Method, Amount: p.Amount, Reference: p.Reference}
	}
	out, change, err := posoffline.Allocate(grand, in, enabled)
	if err != nil {
		return nil, decimal.Zero, err
	}
	res := make([]applied, len(out))
	for i, a := range out {
		res[i] = applied{method: a.Method, amount: a.Amount, tendered: a.Tendered, reference: a.Reference}
	}
	return res, change, nil
}

type SaleDetail struct {
	gen.GetSaleRow
	Items      []gen.SaleItem          `json:"items"`
	Payments   []gen.Payment           `json:"payments"`
	Returns    []gen.SaleReturn        `json:"returns"`
	Promotions []gen.SaleItemPromotion `json:"promotions"`
}

var errDuplicateTxn = errors.New("duplicate client txn")

// Checkout prices the cart server-side and persists sale + items + payments + stock movements +
// outbox event atomically. It is idempotent on (terminal, client_txn_id): replaying a request returns
// the original sale without touching stock again (safe retry for flaky networks and offline sync).
func (s *Service) Checkout(ctx context.Context, in CheckoutInput) (*SaleDetail, bool, error) {
	p := access.MustPrincipal(ctx)
	rq := s.tx.Queries()

	if id, err := rq.GetSaleByClientTxn(ctx, gen.GetSaleByClientTxnParams{TerminalID: in.TerminalID, ClientTxnID: in.ClientTxnID}); err == nil {
		d, err := s.detail(ctx, rq, p, id)
		return d, true, err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}

	// Pre-flight (read-only): does this cart need a supervisor? Verify the PIN BEFORE the sale transaction
	// so the sale tx stays short and PIN throttling/auditing is independent of its outcome.
	cs, err := loadStore(ctx, rq, p, PermCreate, in.TerminalID)
	if err != nil {
		return nil, false, err
	}
	pre, _, err := s.priceCart(ctx, rq, p, cs, in.CartInput)
	if err != nil {
		return nil, false, err
	}
	var approvedBy *uuid.UUID
	if pre.ApprovalRequired {
		approvedBy, err = s.requireApproval(ctx, p, cs.store.ID, cs.term.Path, PermDiscount, in.Approval)
		if err != nil {
			return nil, false, err
		}
	}

	var saleID uuid.UUID
	err = s.tx.Do(ctx, func(q *gen.Queries) error {
		// re-price inside the transaction so what we persist is what the DB says right now
		quote, ex, err := s.priceCart(ctx, q, p, cs, in.CartInput)
		if err != nil {
			return err
		}
		if quote.ApprovalRequired && approvedBy == nil {
			return apperr.New(403, "APPROVAL_REQUIRED", "Supervisor approval required").WithDetails(map[string]string{"permission": PermDiscount})
		}
		sh, err := shift.RequireOpen(ctx, q, p.UserID)
		if err != nil {
			return err
		}
		if sh.TerminalID != in.TerminalID {
			return apperr.Conflict("SHIFT_TERMINAL_MISMATCH", "Your open shift belongs to a different terminal")
		}
		// loyalty points are a tender: validated against the member's balance and the store's limits
		var pointsUsed int64
		pointsValue := decimal.Zero
		var moneyPays []PaymentInput
		for _, pm := range in.Payments {
			if strings.EqualFold(strings.TrimSpace(pm.Method), "POINTS") {
				pointsUsed += pm.Points
			} else {
				moneyPays = append(moneyPays, pm)
			}
		}
		if pointsUsed != 0 {
			switch {
			case ex.customer == nil:
				return apperr.New(422, "POINTS_NEED_CUSTOMER", "Attach a member to pay with points")
			case !ex.rules.Enabled:
				return apperr.New(422, "LOYALTY_DISABLED", "Loyalty is not enabled for this store")
			case pointsUsed < ex.rules.MinRedeemPoints:
				return apperr.New(422, "POINTS_BELOW_MINIMUM", "Minimum redemption is "+itoa(int(ex.rules.MinRedeemPoints))+" points")
			case pointsUsed > ex.rules.MaxRedeemable(ex.customer.PointsBalance, quote.GrandTotal):
				return apperr.New(422, "POINTS_EXCEED_LIMIT", "Points exceed the balance or the maximum share of the bill")
			}
			pointsValue = ex.rules.Value(pointsUsed)
		}
		pays, change, err := allocatePayments(quote.GrandTotal.Sub(pointsValue), moneyPays, quote.PaymentMethods)
		if err != nil {
			return err
		}
		if pointsUsed > 0 {
			pays = append([]applied{{method: "POINTS", amount: pointsValue, tendered: pointsValue, reference: itoa(int(pointsUsed)) + " poin"}}, pays...)
		}
		wh, err := q.GetDefaultWarehouse(ctx, &cs.store.ID)
		if err != nil {
			return apperr.Conflict("NO_WAREHOUSE", "Store has no active default warehouse")
		}

		// 1) stock — in a stable variant order so concurrent sales cannot deadlock
		saleUUID, _ := uuid.NewV7()
		allowNeg := cs.snap.Bool("pos.allow_negative_stock")
		order := make([]int, len(quote.Lines))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			return quote.Lines[order[a]].VariantID.String() < quote.Lines[order[b]].VariantID.String()
		})
		costs := make([]decimal.Decimal, len(quote.Lines))
		for _, i := range order {
			l := quote.Lines[i]
			res, err := inventory.Apply(ctx, q, inventory.Movement{CompanyID: p.CompanyID, StoreID: &cs.store.ID, WarehouseID: wh.ID, VariantID: l.VariantID,
				Type: inventory.Sale, Qty: l.Qty.Neg(), RefType: "sale", RefID: &saleUUID, By: &p.UserID, AllowNegative: allowNeg})
			if err != nil {
				return err
			}
			costs[i] = res.AvgCost
		}

		// 2) loyalty: lock the member row, spend then earn, maybe level up (all rolled back with the sale)
		var earned int64
		if c := ex.customer; c != nil {
			if _, err := q.GetCustomerForUpdate(ctx, gen.GetCustomerForUpdateParams{ID: c.ID, CompanyID: p.CompanyID}); err != nil {
				return err
			}
			if pointsUsed > 0 {
				if err := customerpkg.Redeem(ctx, q, p.CompanyID, c.ID, saleUUID, pointsUsed, &p.UserID); err != nil {
					return err
				}
			}
			earned = ex.rules.PointsFor(quote.GrandTotal.Sub(pointsValue), c.EarnMultiplier)
			if err := customerpkg.Earn(ctx, q, p.CompanyID, c.ID, saleUUID, earned, ex.rules.ExpiryDays, s.now(), &p.UserID); err != nil {
				return err
			}
			if err := customerpkg.AfterPurchase(ctx, q, p.CompanyID, c.ID, quote.GrandTotal); err != nil {
				return err
			}
		}

		// 3) document number LAST: its counter row is locked until commit, so keep that window tiny
		now := s.now()
		doc, err := numbering.Next(ctx, q, p.CompanyID, cs.store.ID, "sale", now)
		if err != nil {
			return err
		}
		loc, _ := time.LoadLocation(cs.store.Timezone)
		if loc == nil {
			loc = time.UTC
		}
		bizDate := now.In(loc)

		paid := decimal.Zero
		for _, a := range pays {
			paid = paid.Add(a.amount)
		}
		sale, err := q.InsertSale(ctx, gen.InsertSaleParams{ID: saleUUID, CompanyID: p.CompanyID, StoreID: cs.store.ID, TerminalID: in.TerminalID, ShiftID: sh.ID,
			CashierID: p.UserID, DocumentNumber: doc, ClientTxnID: in.ClientTxnID, BusinessDate: dateOnly(bizDate), Subtotal: quote.Subtotal,
			DiscountTotal: quote.DiscountTotal, TaxTotal: quote.TaxTotal, GrandTotal: quote.GrandTotal, PaidTotal: paid, ChangeAmount: change, ApprovedBy: approvedBy,
			CustomerID: in.CustomerID, VoucherID: voucherID(ex), VoucherDiscount: quote.VoucherDiscount, PromoDiscount: quote.PromoDiscount, PointsEarned: earned, PointsRedeemed: pointsUsed})
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "sales_terminal_id_client_txn_id_key" {
				return errDuplicateTxn
			}
			return err
		}
		if len(ex.epcs) > 0 { // RFID: the tags leave as SOLD in this very transaction (strict anti-fraud coupling)
			tags := make([]string, 0, len(ex.epcs))
			for _, e := range ex.epcs {
				tags = append(tags, e)
			}
			n, err := q.SellTags(ctx, gen.SellTagsParams{CompanyID: p.CompanyID, Epcs: tags, StoreID: &cs.store.ID, SaleID: &sale.ID})
			if err != nil {
				return err
			}
			if int(n) != len(tags) { // another terminal sold one of them first, or it changed state since the quote
				return apperr.Conflict("TAG_NOT_SELLABLE", "One of the scanned tags is no longer available; rescan the cart")
			}
		}
		for i, l := range quote.Lines {
			iid, _ := uuid.NewV7()
			if _, err := q.InsertSaleItem(ctx, gen.InsertSaleItemParams{ID: iid, SaleID: sale.ID, LineNo: int32(i + 1), VariantID: l.VariantID, Sku: l.SKU,
				ProductName: l.Name, Barcode: l.Barcode, Qty: l.Qty, UnitPrice: l.UnitPrice, DiscountPercent: l.DiscountPercent, DiscountAmount: l.DiscountAmount,
				TaxRate: l.TaxRate, TaxAmount: l.TaxAmount, LineTotal: l.LineTotal, UnitCost: costs[i], WarehouseID: wh.ID, PromoDiscount: l.PromoDiscount}); err != nil {
				return err
			}
			if epc := ex.epcs[i]; epc != "" {
				if err := q.InsertSaleItemTag(ctx, gen.InsertSaleItemTagParams{SaleItemID: iid, Epc: epc}); err != nil {
					return err
				}
			}
			for _, a := range l.Promotions {
				if err := q.InsertSaleItemPromotion(ctx, gen.InsertSaleItemPromotionParams{SaleItemID: iid, PromotionID: a.PromotionID, Name: a.Name, Amount: a.Amount}); err != nil {
					return err
				}
			}
		}
		if ex.voucher != nil {
			if err := voucher.Claim(ctx, q, ex.voucher.ID); err != nil {
				return err
			}
			rid, _ := uuid.NewV7()
			if err := q.InsertRedemption(ctx, gen.InsertRedemptionParams{ID: rid, VoucherID: ex.voucher.ID, SaleID: sale.ID, CustomerID: in.CustomerID, Amount: quote.VoucherDiscount}); err != nil {
				return err
			}
		}
		for _, a := range pays {
			pid, _ := uuid.NewV7()
			if _, err := q.InsertPayment(ctx, gen.InsertPaymentParams{ID: pid, SaleID: sale.ID, Method: a.method, Amount: a.amount, Tendered: a.tendered, Reference: a.reference}); err != nil {
				return err
			}
		}
		if err := outbox.Publish(ctx, q, p.CompanyID, EventSaleCompleted, "sale", sale.ID, map[string]any{
			"store_id": cs.store.ID, "terminal_id": in.TerminalID, "shift_id": sh.ID, "document_number": doc, "grand_total": quote.GrandTotal.String(),
			"discount_total": quote.DiscountTotal.String(), "promo_discount": quote.PromoDiscount.String(), "voucher_discount": quote.VoucherDiscount.String(),
			"tax_total": quote.TaxTotal.String(), "items": len(quote.Lines), "business_date": bizDate.Format("2006-01-02"), "customer_id": in.CustomerID,
			"points_earned": earned, "points_redeemed": pointsUsed}); err != nil {
			return err
		}
		saleID = sale.ID
		return nil
	})
	if errors.Is(err, errDuplicateTxn) { // concurrent replay of the same request: the other one won
		id, gerr := rq.GetSaleByClientTxn(ctx, gen.GetSaleByClientTxnParams{TerminalID: in.TerminalID, ClientTxnID: in.ClientTxnID})
		if gerr != nil {
			return nil, false, gerr
		}
		d, derr := s.detail(ctx, rq, p, id)
		return d, true, derr
	}
	if err != nil {
		return nil, false, err
	}
	d, err := s.detail(ctx, rq, p, saleID)
	return d, false, err
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

var _ = audit.Entry{}

func voucherID(ex *extras) *uuid.UUID {
	if ex == nil || ex.voucher == nil {
		return nil
	}
	return &ex.voucher.ID
}
