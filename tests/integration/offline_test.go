package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/ritel35k/backend/pkg/posoffline"
)

// edgeTerm is a terminal's edge agent as the backend sees it: its own token, its own endpoints.
type edgeTerm struct {
	t    *testing.T
	tn   *tenant
	dev  *device
	shop shop
	user string // cashier id (the owner)
}

func (e *edgeTerm) call(method, path string, body any) (int, envelope) {
	e.t.Helper()
	return (&device{t: e.t, token: e.dev.token}).do(method, "/pos"+path, body)
}

func newEdgeTerm(t *testing.T, tn *tenant, st storeOut) *edgeTerm {
	t.Helper()
	term := terminalOf(t, tn.Owner, st.Store.ID)
	var tok struct {
		Token string `json:"token"`
		Hint  string `json:"hint"`
	}
	tn.Owner.mustOK("POST", "/pos/terminals/"+term+"/sync-token", nil, &tok)
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	tn.Owner.mustOK("GET", "/auth/me", nil, &me)
	return &edgeTerm{t: t, tn: tn, dev: &device{t: t, token: tok.Token}, shop: shop{tn, st, term}, user: me.User.ID}
}

func (e *edgeTerm) snapshot() *posoffline.Snapshot {
	e.t.Helper()
	st, env := e.call("GET", "/snapshot", nil)
	if st != 200 {
		e.t.Fatalf("snapshot %d %+v", st, env.Error)
	}
	var s posoffline.Snapshot
	if err := json.Unmarshal(env.Data, &s); err != nil {
		e.t.Fatal(err)
	}
	return &s
}

func (e *edgeTerm) sync(cmds ...posoffline.Command) []posoffline.CommandResult {
	e.t.Helper()
	st, env := e.call("POST", "/sync", posoffline.SyncRequest{Commands: cmds, Pending: len(cmds)})
	if st != 200 {
		e.t.Fatalf("sync %d %+v", st, env.Error)
	}
	var r posoffline.SyncResponse
	if err := json.Unmarshal(env.Data, &r); err != nil {
		e.t.Fatal(err)
	}
	return r.Results
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// offlineSale prices a cart with the shared engine (what the edge does) and wraps it as a sale command.
func (e *edgeTerm) offlineSale(snap *posoffline.Snapshot, shiftRef posoffline.ShiftRef, at time.Time, items []posoffline.ItemInput, customer *uuid.UUID, cash int64, tags posoffline.TagResolver) posoffline.Command {
	e.t.Helper()
	eng := posoffline.New(snap)
	q, err := eng.Quote(posoffline.CartInput{TerminalID: uuid.MustParse(e.shop.term), Items: items, CustomerID: customer}, at, tags)
	if err != nil {
		e.t.Fatalf("offline quote: %v", err)
	}
	id := uuid.New()
	pay := q.GrandTotal
	tendered := decimal.NewFromInt(cash)
	if tendered.LessThan(pay) {
		tendered = pay
	}
	return posoffline.Command{Type: posoffline.CmdSale, ID: id, OccurredAt: at, Sale: &posoffline.SaleCommand{
		ShiftRef: shiftRef, CashierID: uuid.MustParse(e.user), LocalNumber: "L-" + id.String()[:6], SnapshotVersion: snap.Version, CustomerID: customer, Lines: q.Lines,
		Subtotal: q.Subtotal, DiscountTotal: q.DiscountTotal, PromoDiscount: q.PromoDiscount, TaxTotal: q.TaxTotal, GrandTotal: q.GrandTotal,
		Payments: []posoffline.SalePayment{{Method: "CASH", Amount: pay, Tendered: tendered}}, Change: tendered.Sub(pay)}}
}

// backdatePrices makes every current price effective since long ago, so a sale "from earlier today" can be priced.
func (tn *tenant) backdatePrices(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `update prices set effective_from = now() - interval '30 days' where company_id=$1 and effective_to is null`, uuid.MustParse(tn.CompanyID)); err != nil {
		t.Fatal(err)
	}
}

