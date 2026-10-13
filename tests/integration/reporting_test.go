package integration

import (
	"bufio"
	"context"
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/notification"
	"github.com/ritel35k/backend/internal/summary"
)

// settle runs the outbox relay and the summary rebuild to completion, like the worker does continuously.
func settle(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		n, err := testApp.Relay().RunOnce(ctx, 500)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if _, err := summary.Drain(ctx, testApp.Tx); err != nil {
		t.Fatal(err)
	}
}

type totalsOut struct {
	Transactions int64  `json:"transactions"`
	Voided       int64  `json:"voided"`
	ItemsSold    string `json:"items_sold"`
	Refund       string `json:"refund"`
	NetSales     string `json:"net_sales"`
	GrossProfit  string `json:"gross_profit"`
}

type overviewOut struct {
	Current    totalsOut `json:"current"`
	Previous   totalsOut `json:"previous"`
	ShowProfit bool      `json:"show_profit"`
	Trend      []struct {
		Date         string `json:"date"`
		Transactions int64  `json:"transactions"`
		NetSales     string `json:"net_sales"`
	} `json:"trend"`
	Hourly []struct {
		Transactions int64 `json:"transactions"`
	} `json:"hourly"`
	TopProducts []struct {
		SKU      string `json:"sku"`
		Qty      string `json:"qty"`
		NetSales string `json:"net_sales"`
	} `json:"top_products"`
	Payments []struct {
		Method string `json:"method"`
		Amount string `json:"amount"`
	} `json:"payments"`
	Breakdown []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Path     string `json:"path"`
		Stores   int64  `json:"stores"`
		NetSales string `json:"net_sales"`
	} `json:"breakdown"`
	BreakdownLevel string `json:"breakdown_level"`
	OpenShifts     *int64 `json:"open_shifts"`
}

func dayRange() string {
	n := time.Now().UTC()
	return "from=" + n.AddDate(0, 0, -2).Format("2006-01-02") + "&to=" + n.AddDate(0, 0, 1).Format("2006-01-02")
}

func (c *client) overview(t *testing.T, extra string) overviewOut {
	t.Helper()
	var o overviewOut
	c.mustOK("GET", "/dashboard/overview?"+dayRange()+extra, nil, &o)
	return o
}

// salesWorld: region R with stores A and B and a fixed set of sales whose figures are worked out by hand in the test.
type salesWorld struct {
	tn         *tenant
	region     string
	regionPath string
	a, b       storeOut
	p, q       item
	cashA      *client
	cashB      *client
	supEmail   string
	s1, s2     saleOut
	termA      string
	termB      string
	shiftA     string
}

