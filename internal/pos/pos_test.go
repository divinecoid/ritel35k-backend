package pos

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func d(n int64) decimal.Decimal { return decimal.NewFromInt(n) }

func TestAllocatePayments(t *testing.T) {
	enabled := []string{"CASH", "QRIS"}
	cases := []struct {
		name     string
		grand    int64
		pays     []PaymentInput
		wantCode string
		change   int64
	}{
		{"exact cash", 100, []PaymentInput{{Method: "CASH", Amount: d(100)}}, "", 0},
		{"change", 100, []PaymentInput{{Method: "cash", Amount: d(150)}}, "", 50},
		{"split", 100, []PaymentInput{{Method: "QRIS", Amount: d(40)}, {Method: "CASH", Amount: d(70)}}, "", 10},
		{"short", 100, []PaymentInput{{Method: "CASH", Amount: d(99)}}, "PAYMENT_INSUFFICIENT", 0},
		{"noncash over", 100, []PaymentInput{{Method: "QRIS", Amount: d(101)}}, "PAYMENT_EXCESS", 0},
		{"cash not needed", 100, []PaymentInput{{Method: "QRIS", Amount: d(100)}, {Method: "CASH", Amount: d(5)}}, "PAYMENT_EXCESS", 0},
		{"disabled method", 100, []PaymentInput{{Method: "GIRO", Amount: d(100)}}, "PAYMENT_METHOD_DISABLED", 0},
	}
	for _, c := range cases {
		got, change, err := allocatePayments(d(c.grand), c.pays, enabled)
		if c.wantCode != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantCode) {
				t.Errorf("%s: want %s, got %v", c.name, c.wantCode, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		sum := decimal.Zero
		for _, a := range got {
			sum = sum.Add(a.amount)
		}
		if !sum.Equal(d(c.grand)) || !change.Equal(d(c.change)) {
			t.Errorf("%s: applied=%s change=%s", c.name, sum, change)
		}
	}
}
