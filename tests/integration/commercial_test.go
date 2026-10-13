package integration

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/customer"
	"github.com/ritel35k/backend/internal/platform/tx"
)

type levelOut struct {
	ID, Code string
	Rank     int
}

func (tn *tenant) levels(t *testing.T) map[string]levelOut {
	t.Helper()
	var ls []levelOut
	tn.Owner.mustOK("GET", "/membership-levels", nil, &ls)
	m := map[string]levelOut{}
	for _, l := range ls {
		m[l.Code] = l
	}
	return m
}

type custOut struct {
	ID            string `json:"id"`
	MemberNo      string `json:"member_no"`
	Phone         string `json:"phone"`
	PointsBalance int64  `json:"points_balance"`
	LevelID       string `json:"level_id"`
	LevelName     string `json:"level_name"`
	LifetimeSpend string `json:"lifetime_spend"`
}

func (tn *tenant) customer(t *testing.T, c *client, phone string) custOut {
	t.Helper()
	var out custOut
	c.mustOK("POST", "/customers", map[string]any{"name": "Pelanggan " + phone, "phone": phone}, &out)
	return out
}

func (tn *tenant) getCustomer(t *testing.T, id string) custOut {
	t.Helper()
	var out custOut
	tn.Owner.mustOK("GET", "/customers/"+id, nil, &out)
	return out
}

// sellable store with one terminal, an open owner shift and stocked products.
type shop struct {
	tn   *tenant
	st   storeOut
	term string
}

func newShop(t *testing.T, tn *tenant, code string) shop {
	st := tn.store(t, code, "")
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, tn.Owner, term, 0)
	return shop{tn, st, term}
}

func (s shop) item(t *testing.T, code, price string) item {
	it := s.tn.product(t, code, price)
	s.tn.stock(t, s.st, it, 1000, "1")
	return it
}

func (s shop) quote(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	b := map[string]any{}
	for k, v := range body {
		b[k] = v
	}
	b["terminal_id"] = s.term
	var q map[string]any
	s.tn.Owner.mustOK("POST", "/pos/quote", b, &q)
	return q
}

func lineItem(it item, qty int) map[string]any {
	return map[string]any{"barcode": it.Barcode, "qty": qty}
}

func (s shop) sell(t *testing.T, body map[string]any, pays ...map[string]any) saleFull {
	t.Helper()
	b := map[string]any{}
	for k, v := range body {
		b[k] = v
	}
	b["terminal_id"] = s.term
	b["client_txn_id"] = uuid.NewString()
	b["payments"] = pays
	var out saleFull
	s.tn.Owner.mustOK("POST", "/pos/sales", b, &out)
	return out
}

type saleFull struct {
	saleOut
	CustomerID      *string `json:"customer_id"`
	PointsEarned    int64   `json:"points_earned"`
	PointsRedeemed  int64   `json:"points_redeemed"`
	PromoDiscount   string  `json:"promo_discount"`
	VoucherDiscount string  `json:"voucher_discount"`
	Promotions      []struct {
		Name   string `json:"name"`
		Amount string `json:"amount"`
	} `json:"promotions"`
}

func cashPay(n int) map[string]any { return map[string]any{"method": "CASH", "amount": n} }

func itoa(n int) string { return strconv.Itoa(n) }

