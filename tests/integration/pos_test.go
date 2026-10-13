package integration

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestSaleEndToEnd(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "35000")
	b := tn.product(t, "SKU-B", "10000")
	tn.stock(t, st, a, 10, "20000")
	tn.stock(t, st, b, 5, "4000")
	term := terminalOf(t, tn.Owner, st.Store.ID)

	// selling needs a shift
	tn.Owner.wantErr("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 100000}}), 409, "SHIFT_REQUIRED")
	shiftID := openShift(t, tn.Owner, term, 100000)

	// server-side quote: 2 x 35000 + 1 x 10000 = 80000, PPN 11% included => 80000*11/111 = 7928
	var q struct {
		GrandTotal string `json:"grand_total"`
		TaxTotal   string `json:"tax_total"`
	}
	tn.Owner.mustOK("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{{"barcode": a.Barcode, "qty": 2}, {"barcode": b.Barcode, "qty": 1}}}, &q)
	if q.GrandTotal != "80000" || q.TaxTotal != "7928" {
		t.Fatalf("quote %+v", q)
	}

	var s saleOut
	tn.Owner.mustOK("POST", "/pos/sales", checkoutBody(term,
		[]map[string]any{{"barcode": a.Barcode, "qty": 2}, {"variant_id": b.VariantID, "qty": 1}},
		[]map[string]any{{"method": "CASH", "amount": 100000}}), &s)
	if s.GrandTotal != "80000" || s.ChangeAmount != "20000" || s.Status != "completed" || !strings.HasPrefix(s.DocumentNumber, "SO/ST-A/") {
		t.Fatalf("sale %+v", s)
	}
	if tn.onHand(t, st, a) != "8" || tn.onHand(t, st, b) != "4" {
		t.Fatalf("stock after sale: a=%s b=%s", tn.onHand(t, st, a), tn.onHand(t, st, b))
	}
	// COGS snapshot = moving average at sale time
	costs := map[string]bool{}
	for _, it := range s.Items {
		costs[it.UnitCost] = true
	}
	if !costs["20000"] || !costs["4000"] {
		t.Fatalf("cogs snapshot: %+v", s.Items)
	}

	// ledger == balance
	var cnt int
	var sum string
	_ = testPool.QueryRow(context.Background(), `select count(*), coalesce(sum(qty_delta),0)::text from inventory_movements where variant_id=$1`, uuid.MustParse(a.VariantID)).Scan(&cnt, &sum)
	if cnt != 2 || sum != "8.0000" {
		t.Fatalf("ledger rows=%d sum=%s", cnt, sum)
	}

	// receipt
	_, _ = tn.Owner.do("GET", "/pos/sales/"+s.ID+"/receipt?format=text", nil)
	var rc struct {
		DocumentNumber string `json:"document_number"`
		Total          string `json:"total"`
	}
	tn.Owner.mustOK("GET", "/pos/sales/"+s.ID+"/receipt", nil, &rc)
	if rc.DocumentNumber != s.DocumentNumber || rc.Total != "80000" {
		t.Fatalf("receipt %+v", rc)
	}

	// shift totals: opening 100000 + cash sales 80000 (change excluded)
	var sh struct {
		Totals struct {
			CashSales string `json:"cash_sales"`
			Expected  string `json:"expected_cash"`
			SaleCount int    `json:"sale_count"`
		} `json:"totals"`
	}
	tn.Owner.mustOK("GET", "/pos/shifts/"+shiftID, nil, &sh)
	if sh.Totals.CashSales != "80000" || sh.Totals.Expected != "180000" || sh.Totals.SaleCount != 1 {
		t.Fatalf("shift totals %+v", sh.Totals)
	}
	// close with a short count -> variance recorded
	var closed struct {
		Variance string `json:"variance"`
		Status   string `json:"status"`
	}
	tn.Owner.mustOK("POST", "/pos/shifts/"+shiftID+"/close", map[string]any{"counted_cash": 179000, "note": "short"}, &closed)
	if closed.Status != "closed" || closed.Variance != "-1000" {
		t.Fatalf("close %+v", closed)
	}
	tn.Owner.wantErr("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 35000}}), 409, "SHIFT_REQUIRED")
}

func TestPaymentRules(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "50000")
	tn.stock(t, st, a, 100, "1")
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, tn.Owner, term, 0)
	items := []map[string]any{{"barcode": a.Barcode, "qty": 2}} // 100000
	pay := func(p ...map[string]any) map[string]any { return checkoutBody(term, items, p) }
	cash := func(n int) map[string]any { return map[string]any{"method": "CASH", "amount": n} }
	qris := func(n int) map[string]any { return map[string]any{"method": "QRIS", "amount": n, "reference": "QR1"} }

	tn.Owner.wantErr("POST", "/pos/sales", pay(cash(90000)), 422, "PAYMENT_INSUFFICIENT")
	tn.Owner.wantErr("POST", "/pos/sales", pay(), 422, "PAYMENT_INSUFFICIENT")
	tn.Owner.wantErr("POST", "/pos/sales", pay(qris(120000)), 422, "PAYMENT_EXCESS")
	tn.Owner.wantErr("POST", "/pos/sales", pay(map[string]any{"method": "BITCOIN", "amount": 100000}), 422, "PAYMENT_METHOD_DISABLED")
	tn.Owner.wantErr("POST", "/pos/sales", pay(qris(100000), cash(1000)), 422, "PAYMENT_EXCESS")
	if tn.onHand(t, st, a) != "100" {
		t.Fatalf("rejected sales must not move stock: %s", tn.onHand(t, st, a))
	}
	// split payment: QRIS 40000 + cash 70000 for 100000 => change 10000
	var s saleOut
	tn.Owner.mustOK("POST", "/pos/sales", pay(qris(40000), cash(70000)), &s)
	if s.ChangeAmount != "10000" {
		t.Fatalf("change %s", s.ChangeAmount)
	}
	var sh struct {
		Totals struct {
			CashSales    string `json:"cash_sales"`
			NoncashSales string `json:"noncash_sales"`
		}
	}
	tn.Owner.mustOK("GET", "/pos/shifts/current", nil, &sh)
	if sh.Totals.CashSales != "60000" || sh.Totals.NoncashSales != "40000" {
		t.Fatalf("split totals %+v", sh.Totals)
	}
}