// priceChangedAgo sets a price, then moves the change back in time: it took effect `ago` ago on the server while the
// edge still holds an older snapshot.
func (tn *tenant) priceChangedAgo(t *testing.T, it item, price int, ago string) {
	t.Helper()
	tn.Owner.mustOK("PUT", "/prices", map[string]any{"org_unit_id": tn.RootUnit, "variant_id": it.VariantID, "price": price, "min_qty": 1}, nil)
	ctx, v := context.Background(), uuid.MustParse(it.VariantID)
	if _, err := testPool.Exec(ctx, `update prices set effective_to = now() - $2::interval where variant_id=$1 and effective_to is not null and effective_to > now() - interval '1 minute'`, v, ago); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `update prices set effective_from = now() - $2::interval where variant_id=$1 and effective_to is null`, v, ago); err != nil {
		t.Fatal(err)
	}
}

func vid(it item) *uuid.UUID { u := uuid.MustParse(it.VariantID); return &u }

func offItems(pairs ...any) []posoffline.ItemInput {
	var out []posoffline.ItemInput
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, posoffline.ItemInput{VariantID: vid(pairs[i].(item)), Qty: decimal.NewFromInt(int64(pairs[i+1].(int)))})
	}
	return out
}

// The shared engine and the server's /pos/quote must agree to the rupiah across price tiers, member lists,
// promotions and tax modes — otherwise an offline sale would later show up as a price mismatch.
func TestOfflineEngineMatchesOnlineQuote(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	st := tn.store(t, "OFF-A", jabo)
	a := tn.product(t, "OA", "100000")
	b := tn.product(t, "OB", "12500")
	c := tn.product(t, "OC", "33333")
	for _, it := range []item{a, b, c} {
		tn.stock(t, st, it, 1000, "1")
	}
	lv := tn.levels(t)
	var list struct{ ID string }
	tn.Owner.mustOK("POST", "/price-lists", map[string]any{"code": "OFF-GOLD", "name": "Gold", "membership_level_id": lv["GOLD"].ID}, &list)
	put := func(unit string, listID any, v item, minQty, price int) {
		body := map[string]any{"org_unit_id": unit, "variant_id": v.VariantID, "price": price, "min_qty": minQty}
		if listID != nil {
			body["price_list_id"] = listID
		}
		tn.Owner.mustOK("PUT", "/prices", body, nil)
	}
	put(tn.RootUnit, list.ID, a, 1, 90000)
	put(tn.RootUnit, nil, a, 3, 85000)
	put(jabo, nil, b, 1, 12000)
	put(jabo, nil, b, 10, 11000)
	tn.Owner.mustOK("POST", "/promotions", map[string]any{"code": "OFF-B2G1", "name": "B2G1", "type": "bxgy", "config": map[string]any{"buy_qty": 2, "get_qty": 1}, "stackable": true}, nil)
	tn.Owner.mustOK("POST", "/promotions", map[string]any{"code": "OFF-GOLD10", "name": "Gold 10", "type": "percent", "config": map[string]any{"percent": 10}, "scopes": map[string]any{"level_ids": []string{lv["GOLD"].ID}}}, nil)
	gold := tn.customer(t, tn.Owner, "081277700001")
	tn.Owner.mustOK("PATCH", "/customers/"+gold.ID, map[string]any{"level_id": lv["GOLD"].ID}, nil)
	plain := tn.customer(t, tn.Owner, "081277700002")

	tn.backdatePrices(t)
	e := newEdgeTerm(t, tn, st)
	snap := e.snapshot()
	if snap.Version == "" || snap.StoreID.String() != st.Store.ID || len(snap.Items) != 3 {
		t.Fatalf("snapshot: version=%q items=%d", snap.Version, len(snap.Items))
	}
	eng := posoffline.New(snap)
	now := time.Now()
	gid, pid := uuid.MustParse(gold.ID), uuid.MustParse(plain.ID)
	carts := []struct {
		name  string
		items []posoffline.ItemInput
		cust  *uuid.UUID
		cdp   string
	}{
		{"single", offItems(a, 1), nil, "0"},
		{"tier a", offItems(a, 3, b, 1), nil, "0"},
		{"region tiers b", offItems(b, 12, c, 2), nil, "0"},
		{"bxgy", offItems(b, 3, c, 3), nil, "0"},
		{"gold member", offItems(a, 1, b, 4, c, 1), &gid, "0"},
		{"gold tier", offItems(a, 5), &gid, "0"},
		{"plain member", offItems(a, 2, c, 1), &pid, "0"},
		{"cart discount", offItems(a, 2, b, 2), nil, "5"},
	}
	for _, cs := range carts {
		items := make([]map[string]any, 0, len(cs.items))
		for _, it := range cs.items {
			items = append(items, map[string]any{"variant_id": it.VariantID.String(), "qty": it.Qty.String()})
		}
		body := map[string]any{"terminal_id": e.shop.term, "items": items, "cart_discount_percent": cs.cdp}
		if cs.cust != nil {
			body["customer_id"] = cs.cust.String()
		}
		var online struct {
			GrandTotal    string `json:"grand_total"`
			TaxTotal      string `json:"tax_total"`
			Subtotal      string `json:"subtotal"`
			DiscountTotal string `json:"discount_total"`
			PromoDiscount string `json:"promo_discount"`
			PointsEarn    int64  `json:"points_earn"`
			Lines         []struct {
				UnitPrice string `json:"unit_price"`
				LineTotal string `json:"line_total"`
				TaxAmount string `json:"tax_amount"`
			} `json:"lines"`
		}
		tn.Owner.mustOK("POST", "/pos/quote", body, &online)
		off, err := eng.Quote(posoffline.CartInput{TerminalID: uuid.MustParse(e.shop.term), Items: cs.items, CartDiscountPercent: dec(cs.cdp), CustomerID: cs.cust}, now, nil)
		if err != nil {
			t.Fatalf("%s: %v", cs.name, err)
		}
		if off.GrandTotal.String() != online.GrandTotal || off.TaxTotal.String() != online.TaxTotal || off.Subtotal.String() != online.Subtotal ||
			off.DiscountTotal.String() != online.DiscountTotal || off.PromoDiscount.String() != online.PromoDiscount || off.PointsEarn != online.PointsEarn {
			t.Fatalf("%s: offline %s/%s/%s/%s/%s pts=%d != online %s/%s/%s/%s/%s pts=%d", cs.name, off.GrandTotal, off.TaxTotal, off.Subtotal, off.DiscountTotal, off.PromoDiscount, off.PointsEarn,
				online.GrandTotal, online.TaxTotal, online.Subtotal, online.DiscountTotal, online.PromoDiscount, online.PointsEarn)
		}
		for i, l := range off.Lines {
			if l.UnitPrice.String() != online.Lines[i].UnitPrice || l.LineTotal.String() != online.Lines[i].LineTotal || l.TaxAmount.String() != online.Lines[i].TaxAmount {
				t.Fatalf("%s line %d: offline %s/%s/%s != online %+v", cs.name, i, l.UnitPrice, l.LineTotal, l.TaxAmount, online.Lines[i])
			}
		}
	}
	// vouchers need the server: the engine says so instead of guessing
	if _, err := eng.Quote(posoffline.CartInput{Items: offItems(a, 1), VoucherCode: "X"}, now, nil); err == nil {
		t.Fatal("offline voucher must be refused")
	}
	// the version is stable until the catalogue changes
	if again := e.snapshot(); again.Version != snap.Version {
		t.Fatalf("snapshot version drifted without a change: %s vs %s", snap.Version, again.Version)
	}
	tn.priceChangedAgo(t, c, 30000, "1 hour")
	if changed := e.snapshot(); changed.Version == snap.Version {
		t.Fatal("snapshot version must change when a price changes")
	}
}

