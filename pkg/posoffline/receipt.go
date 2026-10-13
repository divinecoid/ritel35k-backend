// Package posoffline is everything the store needs to keep selling when the cloud is unreachable, written as pure
// functions over a versioned snapshot so the SAME code runs in the edge agent and is differential-tested against the
// online pricing path: pricing (price lists, tiers, promotions, tax), receipt rendering, and the sync command contract.
package posoffline

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
)

type ReceiptLine struct {
	Name     string          `json:"name"`
	Qty      decimal.Decimal `json:"qty"`
	Price    decimal.Decimal `json:"unit_price"`
	Discount decimal.Decimal `json:"discount"`
	Total    decimal.Decimal `json:"total"`
}

type ReceiptPayment struct {
	Method   string          `json:"method"`
	Amount   decimal.Decimal `json:"amount"`
	Tendered decimal.Decimal `json:"tendered"`
}

type Receipt struct {
	StoreName       string           `json:"store_name"`
	StoreCode       string           `json:"store_code"`
	Address         string           `json:"address"`
	Header          string           `json:"header"`
	Footer          string           `json:"footer"`
	DocumentNumber  string           `json:"document_number"`
	Status          string           `json:"status"`
	PrintedAt       time.Time        `json:"date"`
	Cashier         string           `json:"cashier"`
	Terminal        string           `json:"terminal"`
	Lines           []ReceiptLine    `json:"lines"`
	Subtotal        decimal.Decimal  `json:"subtotal"`
	Discount        decimal.Decimal  `json:"discount"`
	Tax             decimal.Decimal  `json:"tax"`
	TaxIncluded     bool             `json:"tax_included"`
	Total           decimal.Decimal  `json:"total"`
	Payments        []ReceiptPayment `json:"payments"`
	Change          decimal.Decimal  `json:"change"`
	PromoDiscount   decimal.Decimal  `json:"promo_discount"`
	VoucherDiscount decimal.Decimal  `json:"voucher_discount"`
	Customer        string           `json:"customer"`
	MemberNo        string           `json:"member_no"`
	PointsEarned    int64            `json:"points_earned"`
	PointsRedeemed  int64            `json:"points_redeemed"`
	PointsBalance   int64            `json:"points_balance"`
}

func rp(d decimal.Decimal) string { // 1234567 -> 1.234.567
	s := d.Round(0).String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func pad(left, right string, w int) string {
	gap := w - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	if gap < 1 {
		gap = 1
		if max := w - utf8.RuneCountInString(right) - 1; max > 0 && utf8.RuneCountInString(left) > max {
			left = string([]rune(left)[:max])
		}
	}
	return left + strings.Repeat(" ", gap) + right
}

func center(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return strings.Repeat(" ", (w-n)/2) + s
}

// Text renders a fixed-width receipt (32 columns = 58mm, 42/48 = 80mm) for thermal printers.
func (r *Receipt) Text(width int) string {
	if width < 24 {
		width = 32
	}
	line := strings.Repeat("-", width)
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	w(center(r.StoreName, width))
	if r.Address != "" {
		w(center(r.Address, width))
	}
	if r.Header != "" {
		w(center(r.Header, width))
	}
	w(line)
	w(pad("No", r.DocumentNumber, width))
	w(pad("Tgl", r.PrintedAt.Format("02/01/2006 15:04"), width))
	w(pad("Kasir", r.Cashier+" / "+r.Terminal, width))
	if r.Status == "voided" {
		w(center("*** VOID ***", width))
	}
	w(line)
	for _, l := range r.Lines {
		w(l.Name)
		w(pad(fmt.Sprintf("  %s x %s", l.Qty.String(), rp(l.Price)), rp(l.Qty.Mul(l.Price)), width))
		if l.Discount.IsPositive() {
			w(pad("  Diskon", "-"+rp(l.Discount), width))
		}
	}
	w(line)
	w(pad("Subtotal", rp(r.Subtotal), width))
	if r.PromoDiscount.IsPositive() {
		w(pad("  Promo", "-"+rp(r.PromoDiscount), width))
	}
	if r.VoucherDiscount.IsPositive() {
		w(pad("  Voucher", "-"+rp(r.VoucherDiscount), width))
	}
	if r.Discount.IsPositive() {
		w(pad("Total diskon", "-"+rp(r.Discount), width))
	}
	if r.TaxIncluded {
		w(pad("PPN (termasuk)", rp(r.Tax), width))
	} else if r.Tax.IsPositive() {
		w(pad("PPN", rp(r.Tax), width))
	}
	w(pad("TOTAL", rp(r.Total), width))
	for _, p := range r.Payments {
		w(pad(p.Method, rp(p.Tendered), width))
	}
	if r.Change.IsPositive() {
		w(pad("Kembali", rp(r.Change), width))
	}
	if r.Customer != "" {
		w(line)
		w(pad("Member", r.MemberNo, width))
		w(r.Customer)
		if r.PointsRedeemed > 0 {
			w(pad("Poin dipakai", "-"+rp(decimal.NewFromInt(r.PointsRedeemed)), width))
		}
		w(pad("Poin didapat", "+"+rp(decimal.NewFromInt(r.PointsEarned)), width))
		w(pad("Saldo poin", rp(decimal.NewFromInt(r.PointsBalance)), width))
	}
	w(line)
	if r.Footer != "" {
		w(center(r.Footer, width))
	}
	return b.String()
}