func buildWorld(t *testing.T) *salesWorld {
	t.Helper()
	tn := newTenant(t)
	w := &salesWorld{tn: tn}
	w.region = tn.unit(t, "region", "R-"+uniq("r"), tn.RootUnit)
	w.a = tn.store(t, "A-"+uniq("s"), w.region)
	w.b = tn.store(t, "B-"+uniq("s"), w.region)
	w.regionPath = w.a.Store.Path[:strings.LastIndex(w.a.Store.Path, ".")]
	w.p = tn.product(t, uniq("P"), "50000")
	w.q = tn.product(t, uniq("Q"), "10000")
	for _, st := range []storeOut{w.a, w.b} {
		tn.stock(t, st, w.p, 10, "20000")
		tn.stock(t, st, w.q, 10, "4000")
	}
	w.cashA = tn.user(t, "cashier", w.a.Store.OrgUnitID)
	w.cashB = tn.user(t, "cashier", w.b.Store.OrgUnitID)
	tn.user(t, "supervisor", w.a.Store.OrgUnitID)
	_ = testPool.QueryRow(context.Background(), `select email from users where company_id=$1 and name='supervisor'`, uuid.MustParse(tn.CompanyID)).Scan(&w.supEmail)
	w.termA, w.termB = terminalOf(t, tn.Owner, w.a.Store.ID), terminalOf(t, tn.Owner, w.b.Store.ID)
	w.shiftA = openShift(t, w.cashA, w.termA, 0)
	openShift(t, w.cashB, w.termB, 0)

	cash := func(n int) []map[string]any { return []map[string]any{{"method": "CASH", "amount": n}} }
	qris := func(n int) []map[string]any {
		return []map[string]any{{"method": "QRIS", "amount": n, "reference": "Q"}}
	}
	// store A: 4 x P (200.000) cash, 2 x P (100.000) QRIS then voided, 1 of the 4 returned (50.000, restocked)
	w.cashA.mustOK("POST", "/pos/sales", checkoutBody(w.termA, []map[string]any{{"barcode": w.p.Barcode, "qty": 4}}, cash(200000)), &w.s1)
	w.cashA.mustOK("POST", "/pos/sales", checkoutBody(w.termA, []map[string]any{{"barcode": w.p.Barcode, "qty": 2}}, qris(100000)), &w.s2)
	w.cashA.mustOK("POST", "/pos/sales/"+w.s2.ID+"/void", map[string]any{"reason": "salah input", "approval": map[string]any{"approver_email": w.supEmail, "pin": "1234"}}, nil)
	w.cashA.mustOK("POST", "/pos/sales/"+w.s1.ID+"/returns", map[string]any{"reason": "ukuran", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": w.s1.Items[0].ID, "qty": 1}}}, nil)
	// store B: 3 x Q (30.000) and P + Q (60.000)
	var sb saleOut
	w.cashB.mustOK("POST", "/pos/sales", checkoutBody(w.termB, []map[string]any{{"barcode": w.q.Barcode, "qty": 3}}, cash(30000)), &sb)
	w.cashB.mustOK("POST", "/pos/sales", checkoutBody(w.termB, []map[string]any{{"barcode": w.p.Barcode, "qty": 1}, {"barcode": w.q.Barcode, "qty": 1}}, cash(60000)), &sb)
	return w
}

// Hand-worked figures (prices include 11% PPN; tax is rounded per sale):
//
//	A: s1 200.000 (tax 19.820 → net 180.180), refund 50.000 (net 45.045), voided s2 excluded → net 135.135, COGS 80.000-20.000=60.000, profit 75.135
//	B: 30.000 (tax 2.973 → 27.027) + 60.000 (tax 5.946 → 54.054) = net 81.081, COGS 12.000+24.000=36.000, profit 45.081
func TestSummaryMatchesHandWorkedFigures(t *testing.T) {
	w := buildWorld(t)
	tn := w.tn

	// Before the worker runs the summary is empty: reporting is asynchronous by design.
	if o := tn.Owner.overview(t, ""); o.Current.Transactions != 0 {
		t.Fatalf("summary should lag until the worker runs, got %+v", o.Current)
	}
	settle(t)

	o := tn.Owner.overview(t, "")
	if !o.ShowProfit || o.Current.Transactions != 3 || o.Current.Voided != 1 || o.Current.NetSales != "216216" || o.Current.Refund != "50000" || o.Current.GrossProfit != "120216" {
		t.Fatalf("company totals %+v", o.Current)
	}
	if o.Current.ItemsSold != "8" { // 4 + 3 + 1 + 1 sold, minus the 1 returned; the voided sale does not count
		t.Fatalf("items_sold=%s", o.Current.ItemsSold)
	}
	var hourly int64
	for _, h := range o.Hourly {
		hourly += h.Transactions
	}
	if hourly != 3 {
		t.Fatalf("hourly transactions %d", hourly)
	}
	pay := map[string]string{}
	for _, p := range o.Payments {
		pay[p.Method] = p.Amount
	}
	if pay["CASH"] != "290000" || pay["QRIS"] != "" {
		t.Fatalf("payments %+v (voided QRIS sale must not count)", o.Payments)
	}
	top := map[string]string{}
	for _, p := range o.TopProducts {
		top[p.SKU] = p.Qty + "|" + p.NetSales
	}
	if len(top) != 2 {
		t.Fatalf("top products %+v", o.TopProducts)
	}
	// dense daily series covering the whole range
	if len(o.Trend) != 4 {
		t.Fatalf("trend days %d", len(o.Trend))
	}

	// company → region → stores: "All" equals the sum of its parts at every level
	if o.BreakdownLevel != "region" || len(o.Breakdown) != 1 || o.Breakdown[0].NetSales != "216216" || o.Breakdown[0].Stores != 2 {
		t.Fatalf("company breakdown %+v", o.Breakdown)
	}
	r := tn.Owner.overview(t, "&under_path="+w.regionPath)
	if r.BreakdownLevel != "store" || len(r.Breakdown) != 2 {
		t.Fatalf("region breakdown %+v", r.Breakdown)
	}
	got := map[string]string{}
	for _, b := range r.Breakdown {
		got[b.Path] = b.NetSales
	}
	if got[w.a.Store.Path] != "135135" || got[w.b.Store.Path] != "81081" {
		t.Fatalf("per-store net %+v", got)
	}
	// a single store: its own totals, no breakdown
	sa := tn.Owner.overview(t, "&store_id="+w.a.Store.ID)
	if sa.Current.NetSales != "135135" || sa.Current.GrossProfit != "75135" || sa.Current.Refund != "50000" || sa.Current.Voided != 1 || len(sa.Breakdown) != 0 {
		t.Fatalf("store A %+v", sa)
	}
	// previous period of equal length is empty
	if o.Previous.Transactions != 0 {
		t.Fatalf("previous %+v", o.Previous)
	}

	// scoped users: store A's manager sees only A and no profit; asking for B yields nothing
	mgrA := tn.user(t, "store_manager", w.a.Store.OrgUnitID)
	ma := mgrA.overview(t, "")
	if ma.Current.NetSales != "135135" || ma.ShowProfit || ma.Current.GrossProfit != "0" {
		t.Fatalf("manager A overview %+v", ma)
	}
	if mb := mgrA.overview(t, "&under_path="+w.b.Store.Path); mb.Current.Transactions != 0 || mb.Current.NetSales != "0" {
		t.Fatalf("manager A must not see store B: %+v", mb)
	}
	if mb := mgrA.overview(t, "&store_id="+w.b.Store.ID); mb.Current.Transactions != 0 {
		t.Fatalf("manager A store filter must not widen scope: %+v", mb)
	}
	// a cashier has no dashboard permission
	w.cashA.wantErr("GET", "/dashboard/overview?"+dayRange(), nil, 403, "FORBIDDEN")
	// bad filters
	tn.Owner.wantErr("GET", "/dashboard/overview?from=2026-13-01", nil, 400, "BAD_REQUEST")
	tn.Owner.wantErr("GET", "/dashboard/overview?under_path=a%3Bdrop", nil, 400, "BAD_REQUEST")
	tn.Owner.wantErr("GET", "/dashboard/overview?from=2020-01-01&to=2026-01-01", nil, 400, "BAD_REQUEST")
}

func TestSummaryIsIdempotentAndSelfHealing(t *testing.T) {
	w := buildWorld(t)
	tn := w.tn
	settle(t)
	before := tn.Owner.overview(t, "")

	// rebuilding the same slices again changes nothing
	if _, err := testPool.Exec(context.Background(), `insert into summary_dirty (store_id, business_date, company_id)
		select store_id, business_date, company_id from store_kpi_daily where company_id=$1`, uuid.MustParse(tn.CompanyID)); err != nil {
		t.Fatal(err)
	}
	if n, err := summary.Drain(context.Background(), testApp.Tx); err != nil || n < 2 {
		t.Fatalf("drain n=%d err=%v", n, err)
	}
	after := tn.Owner.overview(t, "")
	if before.Current != after.Current {
		t.Fatalf("rebuild changed figures: %+v vs %+v", before.Current, after.Current)
	}
	// replaying every outbox event is harmless too (consumers are idempotent per event)
	if _, err := testPool.Exec(context.Background(), `delete from processed_events where consumer like 'summary.%'`); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `update outbox set published_at = null, attempts = 0 where company_id=$1`, uuid.MustParse(tn.CompanyID)); err != nil {
		t.Fatal(err)
	}
	settle(t)
	if again := tn.Owner.overview(t, ""); again.Current != before.Current {
		t.Fatalf("event replay changed figures: %+v", again.Current)
	}

	// corrupt the summary behind its back; the reconcile job notices and the rebuild repairs it
	if _, err := testPool.Exec(context.Background(), `update store_kpi_daily set grand_total = grand_total + 999, transactions = transactions + 1 where store_id=$1`, uuid.MustParse(w.b.Store.ID)); err != nil {
		t.Fatal(err)
	}
	if broken := tn.Owner.overview(t, ""); broken.Current.Transactions == before.Current.Transactions {
		t.Fatal("corruption should be visible before reconcile")
	}
	drifted, err := summary.Reconcile(context.Background(), testApp.Tx, 7, time.Now())
	if err != nil || drifted < 1 {
		t.Fatalf("reconcile drifted=%d err=%v", drifted, err)
	}
	if _, err := summary.Drain(context.Background(), testApp.Tx); err != nil {
		t.Fatal(err)
	}
	if healed := tn.Owner.overview(t, ""); healed.Current != before.Current {
		t.Fatalf("not healed: %+v", healed.Current)
	}
	// and a clean database reports no drift
	if n, err := summary.Reconcile(context.Background(), testApp.Tx, 7, time.Now()); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		// other tests' tenants may be mid-flight in the shared DB; only fail when OUR tenant drifts
		var mine int
		_ = testPool.QueryRow(context.Background(), `select count(*) from summary_dirty where company_id=$1`, uuid.MustParse(tn.CompanyID)).Scan(&mine)
		if mine != 0 {
			t.Fatalf("clean tenant drifted: %d", mine)
		}
		_, _ = summary.Drain(context.Background(), testApp.Tx)
	}
}