func receiptText(t *testing.T, c *client, saleID string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", testSrv.URL+"/api/v1/pos/sales/"+saleID+"/receipt?format=text", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestPriceListsVolumeTiersAndInheritance(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	st := tn.store(t, "ST-A", jabo)
	it := tn.product(t, "SKU-A", "100000")
	tn.stock(t, st, it, 100, "1")
	lv := tn.levels(t)

	var list struct{ ID string }
	tn.Owner.mustOK("POST", "/price-lists", map[string]any{"code": "MEMBER-GOLD", "name": "Harga Gold", "membership_level_id": lv["GOLD"].ID}, &list)
	tn.Owner.wantErr("POST", "/price-lists", map[string]any{"code": "MEMBER-GOLD", "name": "dup"}, 409, "PRICE_LIST_CODE_TAKEN")
	set := func(unit string, listID any, minQty, price int) {
		body := map[string]any{"org_unit_id": unit, "variant_id": it.VariantID, "price": price, "min_qty": minQty}
		if listID != nil {
			body["price_list_id"] = listID
		}
		tn.Owner.mustOK("PUT", "/prices", body, nil)
	}
	price := func(level string, qty int) string {
		var r struct {
			Price string `json:"price"`
		}
		url := "/prices/resolve?store_id=" + st.Store.ID + "&variant_id=" + it.VariantID + "&qty=" + itoa(qty)
		if level != "" {
			url += "&level_id=" + lv[level].ID
		}
		tn.Owner.mustOK("GET", url, nil, &r)
		return r.Price
	}

	set(tn.RootUnit, list.ID, 1, 90000) // member list
	set(tn.RootUnit, nil, 3, 85000)     // volume tier on the normal list (3+ units)
	if price("", 1) != "100000" || price("GOLD", 1) != "90000" {
		t.Fatalf("member price: normal=%s gold=%s", price("", 1), price("GOLD", 1))
	}
	if price("", 3) != "85000" || price("GOLD", 3) != "85000" { // best (lowest) applicable list wins
		t.Fatalf("tier: normal=%s gold=%s", price("", 3), price("GOLD", 3))
	}
	if price("SILVER", 1) != "100000" { // a list bound to another level does not apply
		t.Fatalf("silver must not get the gold list: %s", price("SILVER", 1))
	}
	// nearest scope wins before tiers: a region price of 95000 (min 1) hides the company tier for stores in that region
	set(jabo, nil, 1, 95000)
	if price("", 1) != "95000" || price("", 3) != "95000" {
		t.Fatalf("region override beats company tier: %s / %s", price("", 1), price("", 3))
	}
	set(jabo, nil, 5, 80000) // ...but the region can define its own tiers
	if price("", 5) != "80000" || price("", 4) != "95000" {
		t.Fatalf("region tier: %s / %s", price("", 5), price("", 4))
	}
	// the POS charges the same resolved price
	s := shop{tn, st, terminalOf(t, tn.Owner, st.Store.ID)}
	openShift(t, tn.Owner, s.term, 0)
	cid := tn.customer(t, tn.Owner, "081200000001")
	tn.Owner.mustOK("PATCH", "/customers/"+cid.ID, map[string]any{"level_id": lv["GOLD"].ID}, nil)
	q := s.quote(t, map[string]any{"items": []map[string]any{lineItem(it, 1)}, "customer_id": cid.ID})
	if q["grand_total"] != "90000" {
		t.Fatalf("gold member pays the member price (lower than region 95000): %v", q["grand_total"])
	}
}

