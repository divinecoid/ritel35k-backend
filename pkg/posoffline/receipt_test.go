package posoffline

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(n int64) decimal.Decimal { return decimal.NewFromInt(n) }

func TestReceiptText(t *testing.T) {
	r := &Receipt{StoreName: "Jakarta Barat", DocumentNumber: "SO/JKTB/2026/10/000001", PrintedAt: time.Date(2026, 10, 11, 9, 5, 0, 0, time.UTC),
		Cashier: "Siti", Terminal: "POS-01", Subtotal: d(80000), Tax: d(7928), TaxIncluded: true, Total: d(80000), Change: d(20000), Footer: "Terima kasih",
		Lines:    []ReceiptLine{{Name: "Sneakers Classic", Qty: d(2), Price: d(35000), Total: d(70000)}},
		Payments: []ReceiptPayment{{Method: "CASH", Amount: d(80000), Tendered: d(100000)}}}
	txt := r.Text(32)
	for _, want := range []string{"Jakarta Barat", "2 x 35.000", "70.000", "TOTAL", "80.000", "Kembali", "20.000", "Terima kasih"} {
		if !strings.Contains(txt, want) {
			t.Errorf("receipt missing %q\n%s", want, txt)
		}
	}
	for _, line := range strings.Split(txt, "\n") {
		if n := len([]rune(line)); n > 32 {
			t.Errorf("line over 32 cols (%d): %q", n, line)
		}
	}
	if rp(d(1234567)) != "1.234.567" || rp(d(-500)) != "-500" {
		t.Error("rupiah formatting")
	}
}