func TestReportsAndCSVExport(t *testing.T) {
	w := buildWorld(t)
	tn := w.tn
	settle(t)

	var cat []struct{ Key string }
	tn.Owner.mustOK("GET", "/reports", nil, &cat)
	if len(cat) != 10 {
		t.Fatalf("owner catalogue: %+v", cat)
	}
	type table struct {
		Report     string                 `json:"report"`
		Columns    []struct{ Key string } `json:"columns"`
		Rows       [][]any                `json:"rows"`
		Totals     []any                  `json:"totals"`
		ShowProfit bool                   `json:"show_profit"`
	}
	run := func(c *client, key, extra string) table {
		var tb table
		c.mustOK("GET", "/reports/"+key+"?"+dayRange()+extra, nil, &tb)
		return tb
	}
	byStore := run(tn.Owner, "sales_by_store", "")
	if len(byStore.Rows) != 2 || !byStore.ShowProfit {
		t.Fatalf("sales_by_store %+v", byStore)
	}
	colIdx := func(tb table, key string) int {
		for i, c := range tb.Columns {
			if c.Key == key {
				return i
			}
		}
		t.Fatalf("column %s missing in %+v", key, tb.Columns)
		return -1
	}
	net := colIdx(byStore, "net")
	if byStore.Totals[net] != "216216" {
		t.Fatalf("totals row %+v", byStore.Totals)
	}
	var sum int
	for _, r := range byStore.Rows {
		switch r[net] {
		case "135135":
			sum += 135135
		case "81081":
			sum += 81081
		}
	}
	if sum != 216216 {
		t.Fatalf("rows %+v", byStore.Rows)
	}
	if prod := run(tn.Owner, "sales_by_product", "&order=qty"); len(prod.Rows) != 2 || prod.Totals[colIdx(prod, "net")] != "216216" {
		t.Fatalf("sales_by_product %+v", prod)
	}
	if cat := run(tn.Owner, "sales_by_category", ""); cat.Totals[colIdx(cat, "net")] != "216216" {
		t.Fatalf("sales_by_category %+v", cat)
	}
	if day := run(tn.Owner, "sales_by_day", ""); day.Totals[colIdx(day, "transactions")] != float64(3) {
		t.Fatalf("sales_by_day totals %+v", day.Totals)
	}
	if v := run(tn.Owner, "inventory_valuation", ""); len(v.Rows) != 2 {
		t.Fatalf("valuation %+v", v)
	}

	// profit columns are hidden from a user without report.profit.view
	mgrA := tn.user(t, "store_manager", w.a.Store.OrgUnitID)
	ms := run(mgrA, "sales_by_day", "")
	if ms.ShowProfit {
		t.Fatal("store manager must not see profit")
	}
	for _, c := range ms.Columns {
		if c.Key == "cogs" || c.Key == "profit" || c.Key == "margin" {
			t.Fatalf("profit column leaked: %s", c.Key)
		}
	}
	// permission and scope checks
	w.cashA.wantErr("GET", "/reports", nil, 403, "FORBIDDEN")
	mgrA.wantErr("GET", "/reports/nope?"+dayRange(), nil, 404, "NOT_FOUND")
	if by := run(mgrA, "sales_by_store", ""); len(by.Rows) != 1 {
		t.Fatalf("manager A must see only his store: %+v", by.Rows)
	}

	// CSV: BOM, header, rows, totals, attachment name, and an audit trail of the export
	req, _ := http.NewRequest("GET", testSrv.URL+"/api/v1/reports/sales_by_store?"+dayRange()+"&format=csv", nil)
	req.Header.Set("Authorization", "Bearer "+tn.Owner.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || !strings.Contains(resp.Header.Get("Content-Disposition"), "sales_by_store_") {
		t.Fatalf("csv response %d %v", resp.StatusCode, resp.Header)
	}
	if !strings.HasPrefix(string(body), "\xEF\xBB\xBF") {
		t.Fatal("csv must start with a UTF-8 BOM for Excel")
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil || len(recs) != 4 { // header + 2 stores + totals
		t.Fatalf("csv rows %d err=%v\n%s", len(recs), err, body)
	}
	if recs[0][0] != "Kode" || recs[3][0] != "Total" {
		t.Fatalf("csv layout %v / %v", recs[0], recs[3])
	}
	var exports int
	_ = testPool.QueryRow(context.Background(), `select count(*) from audit_logs where company_id=$1 and action='REPORT_EXPORTED'`, uuid.MustParse(tn.CompanyID)).Scan(&exports)
	if exports != 1 {
		t.Fatalf("export audit rows: %d", exports)
	}
}

func TestNotificationsFromEvents(t *testing.T) {
	w := buildWorld(t)
	tn := w.tn
	mgrA := tn.user(t, "store_manager", w.a.Store.OrgUnitID)
	mgrB := tn.user(t, "store_manager", w.b.Store.OrgUnitID)
	// alert on the very first void of a day, and on any refund
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "pos.void_alert_per_day", "value": 1}, nil)
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "notify.refund_amount", "value": 0}, nil)

	var s3 saleOut
	w.cashA.mustOK("POST", "/pos/sales", checkoutBody(w.termA, []map[string]any{{"barcode": w.q.Barcode, "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 10000}}), &s3)
	w.cashA.mustOK("POST", "/pos/sales/"+s3.ID+"/void", map[string]any{"reason": "batal", "approval": map[string]any{"approver_email": w.supEmail, "pin": "1234"}}, nil)
	// close store A's shift short by 20.000 (threshold default 10.000)
	var sh struct {
		Totals struct {
			Expected string `json:"expected_cash"`
		} `json:"totals"`
	}
	w.cashA.mustOK("GET", "/pos/shifts/"+w.shiftA, nil, &sh)
	w.cashA.mustOK("POST", "/pos/shifts/"+w.shiftA+"/close", map[string]any{"counted_cash": 130000, "note": "kurang"}, nil) // expected 150.000 → variance -20.000
	settle(t)
	settle(t) // a second pass must not duplicate anything

	type notif struct {
		ID        string  `json:"id"`
		Type      string  `json:"type"`
		Severity  string  `json:"severity"`
		Title     string  `json:"title"`
		StoreName string  `json:"store_name"`
		ReadAt    *string `json:"read_at"`
	}
	list := func(c *client) []notif {
		var out []notif
		c.mustOK("GET", "/notifications", nil, &out)
		return out
	}
	types := func(ns []notif) map[string]int {
		m := map[string]int{}
		for _, n := range ns {
			m[n.Type]++
		}
		return m
	}
	a := types(list(mgrA))
	if a["void_alert"] != 1 || a["large_refund"] != 1 || a["shift_variance"] != 1 {
		t.Fatalf("manager A notifications: %v", a)
	}
	if b := list(mgrB); len(b) != 0 {
		t.Fatalf("manager B must not hear about store A: %+v", b)
	}
	if c := list(w.cashA); len(c) != 0 {
		t.Fatalf("cashiers have no report permission: %+v", c)
	}
	// owner (company-wide) receives them too; the shortage is critical and formatted in rupiah
	var shift notif
	for _, n := range list(tn.Owner) {
		if n.Type == "shift_variance" {
			shift = n
		}
	}
	if shift.Severity != "critical" || !strings.Contains(shift.Title, "-Rp 20.000") {
		t.Fatalf("shift notification %+v", shift)
	}

	// unread counter, mark one read, mark all read
	var cnt struct{ Unread int }
	mgrA.mustOK("GET", "/notifications/unread-count", nil, &cnt)
	if cnt.Unread != 3 {
		t.Fatalf("unread %d", cnt.Unread)
	}
	ns := list(mgrA)
	mgrA.mustOK("POST", "/notifications/"+ns[0].ID+"/read", nil, nil)
	mgrA.mustOK("GET", "/notifications/unread-count", nil, &cnt)
	if cnt.Unread != 2 {
		t.Fatalf("unread after one read %d", cnt.Unread)
	}
	// another user cannot mark somebody else's notification (silently ignored, count unchanged)
	mgrB.mustOK("POST", "/notifications/"+ns[1].ID+"/read", nil, nil)
	mgrA.mustOK("GET", "/notifications/unread-count", nil, &cnt)
	if cnt.Unread != 2 {
		t.Fatalf("foreign read changed state: %d", cnt.Unread)
	}
	mgrA.mustOK("POST", "/notifications/read-all", nil, nil)
	mgrA.mustOK("GET", "/notifications/unread-count", nil, &cnt)
	if cnt.Unread != 0 {
		t.Fatalf("unread after read-all %d", cnt.Unread)
	}
}

func TestLowStockNotificationOncePerDay(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "LS-"+uniq("s"), "")
	it := tn.product(t, uniq("L"), "10000")
	tn.stock(t, st, it, 3, "1000")
	// reorder point 5 → 3 on hand is below it
	var pid struct {
		ID string `json:"id"`
	}
	_ = pid
	if _, err := testPool.Exec(context.Background(), `update product_variants set reorder_point = 5 where id=$1`, uuid.MustParse(it.VariantID)); err != nil {
		t.Fatal(err)
	}
	buyer := tn.user(t, "store_manager", st.Store.OrgUnitID) // holds purchase_order.create
	n1, err := notification.CheckLowStock(context.Background(), testApp.Tx, time.Now())
	if err != nil || n1 < 1 {
		t.Fatalf("first check sent=%d err=%v", n1, err)
	}
	if _, err := notification.CheckLowStock(context.Background(), testApp.Tx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var ns []struct{ Type, Title string }
	buyer.mustOK("GET", "/notifications", nil, &ns)
	low := 0
	for _, n := range ns {
		if n.Type == "low_stock" {
			low++
		}
	}
	if low != 1 {
		t.Fatalf("expected exactly one low-stock notification per day, got %d (%+v)", low, ns)
	}
	// the low-stock report lists the SKU with a suggested order
	var rep struct {
		Rows [][]any `json:"rows"`
	}
	tn.Owner.mustOK("GET", "/reports/low_stock", nil, &rep)
	if len(rep.Rows) != 1 {
		t.Fatalf("low stock report %+v", rep)
	}
}

func TestApprovalInbox(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "AP-"+uniq("s"), "")
	it := tn.product(t, uniq("A"), "50000")
	sup := tn.supplier(t)
	mgr := tn.user(t, "store_manager", st.Store.OrgUnitID) // can create POs, cannot approve
	regional := tn.user(t, "regional_manager", "")         // can approve

	var po poOut
	mgr.mustOK("POST", "/purchase-orders", poBody(sup, st.Warehouse.ID, line(it.VariantID, 1000, 10000)), &po) // 10.000.000 > auto-approve limit
	mgr.mustOK("POST", "/purchase-orders/"+po.ID+"/submit", map[string]any{}, &po)
	if po.Status != "submitted" {
		t.Fatalf("po should wait for approval: %s", po.Status)
	}
	type inbox struct {
		Total  int            `json:"total"`
		Counts map[string]int `json:"counts"`
		Items  []struct {
			Type           string `json:"type"`
			DocumentNumber string `json:"document_number"`
			Href           string `json:"href"`
			Own            bool   `json:"own"`
		} `json:"items"`
	}
	var in inbox
	regional.mustOK("GET", "/approvals", nil, &in)
	if in.Total != 1 || in.Counts["purchase_order"] != 1 || in.Items[0].DocumentNumber != po.DocumentNumber || in.Items[0].Own {
		t.Fatalf("approver inbox %+v", in)
	}
	// the requester may approve transfers/counts in his store but not purchase orders, so his own PO is not in his inbox
	var mine inbox
	mgr.mustOK("GET", "/approvals", nil, &mine)
	if mine.Counts["purchase_order"] != 0 {
		t.Fatalf("requester must not see POs to approve: %+v", mine)
	}
	// a cashier has no approval rights at all
	tn.user(t, "cashier", st.Store.OrgUnitID).wantErr("GET", "/approvals", nil, 403, "FORBIDDEN")
	// once approved it leaves the inbox
	regional.mustOK("POST", "/purchase-orders/"+po.ID+"/approve", map[string]any{}, nil)
	regional.mustOK("GET", "/approvals", nil, &in)
	if in.Total != 0 {
		t.Fatalf("approved PO still pending: %+v", in)
	}
}

func TestStreamSignalsDashboardAndNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go testApp.Hub.Run(ctx, testPool)
	time.Sleep(300 * time.Millisecond) // let LISTEN attach

	w := buildWorld(t)
	tn := w.tn
	req, _ := http.NewRequestWithContext(ctx, "GET", testSrv.URL+"/api/v1/stream/events", nil)
	req.Header.Set("Authorization", "Bearer "+tn.Owner.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream %d %v", resp.StatusCode, resp.Header)
	}
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "event: ") {
				events <- strings.TrimPrefix(l, "event: ")
			}
		}
	}()
	// a sale → relay → summary rebuild → "dashboard" signal reaches the open tab
	settle(t)
	select {
	case e := <-events:
		if e != "dashboard" && e != "notification" {
			t.Fatalf("event %q", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE signal after the summary rebuild")
	}
	// another company's tab must not hear this tenant's signals
	other := newTenant(t)
	req2, _ := http.NewRequestWithContext(ctx, "GET", testSrv.URL+"/api/v1/stream/events", nil)
	req2.Header.Set("Authorization", "Bearer "+other.Owner.token)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	leak := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(resp2.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "event: ") {
				leak <- l
			}
		}
	}()
	w2 := buildWorld(t) // activity in a *different* company again (its own tenant), after `other` subscribed
	_ = w2
	settle(t)
	select {
	case l := <-leak:
		t.Fatalf("signal leaked across companies: %s", l)
	case <-time.After(1500 * time.Millisecond):
	}
}