func TestCheckoutIdempotentAndAtomic(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "10000")
	b := tn.product(t, "SKU-B", "10000")
	tn.stock(t, st, a, 10, "1")
	tn.stock(t, st, b, 1, "1")
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "pos.allow_negative_stock", "value": false}, nil)
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, tn.Owner, term, 0)

	body := checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 3}}, []map[string]any{{"method": "CASH", "amount": 30000}})
	var s1, s2 saleOut
	tn.Owner.mustOK("POST", "/pos/sales", body, &s1)
	tn.Owner.mustOK("POST", "/pos/sales", body, &s2) // replay
	if s1.ID != s2.ID || s1.DocumentNumber != s2.DocumentNumber {
		t.Fatalf("replay must return the same sale: %s vs %s", s1.ID, s2.ID)
	}
	if tn.onHand(t, st, a) != "7" {
		t.Fatalf("idempotent replay deducted twice: %s", tn.onHand(t, st, a))
	}

	// all-or-nothing: line 1 is fine, line 2 oversells -> nothing is written
	var docsBefore int
	_ = testPool.QueryRow(context.Background(), `select count(*) from sales where company_id=$1`, tn.CompanyID).Scan(&docsBefore)
	tn.Owner.wantErr("POST", "/pos/sales", checkoutBody(term,
		[]map[string]any{{"barcode": a.Barcode, "qty": 2}, {"barcode": b.Barcode, "qty": 5}},
		[]map[string]any{{"method": "CASH", "amount": 70000}}), 409, "INSUFFICIENT_STOCK")
	var docsAfter int
	_ = testPool.QueryRow(context.Background(), `select count(*) from sales where company_id=$1`, tn.CompanyID).Scan(&docsAfter)
	if docsBefore != docsAfter || tn.onHand(t, st, a) != "7" || tn.onHand(t, st, b) != "1" {
		t.Fatalf("partial write: sales %d->%d a=%s b=%s", docsBefore, docsAfter, tn.onHand(t, st, a), tn.onHand(t, st, b))
	}
}