func TestPromotionsInCheckout(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	bali := tn.unit(t, "region", "BALI", tn.RootUnit)
	s := newShop(t, tn, "ST-A")
	// a second store in another region to prove org scoping
	other := tn.store(t, "ST-B", bali)
	_ = jabo
	a := s.item(t, "SKU-A", "30000")
	b := s.item(t, "SKU-B", "20000")
	c := s.item(t, "SKU-C", "10000")
	cart := map[string]any{"items": []map[string]any{lineItem(a, 1), lineItem(b, 1), lineItem(c, 1)}}

	// config validation happens before anything reaches the POS
	tn.Owner.wantErr("POST", "/promotions", map[string]any{"code": "BAD", "name": "bad", "type": "percent", "config": map[string]any{"percent": 150}}, 422, "VALIDATION_FAILED")
	tn.Owner.wantErr("POST", "/promotions", map[string]any{"code": "BAD2", "name": "bad", "type": "threshold", "config": map[string]any{"min_spend": 1000}}, 422, "VALIDATION_FAILED")

	var promo struct {
		ID string `json:"id"`
	}
	tn.Owner.mustOK("POST", "/promotions", map[string]any{"code": "B2G1", "name": "Beli 2 Gratis 1", "type": "bxgy", "config": map[string]any{"buy_qty": 2, "get_qty": 1}, "stackable": true}, &promo)
	q := s.quote(t, cart)
	if q["promo_discount"] != "10000" || q["grand_total"] != "50000" {
		t.Fatalf("buy2get1 (cheapest free): %v / %v", q["promo_discount"], q["grand_total"])
	}
	sale := s.sell(t, cart, cashPay(50000))
	if sale.GrandTotal != "50000" || sale.PromoDiscount != "10000" || len(sale.Promotions) != 1 || sale.Promotions[0].Name != "Beli 2 Gratis 1" {
		t.Fatalf("sale %+v", sale)
	}

	// scoped to Bali only: the Jabodetabek store must not see it
	tn.Owner.mustOK("PUT", "/promotions/"+promo.ID, map[string]any{"code": "B2G1", "name": "Beli 2 Gratis 1", "type": "bxgy", "config": map[string]any{"buy_qty": 2, "get_qty": 1},
		"stackable": true, "scopes": map[string]any{"org_unit_ids": []string{bali}}}, nil)
	if q := s.quote(t, cart); q["promo_discount"] != "0" {
		t.Fatalf("outside scope: %v", q["promo_discount"])
	}
	_ = other
	// inactive and future promotions are ignored
	tn.Owner.mustOK("PUT", "/promotions/"+promo.ID, map[string]any{"code": "B2G1", "name": "x", "type": "bxgy", "config": map[string]any{"buy_qty": 2, "get_qty": 1}, "status": "inactive"}, nil)
	if q := s.quote(t, cart); q["promo_discount"] != "0" {
		t.Fatalf("inactive: %v", q["promo_discount"])
	}
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	tn.Owner.mustOK("POST", "/promotions", map[string]any{"code": "SOON", "name": "Soon", "type": "percent", "config": map[string]any{"percent": 50}, "starts_at": future}, nil)
	if q := s.quote(t, cart); q["promo_discount"] != "0" {
		t.Fatalf("future: %v", q["promo_discount"])
	}
	// store managers can't create promotions
	mgr := tn.user(t, "store_manager", s.st.Store.OrgUnitID)
	mgr.wantErr("POST", "/promotions", map[string]any{"code": "X1", "name": "x", "type": "percent", "config": map[string]any{"percent": 5}}, 403, "FORBIDDEN")
	tn.Owner.wantErr("POST", "/promotions", map[string]any{"code": "SOON", "name": "dup", "type": "percent", "config": map[string]any{"percent": 5}}, 409, "PROMOTION_CODE_TAKEN")
}

