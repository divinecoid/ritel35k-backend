package pos

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/access"
	"github.com/ritel35k/backend/pkg/posoffline"
)

// The receipt layout is shared with the offline POS (edge agent), so it lives in pkg/posoffline.
type (
	ReceiptLine    = posoffline.ReceiptLine
	ReceiptPayment = posoffline.ReceiptPayment
	Receipt        = posoffline.Receipt
)

func (s *Service) Receipt(ctx context.Context, saleID uuid.UUID) (*Receipt, error) {
	p := access.MustPrincipal(ctx)
	q := s.tx.Queries()
	d, err := s.detail(ctx, q, p, saleID)
	if err != nil {
		return nil, err
	}
	snap, err := loadSnapshotForStore(ctx, q, p, d.StoreID)
	if err != nil {
		return nil, err
	}
	var addr struct {
		Line1 string `json:"line1"`
	}
	if st, err := q.GetStoreAddress(ctx, d.StoreID); err == nil {
		_ = json.Unmarshal(st, &addr)
	}
	loc, _ := time.LoadLocation(d.StoreTimezone)
	if loc == nil {
		loc = time.UTC
	}
	r := &Receipt{StoreName: d.StoreName, StoreCode: d.StoreCode, Address: addr.Line1, Header: snap.String("receipt.header"), Footer: snap.String("receipt.footer"),
		DocumentNumber: d.DocumentNumber, Status: d.Status, PrintedAt: d.CreatedAt.In(loc), Cashier: d.CashierName, Terminal: d.TerminalCode,
		Subtotal: d.Subtotal, Discount: d.DiscountTotal, Tax: d.TaxTotal, TaxIncluded: snap.Bool("tax.price_includes_tax"), Total: d.GrandTotal, Change: d.ChangeAmount}
	r.PromoDiscount, r.VoucherDiscount = d.PromoDiscount, d.VoucherDiscount
	if d.CustomerName != nil {
		r.Customer = *d.CustomerName
	}
	if d.CustomerMemberNo != nil {
		r.MemberNo = *d.CustomerMemberNo
	}
	r.PointsEarned, r.PointsRedeemed = d.PointsEarned, d.PointsRedeemed
	if d.CustomerPoints != nil {
		r.PointsBalance = *d.CustomerPoints
	}
	for _, it := range d.Items {
		r.Lines = append(r.Lines, ReceiptLine{Name: it.ProductName, Qty: it.Qty, Price: it.UnitPrice, Discount: it.DiscountAmount, Total: it.LineTotal})
	}
	for _, pay := range d.Payments {
		r.Payments = append(r.Payments, ReceiptPayment{Method: pay.Method, Amount: pay.Amount, Tendered: pay.Tendered})
	}
	return r, nil
}