func TestOfflineSyncIsExactlyOnceAndKeepsTimes(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "OFF-B", "")
	a := tn.product(t, "XA", "35000")
	b := tn.product(t, "XB", "10000")
	tn.stock(t, st, a, 20, "20000")
	tn.stock(t, st, b, 20, "4000")
	cust := tn.customer(t, tn.Owner, "081277700010")
	tn.backdatePrices(t)
	e := newEdgeTerm(t, tn, st)
	snap := e.snapshot()

	// the store lost internet 3 hours ago: shift opened, three sales, shift closed — all queued at the edge
	base := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	shiftID := uuid.New()
	ref := posoffline.ShiftRef{ClientShiftID: &shiftID}
	cid := uuid.MustParse(cust.ID)
	open := posoffline.Command{Type: posoffline.CmdShiftOpen, ID: shiftID, OccurredAt: base, ShiftOpen: &posoffline.ShiftOpenCommand{CashierID: uuid.MustParse(e.user), OpeningCash: dec("100000")}}
	s1 := e.offlineSale(snap, ref, base.Add(10*time.Minute), offItems(a, 2), &cid, 100000, nil)
	s2 := e.offlineSale(snap, ref, base.Add(30*time.Minute), offItems(a, 1, b, 3), nil, 100000, nil)
	s3 := e.offlineSale(snap, ref, base.Add(50*time.Minute), offItems(b, 1), nil, 10000, nil)
	// expected drawer: 100000 + 70000 + 65000 + 10000 = 245000 -> count 244000 (short 1000)
	closeCmd := posoffline.Command{Type: posoffline.CmdShiftClose, ID: uuid.New(), OccurredAt: base.Add(2 * time.Hour),
		ShiftClose: &posoffline.ShiftCloseCommand{ShiftRef: ref, CountedCash: dec("244000"), Note: "offline day"}}
	batch := []posoffline.Command{open, s1, s2, s3, closeCmd}

	res := e.sync(batch...)
	if len(res) != 5 {
		t.Fatalf("results %d", len(res))
	}
	for i, r := range res {
		if r.Status != posoffline.StatusApplied {
			t.Fatalf("command %d (%s): %+v", i, r.Type, r)
		}
	}
	if res[1].DocumentNumber == "" || res[1].DocumentNumber == "L-"+s1.ID.String()[:6] || len(res[1].Exceptions) != 0 {
		t.Fatalf("official number / exceptions: %+v", res[1])
	}
	if tn.onHand(t, st, a) != "17" || tn.onHand(t, st, b) != "16" {
		t.Fatalf("stock after sync: a=%s b=%s", tn.onHand(t, st, a), tn.onHand(t, st, b))
	}

	// replaying the whole batch (lost response) changes nothing
	again := e.sync(batch...)
	for i, r := range again {
		if r.Status != posoffline.StatusDuplicate {
			t.Fatalf("replay %d: %+v", i, r)
		}
	}
	if again[1].SaleID == nil || res[1].SaleID == nil || *again[1].SaleID != *res[1].SaleID || again[1].DocumentNumber != res[1].DocumentNumber {
		t.Fatalf("replay must point at the original sale: %+v vs %+v", again[1], res[1])
	}
	if tn.onHand(t, st, a) != "17" || tn.onHand(t, st, b) != "16" {
		t.Fatalf("replay moved stock: a=%s b=%s", tn.onHand(t, st, a), tn.onHand(t, st, b))
	}
	var nSales, nMoves int
	_ = testPool.QueryRow(context.Background(), `select count(*) from sales where terminal_id=$1 and offline`, uuid.MustParse(e.shop.term)).Scan(&nSales)
	_ = testPool.QueryRow(context.Background(), `select count(*) from inventory_movements where ref_type='sale' and ref_id=$1`, *res[1].SaleID).Scan(&nMoves)
	if nSales != 3 || nMoves != 1 {
		t.Fatalf("sales=%d moves(for s1)=%d", nSales, nMoves)
	}

	// history keeps WHEN it happened, not when it synced
	var occ, created, moveAt time.Time
	var local, snapV string
	var bizDate time.Time
	_ = testPool.QueryRow(context.Background(), `select occurred_at, created_at, local_number, snapshot_version, business_date from sales where id=$1`, *res[1].SaleID).Scan(&occ, &created, &local, &snapV, &bizDate)
	_ = testPool.QueryRow(context.Background(), `select created_at from inventory_movements where ref_type='sale' and ref_id=$1`, *res[1].SaleID).Scan(&moveAt)
	if !occ.Equal(s1.OccurredAt) || !created.Equal(s1.OccurredAt) || !moveAt.Equal(s1.OccurredAt) || local != s1.Sale.LocalNumber || snapV != snap.Version {
		t.Fatalf("times: occurred=%v created=%v move=%v want %v; local=%s snap=%s", occ, created, moveAt, s1.OccurredAt, local, snapV)
	}

	// member points were credited at sync from the server's rules
	got := tn.getCustomer(t, cust.ID)
	if got.PointsBalance <= 0 || got.PointsBalance != int64(s1.Sale.GrandTotal.IntPart()/10000) {
		t.Fatalf("points: %d for %s", got.PointsBalance, s1.Sale.GrandTotal)
	}

	// the shift closed with the right drawer maths and variance, at the time it really closed
	var status string
	var exp, counted, variance decimal.Decimal
	var closedAt time.Time
	_ = testPool.QueryRow(context.Background(), `select status, expected_cash, counted_cash, variance, closed_at from cashier_shifts where terminal_id=$1 and client_shift_id=$2`,
		uuid.MustParse(e.shop.term), shiftID).Scan(&status, &exp, &counted, &variance, &closedAt)
	if status != "closed" || !exp.Equal(dec("245000")) || !variance.Equal(dec("-1000")) || !closedAt.Equal(closeCmd.OccurredAt) {
		t.Fatalf("shift: %s expected=%s variance=%s closed=%v", status, exp, variance, closedAt)
	}

	// sync status shows up in the back office
	var rows []struct {
		TerminalID string `json:"terminal_id"`
		Pending    int    `json:"pending"`
		LastSyncAt string `json:"last_sync_at"`
	}
	tn.Owner.mustOK("GET", "/pos/sync-status", nil, &rows)
	if len(rows) != 1 || rows[0].TerminalID != e.shop.term || rows[0].LastSyncAt == "" || rows[0].Pending != 0 {
		t.Fatalf("sync status: %+v", rows)
	}
}