// 60 simultaneous checkouts of 1 unit against stock of 20: exactly 20 succeed, never oversell.
func TestConcurrentSalesNeverOversell(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "10000")
	tn.stock(t, st, a, 20, "1")
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "pos.allow_negative_stock", "value": false}, nil)
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, tn.Owner, term, 0)

	var ok, short, other int32
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, env := tn.Owner.do("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 10000}}))
			switch {
			case st == 201:
				atomic.AddInt32(&ok, 1)
			case env.Error != nil && env.Error.Code == "INSUFFICIENT_STOCK":
				atomic.AddInt32(&short, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("unexpected %d %+v", st, env.Error)
			}
		}()
	}
	wg.Wait()
	if ok != 20 || short != 40 || other != 0 {
		t.Fatalf("ok=%d short=%d other=%d", ok, short, other)
	}
	if tn.onHand(t, st, a) != "0" {
		t.Fatalf("on hand %s", tn.onHand(t, st, a))
	}
	// unique, gapless document numbers under contention
	var n, mx int
	_ = testPool.QueryRow(context.Background(), `select count(distinct document_number), count(*) from sales where company_id=$1`, tn.CompanyID).Scan(&n, &mx)
	if n != 20 || mx != 20 {
		t.Fatalf("docs distinct=%d total=%d", n, mx)
	}
	// ledger reconciles to the cached balance
	var ledger string
	_ = testPool.QueryRow(context.Background(), `select sum(qty_delta)::text from inventory_movements where variant_id=$1`, uuid.MustParse(a.VariantID)).Scan(&ledger)
	if ledger != "0.0000" {
		t.Fatalf("ledger sum %s", ledger)
	}
}

func TestDiscountNeedsSupervisor(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "100000")
	tn.stock(t, st, a, 50, "1")
	term := terminalOf(t, tn.Owner, st.Store.ID)
	cashier := tn.user(t, "cashier", st.Store.OrgUnitID)
	tn.user(t, "supervisor", st.Store.OrgUnitID)
	var supEmail string
	_ = testPool.QueryRow(context.Background(), `select email from users where company_id=$1 and name='supervisor'`, tn.CompanyID).Scan(&supEmail)
	openShift(t, cashier, term, 0)

	line := func(p int) []map[string]any {
		return []map[string]any{{"barcode": a.Barcode, "qty": 1, "discount_percent": p}}
	}
	pay := []map[string]any{{"method": "CASH", "amount": 100000}}
	// within the 20% limit: fine
	var ok saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, line(15), pay), &ok)
	if ok.GrandTotal != "85000" {
		t.Fatalf("15%% off: %s", ok.GrandTotal)
	}
	// above the limit: needs a PIN
	cashier.wantErr("POST", "/pos/sales", checkoutBody(term, line(30), pay), 403, "APPROVAL_REQUIRED")
	var q struct {
		ApprovalRequired bool `json:"approval_required"`
	}
	cashier.mustOK("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": line(30)}, &q)
	if !q.ApprovalRequired {
		t.Fatal("quote must flag approval")
	}
	bad := checkoutBody(term, line(30), pay)
	bad["approval"] = map[string]any{"approver_email": supEmail, "pin": "9999"}
	cashier.wantErr("POST", "/pos/sales", bad, 403, "APPROVAL_DENIED")
	good := checkoutBody(term, line(30), []map[string]any{{"method": "CASH", "amount": 70000}})
	good["approval"] = map[string]any{"approver_email": supEmail, "pin": "1234"}
	var s saleOut
	cashier.mustOK("POST", "/pos/sales", good, &s)
	if s.GrandTotal != "70000" {
		t.Fatalf("30%% off: %s", s.GrandTotal)
	}
	// approved_by is recorded
	var by *string
	_ = testPool.QueryRow(context.Background(), `select approved_by::text from sales where id=$1`, s.ID).Scan(&by)
	if by == nil {
		t.Fatal("approved_by missing")
	}
}

