package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type suggestionOut struct {
	VariantID    string `json:"variant_id"`
	SKU          string `json:"sku"`
	SuggestedQty string `json:"suggested_qty"`
	Urgency      string `json:"urgency"`
	SupplierID   string `json:"supplier_id"`
	WarehouseID  string `json:"warehouse_id"`
	DailyDemand  string `json:"daily_demand"`
	StoreName    string `json:"store_name"`
}

func TestReplenishmentSuggestionsAndDraftPO(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "RP-A", "")
	other := tn.store(t, "RP-B", "")
	a := tn.product(t, "RPA", "50000")
	b := tn.product(t, "RPB", "20000")
	tn.stock(t, st, a, 100, "20000")
	tn.stock(t, st, b, 100, "8000")
	tn.stock(t, other, a, 50, "20000")
	var sup struct{ ID string }
	tn.Owner.mustOK("POST", "/suppliers", map[string]any{"code": "SUP1", "name": "PT Pemasok", "lead_time_days": 2}, &sup)
	tn.Owner.mustOK("PUT", "/suppliers/"+sup.ID+"/products", map[string]any{"variant_id": a.VariantID, "purchase_price": 19000, "moq": 10, "lead_time_days": 2}, nil)
	tn.backdatePrices(t)

	// sell 90 of A over the last days (10/day-ish): stock 100 -> 10
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, tn.Owner, term, 0)
	for i := 0; i < 9; i++ {
		tn.Owner.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 10}}, []map[string]any{{"method": "CASH", "amount": 600000}}), nil)
	}
	// spread the sales over the window so demand is per day, not one spike
	if _, err := testPool.Exec(context.Background(), `update sales set business_date = current_date - (floor(random()*20))::int where company_id=$1`, uuid.MustParse(tn.CompanyID)); err != nil {
		t.Fatal(err)
	}

	var res struct {
		WindowDays  int             `json:"window_days"`
		Suggestions []suggestionOut `json:"suggestions"`
		TotalCost   string          `json:"total_cost"`
	}
	tn.Owner.mustOK("GET", "/replenishment?store_id="+st.Store.ID, nil, &res)
	if len(res.Suggestions) != 1 || res.Suggestions[0].SKU != "RPA" {
		t.Fatalf("only the SKU that sells is suggested (B never sold, A in the other store was not asked for): %+v", res.Suggestions)
	}
	s := res.Suggestions[0]
	// 90 sold / 28 days ≈ 3.21/day; (2 lead + 7 cover) × 3.21 ≈ 28.9 target; position 10 -> need ≈ 18.9 -> MOQ 10 -> 20
	if s.SuggestedQty != "20" || s.SupplierID != sup.ID || s.StoreName == "" || s.Urgency == "" {
		t.Fatalf("suggestion: %+v", s)
	}

	// scope: a manager of the other store sees only their own store's data
	mgr := tn.user(t, "store_manager", other.Store.OrgUnitID)
	var theirs struct {
		Suggestions []suggestionOut `json:"suggestions"`
	}
	mgr.mustOK("GET", "/replenishment", nil, &theirs)
	if len(theirs.Suggestions) != 0 {
		t.Fatalf("another store's manager sees %d suggestions", len(theirs.Suggestions))
	}
	cashier := tn.user(t, "cashier", st.Store.OrgUnitID)
	cashier.wantErr("POST", "/replenishment/draft-po", map[string]any{"lines": []map[string]any{{"warehouse_id": s.WarehouseID, "supplier_id": s.SupplierID, "variant_id": s.VariantID, "qty": 20}}}, 403, "FORBIDDEN")
	tn.Owner.wantErr("GET", "/replenishment?window_days=999", nil, 422, "VALIDATION_FAILED")

	// one click: a DRAFT purchase order for the supplier, priced from the supplier's list
	var drafts []struct {
		ID             string `json:"id"`
		DocumentNumber string `json:"document_number"`
		Lines          int    `json:"lines"`
		Total          string `json:"total"`
	}
	tn.Owner.mustOK("POST", "/replenishment/draft-po", map[string]any{"lines": []map[string]any{{"warehouse_id": s.WarehouseID, "supplier_id": s.SupplierID, "variant_id": s.VariantID, "qty": 20}}}, &drafts)
	if len(drafts) != 1 || drafts[0].Lines != 1 || drafts[0].Total != "380000" {
		t.Fatalf("draft: %+v", drafts)
	}
	var po struct {
		Status string `json:"status"`
	}
	tn.Owner.mustOK("GET", "/purchase-orders/"+drafts[0].ID, nil, &po)
	if po.Status != "draft" {
		t.Fatalf("po status %q", po.Status)
	}
	tn.Owner.wantErr("POST", "/replenishment/draft-po", map[string]any{"lines": []map[string]any{{"warehouse_id": s.WarehouseID, "supplier_id": s.SupplierID, "variant_id": s.VariantID, "qty": 0}}}, 422, "VALIDATION_FAILED")
	_ = time.Now
}