type excRow struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Severity       string `json:"severity"`
	Status         string `json:"status"`
	DocumentNumber string `json:"document_number"`
	Detail         json.RawMessage
}

func (tn *tenant) exceptions(t *testing.T, c *client, q string) []excRow {
	t.Helper()
	var rows []excRow
	c.mustOK("GET", "/pos/exceptions?per_page=100"+q, nil, &rows)
	return rows
}

func typesOf(rows []excRow) map[string]int {
	m := map[string]int{}
	for _, r := range rows {
		m[r.Type]++
	}
	return m
}

func TestOfflineDifferencesBecomeExceptionsNotRejections(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "OFF-C", "")
	other := tn.store(t, "OFF-D", "")
	a := tn.product(t, "YA", "50000")
	b := tn.product(t, "YB", "20000")
	r := tn.rfidProduct(t, "YR", "70000")
	tn.stock(t, st, a, 2, "20000") // deliberately thin: the offline sale below sells more than exists
	tn.stock(t, st, b, 50, "5000")
	epcs := newEPCs(1)
	tn.registerTags(t, tn.Owner, r, st, "ON_SHELF", epcs)
	tn.stock(t, st, r, 1, "30000")
	mgr := tn.user(t, "store_manager", st.Store.OrgUnitID)
	mgrOther := tn.user(t, "store_manager", other.Store.OrgUnitID)
	cashier := tn.user(t, "cashier", st.Store.OrgUnitID)
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "pos.allow_negative_stock", "value": false}, nil) // so overselling is an exception
	tn.backdatePrices(t)
	e := newEdgeTerm(t, tn, st)
	snap := e.snapshot()
	now := time.Now().UTC()

	// auth + scope of the device credential
	if s, _ := (&device{t: t, token: "trm_nope"}).do("GET", "/pos/snapshot", nil); s != 401 {
		t.Fatalf("bad terminal token: %d", s)
	}
	if s, _ := (&device{t: t}).do("POST", "/pos/sync", posoffline.SyncRequest{}); s != 401 {
		t.Fatalf("no token: %d", s)
	}
	if s, _ := (&device{t: t, token: e.dev.token}).do("GET", "/snapshot", nil); s != 401 { // a terminal token is not a reader token
		t.Fatalf("terminal token must not open reader endpoints: %d", s)
	}
	mgrOther.wantErr("POST", "/pos/terminals/"+e.shop.term+"/sync-token", nil, 404, "NOT_FOUND")
	cashier.wantErr("POST", "/pos/terminals/"+e.shop.term+"/sync-token", nil, 403, "FORBIDDEN")

	shiftID := uuid.New()
	ref := posoffline.ShiftRef{ClientShiftID: &shiftID}
	open := posoffline.Command{Type: posoffline.CmdShiftOpen, ID: shiftID, OccurredAt: now.Add(-time.Hour), ShiftOpen: &posoffline.ShiftOpenCommand{CashierID: uuid.MustParse(e.user)}}

	// 1) the price changed online after the snapshot (big difference), plus the stock runs out
	tn.priceChangedAgo(t, a, 80000, "2 hours")
	pricey := e.offlineSale(snap, ref, now.Add(-30*time.Minute), offItems(a, 3), nil, 150000, nil) // 3 x 50000 offline, 3 x 80000 on the server; only 2 in stock
	// 2) a tiny difference stays a warning
	tn.priceChangedAgo(t, b, 20100, "2 hours")
	small := e.offlineSale(snap, ref, now.Add(-20*time.Minute), offItems(b, 1), nil, 20000, nil)
	// 3) an RFID tag sold online while the store was offline, then sold again offline
	soldOnline := newEPCs(1)[0]
	_ = soldOnline
	tagSale := e.offlineSale(snap, ref, now.Add(-10*time.Minute), offItems(r, 1), nil, 70000, func(string) (uuid.UUID, error) { return uuid.MustParse(r.VariantID), nil })
	tagSale.Sale.Lines[0].EPC = epcs[0]
	openOnline := openShift(t, tn.Owner, e.shop.term, 0) // the online till sells that very tag first
	if _, err := testPool.Exec(context.Background(), `update cashier_shifts set opened_at = now() - interval '3 hours' where id=$1`, uuid.MustParse(openOnline)); err != nil {
		t.Fatal(err)
	}
	tn.Owner.mustOK("POST", "/pos/sales", checkoutBody(e.shop.term, []map[string]any{{"epc": epcs[0], "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 100000}}), nil)
	_ = openOnline
	// 4) a sale that cannot be booked: totals do not add up
	broken := e.offlineSale(snap, ref, now.Add(-5*time.Minute), offItems(b, 1), nil, 20000, nil)
	broken.Sale.GrandTotal = dec("1")

	res := e.sync(open, pricey, small, tagSale, broken)
	if len(res) != 5 {
		t.Fatalf("results: %+v", res)
	}
	// the online shift above already existed for this cashier+terminal, so the offline open is a conflict, and sales fall back to it
	if res[0].Status != posoffline.StatusRejected || res[0].Code != "SHIFT_CONFLICT" {
		t.Fatalf("shift open: %+v", res[0])
	}
	for i := 1; i <= 3; i++ {
		if res[i].Status != posoffline.StatusApplied {
			t.Fatalf("sale %d must be booked as given: %+v", i, res[i])
		}
	}
	if res[4].Status != posoffline.StatusRejected || res[4].Code != "INCONSISTENT_SALE" {
		t.Fatalf("broken sale: %+v", res[4])
	}

	rows := tn.exceptions(t, mgr, "")
	got := typesOf(rows)
	if got["PRICE_MISMATCH"] != 2 || got["NEGATIVE_STOCK"] != 2 /* oversold item + the twice-sold tagged item */ || got["TAG_CONFLICT"] != 1 || got["REJECTED_SALE"] != 1 {
		t.Fatalf("exceptions: %v", got)
	}
	sev := map[string]string{}
	for _, x := range rows {
		if x.Type == "PRICE_MISMATCH" {
			sev[x.DocumentNumber] = x.Severity
		}
	}
	if sev[res[1].DocumentNumber] != "critical" || sev[res[2].DocumentNumber] != "warning" {
		t.Fatalf("price mismatch severities: %v (big=%s small=%s)", sev, res[1].DocumentNumber, res[2].DocumentNumber)
	}

	// a large difference also raises a security event and notifies the people who watch the store
	var events []struct {
		Type, Severity string
	}
	mgr.mustOK("GET", "/security/events?type=OFFLINE_PRICE_MISMATCH", nil, &events)
	if len(events) != 1 || events[0].Severity != "critical" {
		t.Fatalf("security events: %+v", events)
	}
	var notes []struct{ Type, Severity string }
	mgr.mustOK("GET", "/notifications", nil, &notes)
	seen := map[string]bool{}
	for _, n := range notes {
		seen[n.Type] = true
	}
	if !seen["OFFLINE_PRICE_MISMATCH"] || !seen["OFFLINE_SALE_REJECTED"] {
		t.Fatalf("notifications: %+v", notes)
	}

	// the money is in: stock went negative (flagged), the first tag buyer keeps the tag
	if got := tn.onHand(t, st, a); got != "-1" {
		t.Fatalf("stock a after oversell: %s", got)
	}
	var tagSales int
	_ = testPool.QueryRow(context.Background(), `select count(*) from sale_item_tags where epc=$1`, epcs[0]).Scan(&tagSales)
	if tagSales != 2 {
		t.Fatalf("both sales are recorded against the tag: %d", tagSales)
	}

	// review workflow and scope
	var pm excRow
	for _, x := range rows {
		if x.Type == "PRICE_MISMATCH" && x.Severity == "warning" {
			pm = x
		}
	}
	cashier.wantErr("GET", "/pos/exceptions", nil, 403, "FORBIDDEN")
	if other := tn.exceptions(t, mgrOther, ""); len(other) != 0 {
		t.Fatalf("another store's manager sees %d exceptions", len(other))
	}
	cashier.wantErr("POST", "/pos/exceptions/"+pm.ID+"/review", map[string]any{"status": "reviewed"}, 403, "FORBIDDEN")
	mgr.wantErr("POST", "/pos/exceptions/"+pm.ID+"/review", map[string]any{"status": "reviewed"}, 403, "FORBIDDEN") // resolving is a head-office duty, like security events
	tn.Owner.mustOK("POST", "/pos/exceptions/"+pm.ID+"/review", map[string]any{"status": "reviewed", "note": "price list was updated, fine"}, nil)
	tn.Owner.wantErr("POST", "/pos/exceptions/"+pm.ID+"/review", map[string]any{"status": "dismissed"}, 409, "ALREADY_REVIEWED")
	if open := tn.exceptions(t, mgr, "&status=open"); len(open) != 5 {
		t.Fatalf("open exceptions after one review: %d", len(open))
	}

	// the rejected sale keeps its whole payload so the takings can be entered by hand
	var payload string
	_ = testPool.QueryRow(context.Background(), `select detail::text from offline_exceptions where type='REJECTED_SALE' and company_id=$1`, uuid.MustParse(tn.CompanyID)).Scan(&payload)
	if !strings.Contains(payload, broken.ID.String()) || !strings.Contains(payload, "INCONSISTENT_SALE") {
		t.Fatalf("rejected payload lost data: %s", payload)
	}
}

func TestOfflineSaleIntoClosedShiftAndUnknownShift(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "OFF-E", "")
	a := tn.product(t, "ZA", "10000")
	tn.stock(t, st, a, 10, "5000")
	tn.backdatePrices(t)
	e := newEdgeTerm(t, tn, st)
	snap := e.snapshot()
	now := time.Now().UTC()

	// an online shift that is closed while the edge still holds a sale from before the close
	shID := openShift(t, tn.Owner, e.shop.term, 50000)
	tn.Owner.mustOK("POST", "/pos/shifts/"+shID+"/close", map[string]any{"counted_cash": 50000}, nil)
	late := e.offlineSale(snap, posoffline.ShiftRef{}, now.Add(-time.Minute), offItems(a, 1), nil, 10000, nil)
	late.Sale.ShiftRef.ShiftID = ptrUUID(uuid.MustParse(shID))
	res := e.sync(late)
	if res[0].Status != posoffline.StatusApplied || len(res[0].Exceptions) != 1 || res[0].Exceptions[0] != "LATE_SALE" {
		t.Fatalf("late sale: %+v", res[0])
	}

	// no shift at all can cover this moment: kept as a REJECTED_SALE, never lost
	ghost := e.offlineSale(snap, posoffline.ShiftRef{ClientShiftID: ptrUUID(uuid.New())}, now.Add(-72*time.Hour), offItems(a, 1), nil, 10000, nil)
	res = e.sync(ghost)
	if res[0].Status != posoffline.StatusRejected || res[0].Code != "NO_SHIFT" {
		t.Fatalf("ghost sale: %+v", res[0])
	}
	if got := typesOf(tn.exceptions(t, tn.Owner, "")); got["LATE_SALE"] != 1 || got["REJECTED_SALE"] != 1 {
		t.Fatalf("exceptions: %v", got)
	}
	// an unknown command type is rejected, not retried forever
	res = e.sync(posoffline.Command{Type: "teleport", ID: uuid.New()})
	if res[0].Status != posoffline.StatusRejected || res[0].Code != "UNKNOWN_COMMAND" {
		t.Fatalf("unknown command: %+v", res[0])
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }
