package posoffline

import (
	"strings"

	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/pkg/apperr"
)

// PayInput is a tender as the cashier enters it.
type PayInput struct {
	Method    string
	Amount    decimal.Decimal // tendered (cash handed over) or charged (non-cash)
	Reference string
}

// Applied is a tender after allocation: Amount is what went to the bill, Tendered what was handed over.
type Applied struct {
	Method    string
	Amount    decimal.Decimal
	Tendered  decimal.Decimal
	Reference string
}

// Allocate validates the tender against the bill and computes change. Non-cash tenders must not exceed the bill; cash
// can, and the excess is returned as change. The online checkout and the offline till share this function, so a bill
// is split the same way whichever side takes the money.
func Allocate(grand decimal.Decimal, pays []PayInput, enabled []string) ([]Applied, decimal.Decimal, error) {
	if grand.IsZero() {
		if len(pays) > 0 {
			return nil, decimal.Zero, apperr.Validation(map[string]string{"payments": "nothing to pay"})
		}
		return nil, decimal.Zero, nil
	}
	if len(pays) == 0 {
		return nil, decimal.Zero, apperr.New(422, "PAYMENT_INSUFFICIENT", "Payment is required")
	}
	ok := map[string]bool{}
	for _, m := range enabled {
		ok[m] = true
	}
	var noncash, cash []Applied
	nonSum := decimal.Zero
	for _, p := range pays {
		m := strings.ToUpper(strings.TrimSpace(p.Method))
		if !ok[m] {
			return nil, decimal.Zero, apperr.New(422, "PAYMENT_METHOD_DISABLED", "Payment method not enabled for this store: "+m)
		}
		if !p.Amount.IsPositive() {
			return nil, decimal.Zero, apperr.Validation(map[string]string{"payments": "amounts must be positive"})
		}
		if m == "CASH" {
			cash = append(cash, Applied{Method: m, Tendered: p.Amount, Reference: p.Reference})
		} else {
			noncash = append(noncash, Applied{Method: m, Amount: p.Amount, Tendered: p.Amount, Reference: p.Reference})
			nonSum = nonSum.Add(p.Amount)
		}
	}
	if nonSum.GreaterThan(grand) {
		return nil, decimal.Zero, apperr.New(422, "PAYMENT_EXCESS", "Non-cash payments exceed the total")
	}
	remaining := grand.Sub(nonSum)
	if remaining.IsZero() && len(cash) > 0 {
		return nil, decimal.Zero, apperr.New(422, "PAYMENT_EXCESS", "Cash is not needed: the total is already covered")
	}
	cashTendered := decimal.Zero
	for _, c := range cash {
		cashTendered = cashTendered.Add(c.Tendered)
	}
	if cashTendered.LessThan(remaining) {
		return nil, decimal.Zero, apperr.New(422, "PAYMENT_INSUFFICIENT", "Payment is less than the total").WithDetails(map[string]string{"short_by": remaining.Sub(cashTendered).String()})
	}
	change := cashTendered.Sub(remaining)
	left := remaining
	for i := range cash {
		take := decimal.Min(cash[i].Tendered, left)
		cash[i].Amount = take
		left = left.Sub(take)
	}
	out := append(noncash, cash...)
	final := out[:0]
	for _, a := range out {
		if a.Amount.IsPositive() {
			final = append(final, a)
		} else {
			// a cash line fully consumed by change cannot be represented; reject so the till stays honest
			return nil, decimal.Zero, apperr.New(422, "PAYMENT_EXCESS", "A cash line is not needed to cover the total")
		}
	}
	return final, change, nil
}
