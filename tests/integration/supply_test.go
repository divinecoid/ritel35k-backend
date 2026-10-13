package integration

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func (tn *tenant) supplier(t *testing.T) string {
	t.Helper()
	var s struct{ ID string }
	tn.Owner.mustOK("POST", "/suppliers", map[string]any{"code": strings.ToUpper(uniq("sup")), "name": "PT Supplier"}, &s)
	return s.ID
}

func (tn *tenant) centralWarehouse(t *testing.T) string {
	t.Helper()
	var w struct{ ID string }
	tn.Owner.mustOK("POST", "/warehouses", map[string]any{"code": strings.ToUpper(uniq("MAIN")), "name": "Gudang Pusat", "type": "main"}, &w)
	return w.ID
}

type poOut struct {
	ID             string `json:"id"`
	DocumentNumber string `json:"document_number"`
	Status         string `json:"status"`
	Total          string `json:"total"`
	Lines          []struct {
		ID          string `json:"id"`
		VariantID   string `json:"variant_id"`
		QtyReceived string `json:"qty_received"`
	} `json:"lines"`
}

func poBody(supplier string, wh string, lines ...map[string]any) map[string]any {
	return map[string]any{"supplier_id": supplier, "warehouse_id": wh, "lines": lines}
}

func line(variant string, qty int, price int) map[string]any {
	return map[string]any{"variant_id": variant, "qty": qty, "unit_price": price}
}

func (tn *tenant) onHandAt(t *testing.T, store storeOut, it item) string {
	return tn.onHand(t, store, it)
}

func inTransit(t *testing.T, wh string, variant string) string {
	t.Helper()
	var v string
	_ = testPool.QueryRow(context.Background(), `select in_transit::text from inventory_balances where warehouse_id=$1 and variant_id=$2`, uuid.MustParse(wh), uuid.MustParse(variant)).Scan(&v)
	return v
}

func TestPurchaseOrderToReceipt(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "50000")
	sup := tn.supplier(t)
	wh := st.Warehouse.ID

	var po poOut
	tn.Owner.mustOK("POST", "/purchase-orders", poBody(sup, wh, line(a.VariantID, 10, 10000)), &po)
	if po.Status != "draft" || po.Total != "100000" || !strings.HasPrefix(po.DocumentNumber, "PO/ST-A/") {
		t.Fatalf("po %+v", po)
	}
	// cannot receive a draft
	tn.Owner.wantErr("POST", "/goods-receipts", map[string]any{"po_id": po.ID, "lines": []map[string]any{{"variant_id": a.VariantID, "qty": 1}}}, 409, "PO_NOT_RECEIVABLE")
	// total (100000) <= default threshold (5,000,000) -> auto-approved on submit
	tn.Owner.mustOK("POST", "/purchase-orders/"+po.ID+"/submit", map[string]any{}, &po)
	if po.Status != "approved" {
		t.Fatalf("auto approve: %s", po.Status)
	}
	// approved POs are immutable
	tn.Owner.wantErr("PUT", "/purchase-orders/"+po.ID, poBody(sup, wh, line(a.VariantID, 1, 1)), 409, "PO_NOT_EDITABLE")

	// partial receipt: 6 of 10
	var g1 struct {
		ID             string `json:"id"`
		DocumentNumber string `json:"document_number"`
		TotalCost      string `json:"total_cost"`
	}
	tn.Owner.mustOK("POST", "/goods-receipts", map[string]any{"po_id": po.ID, "supplier_ref": "SJ-001", "lines": []map[string]any{{"variant_id": a.VariantID, "qty": 6}}}, &g1)
	if g1.TotalCost != "60000" || !strings.HasPrefix(g1.DocumentNumber, "GRN/ST-A/") {
		t.Fatalf("grn %+v", g1)
	}
	if tn.onHand(t, st, a) != "6" {
		t.Fatalf("stock after grn: %s", tn.onHand(t, st, a))
	}
	tn.Owner.mustOK("GET", "/purchase-orders/"+po.ID, nil, &po)
	if po.Status != "partially_received" || po.Lines[0].QtyReceived != "6" {
		t.Fatalf("po after partial: %+v", po)
	}
	// cannot over-receive
	tn.Owner.wantErr("POST", "/goods-receipts", map[string]any{"po_id": po.ID, "lines": []map[string]any{{"variant_id": a.VariantID, "qty": 5}}}, 409, "RECEIPT_EXCEEDS_ORDER")
	// receive the rest
	tn.Owner.mustOK("POST", "/goods-receipts", map[string]any{"po_id": po.ID, "lines": []map[string]any{{"variant_id": a.VariantID, "qty": 4}}}, nil)
	tn.Owner.mustOK("GET", "/purchase-orders/"+po.ID, nil, &po)
	if po.Status != "received" || tn.onHand(t, st, a) != "10" {
		t.Fatalf("po %s stock %s", po.Status, tn.onHand(t, st, a))
	}
	tn.Owner.wantErr("POST", "/goods-receipts", map[string]any{"po_id": po.ID, "lines": []map[string]any{{"variant_id": a.VariantID, "qty": 1}}}, 409, "PO_NOT_RECEIVABLE")

	// moving average: direct receipt of 10 more at 20000 -> (10*10000 + 10*20000)/20 = 15000
	tn.Owner.mustOK("POST", "/goods-receipts", map[string]any{"warehouse_id": wh, "supplier_id": sup, "lines": []map[string]any{{"variant_id": a.VariantID, "qty": 10, "unit_cost": 20000}}}, nil)
	var bal []struct {
		OnHand  string `json:"on_hand"`
		AvgCost string `json:"avg_cost"`
	}
	tn.Owner.mustOK("GET", "/inventory/balances?store_id="+st.Store.ID, nil, &bal)
	if bal[0].OnHand != "20" || bal[0].AvgCost != "15000" {
		t.Fatalf("avg cost: %+v", bal[0])
	}
	// ledger reconciles with the cached balance
	var sum string
	_ = testPool.QueryRow(context.Background(), `select sum(qty_delta)::text from inventory_movements where variant_id=$1`, uuid.MustParse(a.VariantID)).Scan(&sum)
	if sum != "20.0000" {
		t.Fatalf("ledger %s", sum)
	}
}