func TestMemberOnlyPromotionAndStackingOrder(t *testing.T) {
	tn := newTenant(t)
	s := newShop(t, tn, "ST-A")
	it := s.item(t, "SKU-A", "100000")
	lv := tn.levels(t)
	tn.Owner.mustOK("POST", "/promotions", map[string]any{"code": "GOLD10", "name": "Gold 10%", "type": "percent", "config": map[string]any{"percent": 10}, "scopes": map[string]any{"level_ids": []string{lv["GOLD"].ID}}}, nil)
	gold := tn.customer(t, tn.Owner, "081200000010")
	reg := tn.customer(t, tn.Owner, "081200000011")
	tn.Owner.mustOK("PATCH", "/customers/"+gold.ID, map[string]any{"level_id": lv["GOLD"].ID}, nil)

	body := func(cust string, extra map[string]any) map[string]any {
		m := map[string]any{"items": []map[string]any{{"barcode": it.Barcode, "qty": 2, "discount_percent": 10}}}
		if cust != "" {
			m["customer_id"] = cust
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	if q := s.quote(t, body("", nil)); q["promo_discount"] != "0" {
		t.Fatalf("anonymous gets no member promo: %v", q["promo_discount"])
	}
	if q := s.quote(t, body(reg.ID, nil)); q["promo_discount"] != "0" {
		t.Fatalf("regular gets no gold promo: %v", q["promo_discount"])
	}
	// order of operations: gross 200000 -> promo 10% = 20000 -> 180000 -> manual 10% = 162000 -> voucher 12000 = 150000
	tn.Owner.mustOK("POST", "/vouchers", map[string]any{"code": "TWELVE", "kind": "fixed", "value": 12000}, nil)
	q := s.quote(t, body(gold.ID, map[string]any{"voucher_code": "TWELVE"}))
	if q["promo_discount"] != "20000" || q["voucher_discount"] != "12000" || q["grand_total"] != "150000" || q["discount_total"] != "50000" {
		t.Fatalf("stacking math: %v", q)
	}
}

func TestVoucherRules(t *testing.T) {
	tn := newTenant(t)
	s := newShop(t, tn, "ST-A")
	it := s.item(t, "SKU-A", "50000")
	cart := func(qty int, code string) map[string]any {
		return map[string]any{"items": []map[string]any{lineItem(it, qty)}, "voucher_code": code}
	}
	mk := func(body map[string]any) { tn.Owner.mustOK("POST", "/vouchers", body, nil) }
	mk(map[string]any{"code": "TENK", "kind": "fixed", "value": 10000, "min_spend": 80000, "max_uses": 1})
	mk(map[string]any{"code": "PCT20", "kind": "percent", "value": 20, "max_discount": 15000})
	mk(map[string]any{"code": "OLDV", "kind": "fixed", "value": 5000, "valid_to": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	owner := tn.customer(t, tn.Owner, "081200000020")
	other := tn.customer(t, tn.Owner, "081200000021")
	mk(map[string]any{"code": "MINE", "kind": "fixed", "value": 5000, "customer_id": owner.ID})

	quoteErr := func(qty int, code string, status int, want string) {
		t.Helper()
		body := cart(qty, code)
		body["terminal_id"] = s.term
		tn.Owner.wantErr("POST", "/pos/quote", body, status, want)
	}
	quoteErr(1, "NOPE", 404, "VOUCHER_NOT_FOUND")
	quoteErr(1, "TENK", 409, "VOUCHER_MIN_SPEND") // 50000 < 80000
	quoteErr(1, "OLDV", 409, "VOUCHER_EXPIRED")
	quoteErr(1, "MINE", 409, "VOUCHER_CUSTOMER_ONLY")
	if q := s.quote(t, cart(2, "pct20")); q["voucher_discount"] != "15000" { // 20% of 100000 = 20000, capped at 15000; code is case-insensitive
		t.Fatalf("percent cap: %v", q["voucher_discount"])
	}
	personal := cart(1, "MINE")
	personal["customer_id"] = owner.ID
	if q := s.quote(t, personal); q["voucher_discount"] != "5000" {
		t.Fatalf("personal voucher for its owner: %v", q["voucher_discount"])
	}
	personal["customer_id"] = other.ID
	body := personal
	body["terminal_id"] = s.term
	tn.Owner.wantErr("POST", "/pos/quote", body, 409, "VOUCHER_CUSTOMER_ONLY")

	// single-use voucher: used once, then spent; voiding the sale frees it again
	sale := s.sell(t, cart(2, "TENK"), cashPay(90000))
	if sale.GrandTotal != "90000" || sale.VoucherDiscount != "10000" {
		t.Fatalf("sale with voucher %+v", sale)
	}
	quoteErr(2, "TENK", 409, "VOUCHER_USED_UP")
	tn.Owner.mustOK("POST", "/pos/sales/"+sale.ID+"/void", map[string]any{"reason": "oops"}, nil)
	if q := s.quote(t, cart(2, "TENK")); q["voucher_discount"] != "10000" {
		t.Fatalf("voided sale must release the voucher: %v", q["voucher_discount"])
	}
}

func TestVoucherCannotBeOverRedeemedConcurrently(t *testing.T) {
	tn := newTenant(t)
	s := newShop(t, tn, "ST-A")
	it := s.item(t, "SKU-A", "50000")
	tn.Owner.mustOK("POST", "/vouchers", map[string]any{"code": "ONLYONE", "kind": "fixed", "value": 5000, "max_uses": 1}, nil)
	var ok, used, other int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, env := tn.Owner.do("POST", "/pos/sales", map[string]any{"terminal_id": s.term, "client_txn_id": uuid.NewString(), "voucher_code": "ONLYONE",
				"items": []map[string]any{lineItem(it, 1)}, "payments": []map[string]any{cashPay(50000)}})
			switch {
			case st == 201:
				atomic.AddInt32(&ok, 1)
			case env.Error != nil && env.Error.Code == "VOUCHER_USED_UP":
				atomic.AddInt32(&used, 1)
			default:
				atomic.AddInt32(&other, 1)
				t.Errorf("unexpected %d %+v", st, env.Error)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || used != 11 || other != 0 {
		t.Fatalf("ok=%d used=%d other=%d", ok, used, other)
	}
	var n int
	_ = testPool.QueryRow(context.Background(), `select used_count from vouchers where company_id=$1 and code='ONLYONE'`, uuid.MustParse(tn.CompanyID)).Scan(&n)
	var sales int
	_ = testPool.QueryRow(context.Background(), `select count(*) from sales where company_id=$1`, uuid.MustParse(tn.CompanyID)).Scan(&sales)
	if n != 1 || sales != 1 {
		t.Fatalf("used_count=%d sales=%d (failed attempts must roll back)", n, sales)
	}
}

func TestLoyaltyEarnRedeemVoidReturnAndLevels(t *testing.T) {
	tn := newTenant(t)
	s := newShop(t, tn, "ST-A")
	it := s.item(t, "SKU-A", "50000")
	lv := tn.levels(t)
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "loyalty.min_redeem_points", "value": 10}, nil)

	c := tn.customer(t, tn.Owner, "+62 812-0000-0030")
	if c.Phone != "081200000030" || !strings.HasPrefix(c.MemberNo, "MBR-") || c.LevelName != "Regular" && c.LevelID != lv["REGULAR"].ID {
		t.Fatalf("customer %+v", c)
	}
	tn.Owner.wantErr("POST", "/customers", map[string]any{"name": "dup", "phone": "0812 0000 0030"}, 409, "PHONE_TAKEN")
	var found struct{ ID string }
	tn.Owner.mustOK("GET", "/customers/lookup?key=%2B6281200000030", nil, &found)
	if found.ID != c.ID {
		t.Fatal("lookup by differently formatted phone")
	}

	// 250000 spent -> 25 points (1 per Rp10.000, Regular x1.0)
	body := func(qty int, extra map[string]any) map[string]any {
		m := map[string]any{"items": []map[string]any{lineItem(it, qty)}, "customer_id": c.ID}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	s1 := s.sell(t, body(5, nil), cashPay(250000))
	if s1.PointsEarned != 25 || tn.getCustomer(t, c.ID).PointsBalance != 25 {
		t.Fatalf("earn: %d / %d", s1.PointsEarned, tn.getCustomer(t, c.ID).PointsBalance)
	}
	// redeem 20 points = Rp2000 off the money due; points paid are not re-earned
	var q struct {
		MaxRedeem int64 `json:"max_redeem_points"`
		Earn      int64 `json:"points_earn"`
	}
	qq := body(1, nil)
	qq["terminal_id"] = s.term
	tn.Owner.mustOK("POST", "/pos/quote", qq, &q)
	if q.MaxRedeem != 25 || q.Earn != 5 {
		t.Fatalf("quote loyalty hints: %+v", q)
	}
	s2 := s.sell(t, body(1, nil), map[string]any{"method": "POINTS", "points": 20}, cashPay(48000))
	if s2.PointsRedeemed != 20 || s2.PointsEarned != 4 { // (50000-2000)/10000 = 4
		t.Fatalf("redeem sale: redeemed=%d earned=%d", s2.PointsRedeemed, s2.PointsEarned)
	}
	if b := tn.getCustomer(t, c.ID).PointsBalance; b != 25-20+4 {
		t.Fatalf("balance after redeem: %d", b)
	}
	// guards
	over := body(1, nil)
	over["terminal_id"], over["client_txn_id"], over["payments"] = s.term, uuid.NewString(), []map[string]any{{"method": "POINTS", "points": 999}, cashPay(50000)}
	tn.Owner.wantErr("POST", "/pos/sales", over, 422, "POINTS_EXCEED_LIMIT")
	anon := map[string]any{"terminal_id": s.term, "client_txn_id": uuid.NewString(), "items": []map[string]any{lineItem(it, 1)}, "payments": []map[string]any{{"method": "POINTS", "points": 10}, cashPay(50000)}}
	tn.Owner.wantErr("POST", "/pos/sales", anon, 422, "POINTS_NEED_CUSTOMER")

	// voiding the redeem sale returns the 20 points and takes back the 4 earned
	tn.Owner.mustOK("POST", "/pos/sales/"+s2.ID+"/void", map[string]any{"reason": "mistake"}, nil)
	if b := tn.getCustomer(t, c.ID).PointsBalance; b != 25 {
		t.Fatalf("balance after void: %d", b)
	}
	// partial return of s1 (2 of 5 units) reverses 40% of the 25 earned points = 10
	tn.Owner.mustOK("POST", "/pos/sales/"+s1.ID+"/returns", map[string]any{"reason": "size", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": s1.Items[0].ID, "qty": 2}}}, nil)
	if b := tn.getCustomer(t, c.ID).PointsBalance; b != 15 {
		t.Fatalf("balance after partial return: %d", b)
	}

	// lifetime spend promotes the member (never demotes automatically): silver at Rp100.000
	tn.Owner.mustOK("PATCH", "/membership-levels/"+lv["SILVER"].ID, map[string]any{"min_spend": 100000, "earn_multiplier": 2}, nil)
	s3 := s.sell(t, body(2, nil), cashPay(100000))
	cur := tn.getCustomer(t, c.ID)
	if cur.LevelName != "Silver" {
		t.Fatalf("expected promotion to Silver, got %s (spend %s)", cur.LevelName, cur.LifetimeSpend)
	}
	s4 := s.sell(t, body(2, nil), cashPay(100000)) // now earns at x2: 10 base -> 20
	if s4.PointsEarned != 20 {
		t.Fatalf("silver multiplier: %d (s3 earned %d)", s4.PointsEarned, s3.PointsEarned)
	}
	// ledger always equals the cached balance
	var sum, bal int64
	_ = testPool.QueryRow(context.Background(), `select coalesce(sum(points),0) from loyalty_transactions where customer_id=$1`, uuid.MustParse(c.ID)).Scan(&sum)
	_ = testPool.QueryRow(context.Background(), `select points_balance from customers where id=$1`, uuid.MustParse(c.ID)).Scan(&bal)
	if sum != bal {
		t.Fatalf("ledger %d != balance %d", sum, bal)
	}

	// manual adjustments need customer.manage and can't go negative
	cashier := tn.user(t, "cashier", s.st.Store.OrgUnitID)
	cashier.wantErr("POST", "/customers/"+c.ID+"/points", map[string]any{"points": 5, "note": "x"}, 403, "FORBIDDEN")
	tn.Owner.wantErr("POST", "/customers/"+c.ID+"/points", map[string]any{"points": -100000, "note": "too much"}, 409, "INSUFFICIENT_POINTS")
	tn.Owner.mustOK("POST", "/customers/"+c.ID+"/points", map[string]any{"points": 50, "note": "goodwill"}, nil)
	// ...but cashiers can register members
	cashier.mustOK("POST", "/customers", map[string]any{"name": "Baru", "phone": "081200000031"}, nil)

	// receipt shows member and points
	rc := receiptText(t, tn.Owner, s4.ID)
	if !strings.Contains(rc, "Poin didapat") || !strings.Contains(rc, c.MemberNo) {
		t.Fatalf("receipt:\n%s", rc)
	}
}

func TestPointsExpiry(t *testing.T) {
	tn := newTenant(t)
	s := newShop(t, tn, "ST-A")
	it := s.item(t, "SKU-A", "50000")
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "loyalty.min_redeem_points", "value": 5}, nil)
	c := tn.customer(t, tn.Owner, "081200000040")
	body := map[string]any{"items": []map[string]any{lineItem(it, 5)}, "customer_id": c.ID}
	s.sell(t, body, cashPay(250000))                                                                                                                             // +25
	s.sell(t, map[string]any{"items": []map[string]any{lineItem(it, 1)}, "customer_id": c.ID}, map[string]any{"method": "POINTS", "points": 10}, cashPay(49000)) // -10, +4
	before := tn.getCustomer(t, c.ID).PointsBalance
	mgr := tx.NewManager(testPool)
	if n, err := customer.ExpirePoints(context.Background(), mgr, time.Now()); err != nil || n != 0 {
		t.Fatalf("nothing is old enough yet: n=%d err=%v", n, err)
	}
	later := time.Now().AddDate(0, 0, 400)
	n, err := customer.ExpirePoints(context.Background(), mgr, later)
	if err != nil {
		t.Fatal(err)
	}
	after := tn.getCustomer(t, c.ID).PointsBalance
	// earned 25+4, spent 10 -> 19 expire; balance was 19
	if n < 1 || after != 0 || before != 19 { // the job is global, so other tenants' customers may expire too
		t.Fatalf("expiry: customers=%d before=%d after=%d", n, before, after)
	}
	if n, _ := customer.ExpirePoints(context.Background(), mgr, later); n != 0 {
		t.Fatalf("expiry must be idempotent, expired again for %d customers", n)
	}
}
