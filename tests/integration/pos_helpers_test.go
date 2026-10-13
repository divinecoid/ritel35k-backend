package integration

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func ean13(n int64) string {
	p := fmt.Sprintf("%012d", n)
	sum := 0
	for i, c := range p {
		d := int(c - '0')
		if i%2 == 1 {
			d *= 3
		}
		sum += d
	}
	return p + fmt.Sprint((10-sum%10)%10)
}

type item struct{ VariantID, Barcode string }

// product creates a product (company-wide default price) and returns its variant id and barcode.
func (tn *tenant) product(t *testing.T, code string, price string) item {
	t.Helper()
	var uoms []struct{ ID, Code string }
	tn.Owner.mustOK("GET", "/uoms", nil, &uoms)
	var pcs string
	for _, u := range uoms {
		if u.Code == "PCS" {
			pcs = u.ID
		}
	}
	bc := ean13(seq.Add(1) + 100000)
	var p struct {
		Variants []struct {
			ID string `json:"id"`
		} `json:"variants"`
	}
	body := map[string]any{"code": code, "name": "Item " + code, "uom_id": pcs, "variants": []map[string]any{{
		"sku": code, "purchase_price": "10000", "selling_price": price, "barcodes": []map[string]any{{"barcode": bc}}}}}
	tn.Owner.mustOK("POST", "/products", body, &p)
	return item{VariantID: p.Variants[0].ID, Barcode: bc}
}

func (tn *tenant) stock(t *testing.T, store storeOut, it item, qty int, cost string) {
	t.Helper()
	tn.Owner.mustOK("POST", "/inventory/adjustments", map[string]any{"store_id": store.Store.ID, "type": "OPENING", "reason": "opening stock",
		"lines": []map[string]any{{"variant_id": it.VariantID, "qty_delta": qty, "unit_cost": cost}}}, nil)
}

func terminalOf(t *testing.T, c *client, storeID string) string {
	t.Helper()
	var ts []struct {
		ID, Code string
	}
	c.mustOK("GET", "/pos/terminals?store_id="+storeID, nil, &ts)
	if len(ts) == 0 {
		t.Fatal("store has no terminal")
	}
	return ts[0].ID
}

func openShift(t *testing.T, c *client, terminal string, cash int) string {
	t.Helper()
	var sh struct{ ID string }
	c.mustOK("POST", "/pos/shifts/open", map[string]any{"terminal_id": terminal, "opening_cash": cash}, &sh)
	return sh.ID
}

type saleOut struct {
	ID             string `json:"id"`
	DocumentNumber string `json:"document_number"`
	Status         string `json:"status"`
	GrandTotal     string `json:"grand_total"`
	TaxTotal       string `json:"tax_total"`
	ChangeAmount   string `json:"change_amount"`
	Items          []struct {
		ID        string `json:"id"`
		UnitCost  string `json:"unit_cost"`
		LineTotal string `json:"line_total"`
	} `json:"items"`
}

func cart(terminal string, qty int, its ...item) map[string]any {
	items := []map[string]any{}
	for _, it := range its {
		items = append(items, map[string]any{"barcode": it.Barcode, "qty": qty})
	}
	return map[string]any{"terminal_id": terminal, "items": items}
}

func checkoutBody(terminal string, items []map[string]any, pays []map[string]any) map[string]any {
	return map[string]any{"terminal_id": terminal, "client_txn_id": uuid.NewString(), "items": items, "payments": pays}
}

func (tn *tenant) onHand(t *testing.T, store storeOut, it item) string {
	t.Helper()
	var rows []struct {
		OnHand string `json:"on_hand"`
	}
	tn.Owner.mustOK("GET", "/inventory/balances?store_id="+store.Store.ID+"&variant_id="+it.VariantID, nil, &rows)
	if len(rows) == 0 {
		return "none"
	}
	return rows[0].OnHand
}