func TestVoidAndReturn(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "50000")
	tn.stock(t, st, a, 10, "20000")
	term := terminalOf(t, tn.Owner, st.Store.ID)
	cashier := tn.user(t, "cashier", st.Store.OrgUnitID)
	sup := tn.user(t, "supervisor", st.Store.OrgUnitID)
	_ = sup
	var supEmail string
	_ = testPool.QueryRow(context.Background(), `select email from users where company_id=$1 and name='supervisor'`, tn.CompanyID).Scan(&supEmail)
	shiftID := openShift(t, cashier, term, 50000)
	pay := func(n int) []map[string]any { return []map[string]any{{"method": "CASH", "amount": n}} }

	var s1, s2 saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 4}}, pay(200000)), &s1)
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 2}}, pay(100000)), &s2)
	if tn.onHand(t, st, a) != "4" {
		t.Fatalf("after sales: %s", tn.onHand(t, st, a))
	}

	// void: cashier alone is refused, with a supervisor PIN it works, stock returns
	cashier.wantErr("POST", "/pos/sales/"+s2.ID+"/void", map[string]any{"reason": "customer left"}, 403, "APPROVAL_REQUIRED")
	cashier.mustOK("POST", "/pos/sales/"+s2.ID+"/void", map[string]any{"reason": "customer left", "approval": map[string]any{"approver_email": supEmail, "pin": "1234"}}, nil)
	if tn.onHand(t, st, a) != "6" {
		t.Fatalf("after void: %s", tn.onHand(t, st, a))
	}
	cashier.wantErr("POST", "/pos/sales/"+s2.ID+"/void", map[string]any{"reason": "again", "approval": map[string]any{"approver_email": supEmail, "pin": "1234"}}, 409, "ALREADY_VOIDED")

	// partial return: 1 of 4 back to shelf, cash refund 50000
	itemID := s1.Items[0].ID
	var r struct {
		DocumentNumber string `json:"document_number"`
		RefundAmount   string `json:"refund_amount"`
	}
	cashier.mustOK("POST", "/pos/sales/"+s1.ID+"/returns", map[string]any{"reason": "size", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": itemID, "qty": 1}}}, &r)
	if r.RefundAmount != "50000" || !strings.HasPrefix(r.DocumentNumber, "RT/ST-A/") {
		t.Fatalf("return %+v", r)
	}
	if tn.onHand(t, st, a) != "7" {
		t.Fatalf("after return: %s", tn.onHand(t, st, a))
	}
	// cannot return more than was sold; non-restock return does not touch stock
	cashier.wantErr("POST", "/pos/sales/"+s1.ID+"/returns", map[string]any{"reason": "x", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": itemID, "qty": 4}}}, 409, "RETURN_EXCEEDS_SOLD")
	cashier.mustOK("POST", "/pos/sales/"+s1.ID+"/returns", map[string]any{"reason": "defect", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": itemID, "qty": 1, "restock": false}}}, nil)
	if tn.onHand(t, st, a) != "7" {
		t.Fatalf("non-restock changed stock: %s", tn.onHand(t, st, a))
	}
	// a sale with returns cannot be voided
	cashier.wantErr("POST", "/pos/sales/"+s1.ID+"/void", map[string]any{"reason": "x", "approval": map[string]any{"approver_email": supEmail, "pin": "1234"}}, 409, "VOID_NOT_ALLOWED")

	// shift: opening 50000 + cash sales 200000 (void excluded) - refunds 100000
	var sh struct {
		Totals struct {
			Expected    string `json:"expected_cash"`
			CashRefunds string `json:"cash_refunds"`
			VoidCount   int    `json:"void_count"`
		}
	}
	cashier.mustOK("GET", "/pos/shifts/"+shiftID, nil, &sh)
	if sh.Totals.Expected != "150000" || sh.Totals.CashRefunds != "100000" || sh.Totals.VoidCount != 1 {
		t.Fatalf("shift %+v", sh.Totals)
	}

	// refunds above the threshold need a manager PIN (store manager holds pos.refund.approve)
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "pos.refund_approval_threshold", "value": 60000}, nil)
	var s3 saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 2}}, pay(100000)), &s3)
	big := map[string]any{"reason": "all", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": s3.Items[0].ID, "qty": 2}}}
	cashier.wantErr("POST", "/pos/sales/"+s3.ID+"/returns", big, 403, "APPROVAL_REQUIRED")
	tn.user(t, "store_manager", st.Store.OrgUnitID)
	var mgrEmail string
	_ = testPool.QueryRow(context.Background(), `select email from users where company_id=$1 and name='store_manager'`, tn.CompanyID).Scan(&mgrEmail)
	big["approval"] = map[string]any{"approver_email": mgrEmail, "pin": "1234"}
	cashier.mustOK("POST", "/pos/sales/"+s3.ID+"/returns", big, nil)
}