func TestPurchaseOrderApprovalAndScope(t *testing.T) {
	tn := newTenant(t)
	a := tn.store(t, "ST-A", "")
	b := tn.store(t, "ST-B", "")
	it := tn.product(t, "SKU-A", "50000")
	sup := tn.supplier(t)
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "purchasing.po_approval_threshold", "value": 1000}, nil)

	mgrA := tn.user(t, "store_manager", a.Store.OrgUnitID)
	mgrB := tn.user(t, "store_manager", b.Store.OrgUnitID)
	cashier := tn.user(t, "cashier", a.Store.OrgUnitID)
	buyer := tn.user(t, "purchasing", "") // company-wide purchasing

	cashier.wantErr("POST", "/purchase-orders", poBody(sup, a.Warehouse.ID, line(it.VariantID, 5, 10000)), 403, "FORBIDDEN")
	mgrA.wantErr("POST", "/purchase-orders", poBody(sup, b.Warehouse.ID, line(it.VariantID, 5, 10000)), 403, "FORBIDDEN") // other store's warehouse

	var po poOut
	mgrA.mustOK("POST", "/purchase-orders", poBody(sup, a.Warehouse.ID, line(it.VariantID, 5, 10000)), &po)
	mgrA.mustOK("POST", "/purchase-orders/"+po.ID+"/submit", map[string]any{}, &po)
	if po.Status != "submitted" { // 50000 > threshold 1000
		t.Fatalf("must wait for approval: %s", po.Status)
	}
	mgrA.wantErr("POST", "/purchase-orders/"+po.ID+"/approve", map[string]any{}, 403, "FORBIDDEN") // store manager lacks approve
	tn.Owner.wantErr("POST", "/purchase-orders/"+po.ID+"/receive", nil, 404, "")
	mgrA.wantErr("POST", "/goods-receipts", map[string]any{"po_id": po.ID, "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 1}}}, 409, "PO_NOT_RECEIVABLE")

	// scope: manager B cannot even see A's PO
	mgrB.wantErr("GET", "/purchase-orders/"+po.ID, nil, 404, "NOT_FOUND")
	var list []struct{ ID string }
	mgrB.mustOK("GET", "/purchase-orders", nil, &list)
	if len(list) != 0 {
		t.Fatalf("B sees %d POs of A", len(list))
	}
	buyer.mustOK("GET", "/purchase-orders", nil, &list)
	if len(list) != 1 {
		t.Fatalf("buyer sees %d", len(list))
	}

	// a creator holding approve permission still cannot approve their own PO
	var own poOut
	buyer.mustOK("POST", "/purchase-orders", poBody(sup, a.Warehouse.ID, line(it.VariantID, 5, 10000)), &own)
	buyer.mustOK("POST", "/purchase-orders/"+own.ID+"/submit", map[string]any{}, &own)
	buyer.wantErr("POST", "/purchase-orders/"+own.ID+"/approve", map[string]any{}, 409, "SELF_APPROVAL_FORBIDDEN")
	buyer.mustOK("POST", "/purchase-orders/"+po.ID+"/approve", map[string]any{}, &po) // someone else's PO is fine
	if po.Status != "approved" {
		t.Fatalf("approved: %s", po.Status)
	}
	tn.Owner.mustOK("POST", "/purchase-orders/"+own.ID+"/approve", map[string]any{}, nil)
	// cancel rules
	tn.Owner.mustOK("POST", "/purchase-orders/"+po.ID+"/cancel", map[string]any{"reason": "supplier out of stock"}, &po)
	if po.Status != "cancelled" {
		t.Fatalf("cancel: %s", po.Status)
	}
	tn.Owner.wantErr("POST", "/purchase-orders/"+po.ID+"/cancel", map[string]any{"reason": "again"}, 409, "PO_BAD_STATE")
}

func TestPurchaseReturn(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	it := tn.product(t, "SKU-A", "50000")
	sup := tn.supplier(t)
	var g struct {
		ID string `json:"id"`
	}
	tn.Owner.mustOK("POST", "/goods-receipts", map[string]any{"warehouse_id": st.Warehouse.ID, "supplier_id": sup, "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 10, "unit_cost": 8000}}}, &g)

	var r struct {
		DocumentNumber string `json:"document_number"`
		TotalCost      string `json:"total_cost"`
	}
	tn.Owner.mustOK("POST", "/purchase-returns", map[string]any{"grn_id": g.ID, "reason": "damaged", "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 4}}}, &r)
	if r.TotalCost != "32000" || !strings.HasPrefix(r.DocumentNumber, "PRT/ST-A/") || tn.onHand(t, st, it) != "6" {
		t.Fatalf("return %+v stock %s", r, tn.onHand(t, st, it))
	}
	tn.Owner.wantErr("POST", "/purchase-returns", map[string]any{"grn_id": g.ID, "reason": "more", "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 7}}}, 409, "RETURN_EXCEEDS_RECEIVED")
	tn.Owner.mustOK("POST", "/purchase-returns", map[string]any{"grn_id": g.ID, "reason": "rest", "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 6}}}, nil)
	if tn.onHand(t, st, it) != "0" {
		t.Fatalf("stock %s", tn.onHand(t, st, it))
	}
	// direct return can never take stock below zero
	tn.Owner.wantErr("POST", "/purchase-returns", map[string]any{"warehouse_id": st.Warehouse.ID, "supplier_id": sup, "reason": "x", "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 1}}}, 409, "INSUFFICIENT_STOCK")
}

type trOut struct {
	ID             string `json:"id"`
	DocumentNumber string `json:"document_number"`
	Status         string `json:"status"`
	Lines          []struct {
		QtyShipped  string `json:"qty_shipped"`
		QtyReceived string `json:"qty_received"`
		UnitCost    string `json:"unit_cost"`
	} `json:"lines"`
}

func TestStockTransferCentralToStore(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	other := tn.store(t, "ST-C", "")
	it := tn.product(t, "SKU-A", "50000")
	sup := tn.supplier(t)
	main := tn.centralWarehouse(t)
	tn.Owner.mustOK("POST", "/goods-receipts", map[string]any{"warehouse_id": main, "supplier_id": sup, "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 20, "unit_cost": 12000}}}, nil)

	mgr := tn.user(t, "store_manager", st.Store.OrgUnitID)
	mgrC := tn.user(t, "store_manager", other.Store.OrgUnitID)

	var tr trOut
	tn.Owner.mustOK("POST", "/stock-transfers", map[string]any{"from_warehouse_id": main, "to_warehouse_id": st.Warehouse.ID, "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 10}}}, &tr)
	if tr.Status != "draft" || !strings.HasPrefix(tr.DocumentNumber, "TRF/MAIN") {
		t.Fatalf("transfer %+v", tr)
	}
	// state machine: cannot ship a draft; store manager cannot approve a transfer FROM the central warehouse
	tn.Owner.wantErr("POST", "/stock-transfers/"+tr.ID+"/ship", map[string]any{}, 409, "TRANSFER_BAD_STATE")
	tn.Owner.mustOK("POST", "/stock-transfers/"+tr.ID+"/submit", map[string]any{}, nil)
	mgr.wantErr("POST", "/stock-transfers/"+tr.ID+"/approve", map[string]any{}, 403, "FORBIDDEN")
	tn.Owner.mustOK("POST", "/stock-transfers/"+tr.ID+"/approve", map[string]any{}, nil)

	// ship: origin down, destination in-transit up (on_hand untouched)
	tn.Owner.mustOK("POST", "/stock-transfers/"+tr.ID+"/ship", map[string]any{}, &tr)
	if tr.Status != "shipped" || tr.Lines[0].QtyShipped != "10" || tr.Lines[0].UnitCost != "12000" {
		t.Fatalf("shipped %+v", tr)
	}
	if inTransit(t, st.Warehouse.ID, it.VariantID) != "10.0000" || tn.onHand(t, st, it) != "0" {
		t.Fatalf("in transit=%s on_hand=%s", inTransit(t, st.Warehouse.ID, it.VariantID), tn.onHand(t, st, it))
	}
	// only people with authority at the destination can receive; unrelated stores can't even see it
	mgrC.wantErr("GET", "/stock-transfers/"+tr.ID, nil, 404, "NOT_FOUND")
	mgrC.wantErr("POST", "/stock-transfers/"+tr.ID+"/receive", map[string]any{}, 404, "")
	// receive 8 of 10: shortage is recorded, cost carried over
	mgr.mustOK("POST", "/stock-transfers/"+tr.ID+"/receive", map[string]any{"lines": []map[string]any{{"variant_id": it.VariantID, "qty": 8}}}, &tr)
	if tr.Status != "received" || tr.Lines[0].QtyReceived != "8" {
		t.Fatalf("received %+v", tr)
	}
	if inTransit(t, st.Warehouse.ID, it.VariantID) != "0.0000" || tn.onHand(t, st, it) != "8" {
		t.Fatalf("after receive: in_transit=%s on_hand=%s", inTransit(t, st.Warehouse.ID, it.VariantID), tn.onHand(t, st, it))
	}
	var avg string
	_ = testPool.QueryRow(context.Background(), `select avg_cost::text from inventory_balances where warehouse_id=$1 and variant_id=$2`, uuid.MustParse(st.Warehouse.ID), uuid.MustParse(it.VariantID)).Scan(&avg)
	if avg != "12000.0000" {
		t.Fatalf("cost must follow the goods: %s", avg)
	}
	var shortage int
	_ = testPool.QueryRow(context.Background(), `select count(*) from audit_logs where action='TRANSFER_SHORTAGE' and company_id=$1`, uuid.MustParse(tn.CompanyID)).Scan(&shortage)
	if shortage != 1 {
		t.Fatalf("shortage audit rows: %d", shortage)
	}
	tn.Owner.wantErr("POST", "/stock-transfers/"+tr.ID+"/receive", map[string]any{}, 409, "TRANSFER_BAD_STATE")
	tn.Owner.wantErr("POST", "/stock-transfers/"+tr.ID+"/cancel", map[string]any{"reason": "x"}, 409, "TRANSFER_BAD_STATE")
}

func TestTransferShipIsAtomicAndStoreToStore(t *testing.T) {
	tn := newTenant(t)
	a := tn.store(t, "ST-A", "")
	b := tn.store(t, "ST-B", "")
	x := tn.product(t, "SKU-X", "10000")
	y := tn.product(t, "SKU-Y", "10000")
	tn.stock(t, a, x, 10, "100")
	tn.stock(t, a, y, 1, "100")
	mgrA := tn.user(t, "store_manager", a.Store.OrgUnitID)
	mgrB := tn.user(t, "store_manager", b.Store.OrgUnitID)

	var tr trOut
	mgrA.mustOK("POST", "/stock-transfers", map[string]any{"from_warehouse_id": a.Warehouse.ID, "to_warehouse_id": b.Warehouse.ID,
		"lines": []map[string]any{{"variant_id": x.VariantID, "qty": 5}, {"variant_id": y.VariantID, "qty": 3}}}, &tr)
	mgrA.mustOK("POST", "/stock-transfers/"+tr.ID+"/submit", map[string]any{}, nil)
	mgrA.mustOK("POST", "/stock-transfers/"+tr.ID+"/approve", map[string]any{}, nil)
	// y is short (1 on hand, 3 requested): the whole shipment fails and x must be untouched
	mgrA.wantErr("POST", "/stock-transfers/"+tr.ID+"/ship", map[string]any{}, 409, "INSUFFICIENT_STOCK")
	if tn.onHand(t, a, x) != "10" || inTransit(t, b.Warehouse.ID, x.VariantID) != "" {
		t.Fatalf("partial ship leaked: x=%s transit=%q", tn.onHand(t, a, x), inTransit(t, b.Warehouse.ID, x.VariantID))
	}
	// ship what exists
	mgrA.mustOK("POST", "/stock-transfers/"+tr.ID+"/ship", map[string]any{"lines": []map[string]any{{"variant_id": y.VariantID, "qty": 1}}}, nil)
	mgrB.mustOK("POST", "/stock-transfers/"+tr.ID+"/receive", map[string]any{}, nil)
	if tn.onHand(t, b, y) != "1" || tn.onHand(t, a, y) != "0" {
		t.Fatalf("y: A=%s B=%s", tn.onHand(t, a, y), tn.onHand(t, b, y))
	}
}

func TestStockCountWithSalesDuringCount(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "10000")
	b := tn.product(t, "SKU-B", "10000")
	c := tn.product(t, "SKU-C", "10000")
	tn.stock(t, st, a, 10, "1000")
	tn.stock(t, st, b, 5, "1000")
	tn.stock(t, st, c, 3, "1000")
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, tn.Owner, term, 0)

	var cnt struct {
		ID             string `json:"id"`
		DocumentNumber string `json:"document_number"`
		Status         string `json:"status"`
		Summary        struct {
			Items int `json:"items"`
		} `json:"summary"`
	}
	tn.Owner.mustOK("POST", "/stock-counts", map[string]any{"warehouse_id": st.Warehouse.ID, "kind": "full"}, &cnt)
	if cnt.Summary.Items != 3 || cnt.Status != "counting" || !strings.HasPrefix(cnt.DocumentNumber, "OPN/ST-A/") {
		t.Fatalf("count %+v", cnt)
	}
	// the shop keeps selling while shelves are counted: 2 x A sold AFTER the snapshot
	tn.Owner.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": a.Barcode, "qty": 2}}, []map[string]any{{"method": "CASH", "amount": 20000}}), nil)

	// physical: A=8 (10-2 sold), B=4 (one really missing), C not counted
	tn.Owner.mustOK("PUT", "/stock-counts/"+cnt.ID+"/items", map[string]any{"items": []map[string]any{{"variant_id": a.VariantID, "counted_qty": 8}}}, nil)
	var scanned struct {
		CountedQty string `json:"counted_qty"`
	}
	for i := 0; i < 4; i++ { // barcode scanning accumulates
		tn.Owner.mustOK("POST", "/stock-counts/"+cnt.ID+"/scan", map[string]any{"barcode": b.Barcode}, &scanned)
	}
	if scanned.CountedQty != "4" {
		t.Fatalf("scan accumulate: %s", scanned.CountedQty)
	}
	tn.Owner.wantErr("POST", "/stock-counts/"+cnt.ID+"/scan", map[string]any{"barcode": "0000000000000"}, 404, "UNKNOWN_BARCODE")
	// uncounted items must be handled explicitly
	tn.Owner.wantErr("POST", "/stock-counts/"+cnt.ID+"/submit", map[string]any{}, 409, "UNCOUNTED_ITEMS")

	var sub struct {
		Summary struct {
			Shortage      string `json:"shortage_qty"`
			VarianceValue string `json:"variance_value"`
		} `json:"summary"`
		Lines []struct {
			SKU      string  `json:"sku"`
			Expected *string `json:"expected_qty"`
			Variance *string `json:"variance"`
		} `json:"lines"`
	}
	tn.Owner.mustOK("POST", "/stock-counts/"+cnt.ID+"/submit", map[string]any{"uncounted": "ignore"}, &sub)
	got := map[string]string{}
	for _, l := range sub.Lines {
		if l.Variance != nil {
			got[l.SKU] = *l.Variance
		} else {
			got[l.SKU] = "-"
		}
	}
	// A: expected 10-2=8, counted 8 => 0 (a naive snapshot diff would have flagged -2). B: 4 vs 5 => -1. C ignored.
	if got["SKU-A"] != "0" || got["SKU-B"] != "-1" || got["SKU-C"] != "-" {
		t.Fatalf("variances: %v", got)
	}
	if sub.Summary.Shortage != "1" || sub.Summary.VarianceValue != "-1000" {
		t.Fatalf("summary %+v", sub.Summary)
	}

	// approval: the person who submitted cannot approve a large variance
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "inventory.count_self_approve_limit", "value": 500}, nil)
	tn.Owner.wantErr("POST", "/stock-counts/"+cnt.ID+"/approve", map[string]any{}, 409, "SELF_APPROVAL_FORBIDDEN")
	mgr := tn.user(t, "store_manager", st.Store.OrgUnitID)
	mgr.mustOK("POST", "/stock-counts/"+cnt.ID+"/approve", map[string]any{}, nil)
	if tn.onHand(t, st, a) != "8" || tn.onHand(t, st, b) != "4" || tn.onHand(t, st, c) != "3" {
		t.Fatalf("after approval: a=%s b=%s c=%s", tn.onHand(t, st, a), tn.onHand(t, st, b), tn.onHand(t, st, c))
	}
	// the adjustment is an OPNAME ledger entry (-1 on B), nothing for A or C
	var n int
	_ = testPool.QueryRow(context.Background(), `select count(*) from inventory_movements where movement_type='OPNAME' and company_id=$1`, uuid.MustParse(tn.CompanyID)).Scan(&n)
	if n != 1 {
		t.Fatalf("opname movements: %d", n)
	}
	tn.Owner.wantErr("POST", "/stock-counts/"+cnt.ID+"/approve", map[string]any{}, 409, "COUNT_BAD_STATE")
	tn.Owner.wantErr("PUT", "/stock-counts/"+cnt.ID+"/items", map[string]any{"items": []map[string]any{{"variant_id": a.VariantID, "counted_qty": 1}}}, 409, "COUNT_NOT_OPEN")
}

func TestPartialCountAndZeroPolicy(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	a := tn.product(t, "SKU-A", "10000")
	b := tn.product(t, "SKU-B", "10000")
	tn.stock(t, st, a, 10, "1000")
	tn.stock(t, st, b, 5, "1000")
	var cnt struct {
		ID string `json:"id"`
	}
	tn.Owner.wantErr("POST", "/stock-counts", map[string]any{"warehouse_id": st.Warehouse.ID, "kind": "partial"}, 422, "VALIDATION_FAILED")
	tn.Owner.mustOK("POST", "/stock-counts", map[string]any{"warehouse_id": st.Warehouse.ID, "kind": "cycle", "variant_ids": []string{a.VariantID}}, &cnt)
	tn.Owner.wantErr("POST", "/stock-counts/"+cnt.ID+"/scan", map[string]any{"barcode": b.Barcode}, 409, "NOT_IN_COUNT")
	// uncounted + zero policy = the shelf is empty
	tn.Owner.mustOK("POST", "/stock-counts/"+cnt.ID+"/submit", map[string]any{"uncounted": "zero"}, nil)
	tn.Owner.mustOK("POST", "/stock-counts/"+cnt.ID+"/approve", map[string]any{}, nil)
	if tn.onHand(t, st, a) != "0" || tn.onHand(t, st, b) != "5" {
		t.Fatalf("a=%s b=%s", tn.onHand(t, st, a), tn.onHand(t, st, b))
	}
}

// Five receipts of 4 against a PO of 10, fired at once: the PO line is locked, so exactly two fit (8 <= 10 < 12).
func TestConcurrentReceiptsCannotOverReceive(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "ST-A", "")
	it := tn.product(t, "SKU-A", "50000")
	sup := tn.supplier(t)
	var po poOut
	tn.Owner.mustOK("POST", "/purchase-orders", poBody(sup, st.Warehouse.ID, line(it.VariantID, 10, 1000)), &po)
	tn.Owner.mustOK("POST", "/purchase-orders/"+po.ID+"/submit", map[string]any{}, nil)

	var wg sync.WaitGroup
	var okN, exceeded, other int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, env := tn.Owner.do("POST", "/goods-receipts", map[string]any{"po_id": po.ID, "lines": []map[string]any{{"variant_id": it.VariantID, "qty": 4}}})
			switch {
			case s == 201:
				atomic.AddInt32(&okN, 1)
			case env.Error != nil && env.Error.Code == "RECEIPT_EXCEEDS_ORDER":
				atomic.AddInt32(&exceeded, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("unexpected %d %+v", s, env.Error)
			}
		}()
	}
	wg.Wait()
	if okN != 2 || exceeded != 3 || other != 0 {
		t.Fatalf("ok=%d exceeded=%d other=%d", okN, exceeded, other)
	}
	if tn.onHand(t, st, it) != "8" {
		t.Fatalf("stock %s", tn.onHand(t, st, it))
	}
}