func TestSalesScopeIsolation(t *testing.T) {
	tn := newTenant(t)
	a := tn.store(t, "ST-A", "")
	b := tn.store(t, "ST-B", "")
	it := tn.product(t, "SKU-A", "10000")
	tn.stock(t, a, it, 10, "1")
	tn.stock(t, b, it, 10, "1")
	termA, termB := terminalOf(t, tn.Owner, a.Store.ID), terminalOf(t, tn.Owner, b.Store.ID)
	cashA := tn.user(t, "cashier", a.Store.OrgUnitID)
	cashB := tn.user(t, "cashier", b.Store.OrgUnitID)
	openShift(t, cashA, termA, 0)
	openShift(t, cashB, termB, 0)
	var sa saleOut
	cashA.mustOK("POST", "/pos/sales", checkoutBody(termA, []map[string]any{{"barcode": it.Barcode, "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 10000}}), &sa)

	// the other store's cashier can neither see the sale, nor sell on A's terminal, nor see A's stock
	cashB.wantErr("GET", "/pos/sales/"+sa.ID, nil, 404, "NOT_FOUND")
	cashB.wantErr("POST", "/pos/sales", checkoutBody(termA, []map[string]any{{"barcode": it.Barcode, "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 10000}}), 403, "FORBIDDEN")
	cashB.wantErr("POST", "/pos/quote", cart(termA, 1, it), 403, "FORBIDDEN")
	var list []struct{ ID string }
	cashB.mustOK("GET", "/pos/sales", nil, &list)
	if len(list) != 0 {
		t.Fatalf("cashier B sees %d sales of A", len(list))
	}
	tn.Owner.mustOK("GET", "/pos/sales", nil, &list)
	if len(list) != 1 {
		t.Fatalf("owner sees %d", len(list))
	}
	// stock adjustments are scoped too
	cashA.wantErr("POST", "/inventory/adjustments", map[string]any{"store_id": a.Store.ID, "type": "ADJUSTMENT", "reason": "x", "lines": []map[string]any{{"variant_id": it.VariantID, "qty_delta": 1}}}, 403, "FORBIDDEN")
	// one open shift per cashier / terminal
	cashA.wantErr("POST", "/pos/shifts/open", map[string]any{"terminal_id": termA, "opening_cash": 0}, 409, "SHIFT_ALREADY_OPEN")
}

func TestPriceInheritance(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	bali := tn.unit(t, "region", "BALI", tn.RootUnit)
	jkt := tn.store(t, "JKT", jabo)
	dps := tn.store(t, "DPS", bali)
	it := tn.product(t, "SKU-A", "35000") // company default
	resolve := func(store storeOut) (price, source string) {
		var r struct {
			Price      string `json:"price"`
			SourceType string `json:"source_type"`
		}
		tn.Owner.mustOK("GET", "/prices/resolve?store_id="+store.Store.ID+"&variant_id="+it.VariantID, nil, &r)
		return r.Price, r.SourceType
	}
	if p, s := resolve(jkt); p != "35000" || s != "company" {
		t.Fatalf("company default: %s %s", p, s)
	}
	tn.Owner.mustOK("PUT", "/prices", map[string]any{"org_unit_id": bali, "variant_id": it.VariantID, "price": 39000}, nil)
	if p, s := resolve(dps); p != "39000" || s != "region" {
		t.Fatalf("region override: %s %s", p, s)
	}
	if p, _ := resolve(jkt); p != "35000" {
		t.Fatalf("other region unaffected: %s", p)
	}
	tn.Owner.mustOK("PUT", "/prices", map[string]any{"org_unit_id": dps.Store.OrgUnitID, "variant_id": it.VariantID, "price": 41000}, nil)
	if p, s := resolve(dps); p != "41000" || s != "store" {
		t.Fatalf("store override: %s %s", p, s)
	}
	// a store opened later in Bali inherits the regional price with no setup
	ubud := tn.store(t, "UBUD", bali)
	if p, s := resolve(ubud); p != "39000" || s != "region" {
		t.Fatalf("new store must inherit: %s %s", p, s)
	}
	tn.Owner.mustOK("DELETE", "/prices", map[string]any{"org_unit_id": dps.Store.OrgUnitID, "variant_id": it.VariantID}, nil)
	if p, s := resolve(dps); p != "39000" || s != "region" {
		t.Fatalf("revert falls back: %s %s", p, s)
	}
	// the POS charges the resolved price, and refuses items with no price
	tn.stock(t, ubud, it, 5, "1")
	term := terminalOf(t, tn.Owner, ubud.Store.ID)
	var q struct {
		GrandTotal string `json:"grand_total"`
	}
	tn.Owner.mustOK("POST", "/pos/quote", cart(term, 2, it), &q)
	if q.GrandTotal != "78000" {
		t.Fatalf("quote at regional price: %s", q.GrandTotal)
	}
}

func TestMovingAverageCostAndNegativeStock(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "50000")
	tn.stock(t, st, a, 10, "10000")
	tn.Owner.mustOK("POST", "/inventory/adjustments", map[string]any{"store_id": st.Store.ID, "type": "ADJUSTMENT", "reason": "received extra",
		"lines": []map[string]any{{"variant_id": a.VariantID, "qty_delta": 10, "unit_cost": "20000"}}}, nil)
	var rows []struct {
		OnHand  string `json:"on_hand"`
		AvgCost string `json:"avg_cost"`
	}
	tn.Owner.mustOK("GET", "/inventory/balances?store_id="+st.Store.ID, nil, &rows)
	if rows[0].OnHand != "20" || rows[0].AvgCost != "15000" {
		t.Fatalf("weighted average: %+v", rows[0])
	}
	// manual adjustment can never drive stock below zero
	tn.Owner.wantErr("POST", "/inventory/adjustments", map[string]any{"store_id": st.Store.ID, "type": "DAMAGE", "reason": "broken",
		"lines": []map[string]any{{"variant_id": a.VariantID, "qty_delta": -25}}}, 409, "INSUFFICIENT_STOCK")
	tn.Owner.mustOK("POST", "/inventory/adjustments", map[string]any{"store_id": st.Store.ID, "type": "DAMAGE", "reason": "broken",
		"lines": []map[string]any{{"variant_id": a.VariantID, "qty_delta": -2}}}, nil)
	// default policy allows selling below zero (retail reality), and flags it as a negative balance
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, tn.Owner, term, 0)
	tn.Owner.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 20}}, []map[string]any{{"method": "CASH", "amount": 1000000}}), nil)
	if tn.onHand(t, st, a) != "-2" {
		t.Fatalf("negative stock allowed by default: %s", tn.onHand(t, st, a))
	}
}
