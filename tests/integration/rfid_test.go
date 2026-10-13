package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/internal/rfid"
)

var epcSeq atomic.Int64

func newEPCs(n int) []string {
	out := make([]string, n)
	base := time.Now().UnixNano() % 1_000_000_000_000
	for i := range out {
		out[i] = fmt.Sprintf("E280%012X%08X", base, epcSeq.Add(1))
	}
	return out
}

// rfidProduct creates an RFID-tracked product and returns its variant + barcode.
func (tn *tenant) rfidProduct(t *testing.T, code, price string) item {
	t.Helper()
	var uoms []struct{ ID, Code string }
	tn.Owner.mustOK("GET", "/uoms", nil, &uoms)
	var pcs string
	for _, u := range uoms {
		if u.Code == "PCS" {
			pcs = u.ID
		}
	}
	bc := ean13(seq.Add(1) + 500000)
	var p struct {
		Variants []struct {
			ID string `json:"id"`
		} `json:"variants"`
	}
	tn.Owner.mustOK("POST", "/products", map[string]any{"code": code, "name": "RFID " + code, "uom_id": pcs, "is_rfid_tracked": true, "variants": []map[string]any{{
		"sku": code, "purchase_price": "10000", "selling_price": price, "barcodes": []map[string]any{{"barcode": bc}}}}}, &p)
	return item{VariantID: p.Variants[0].ID, Barcode: bc}
}

func (tn *tenant) registerTags(t *testing.T, c *client, it item, store storeOut, status string, epcs []string) {
	t.Helper()
	var res struct {
		Created    int      `json:"created"`
		Duplicates []string `json:"duplicates"`
	}
	c.mustOK("POST", "/rfid/tags/register", map[string]any{"variant_id": it.VariantID, "store_id": store.Store.ID, "status": status, "epcs": epcs}, &res)
	if res.Created != len(epcs) {
		t.Fatalf("registered %d of %d (dups %v)", res.Created, len(epcs), res.Duplicates)
	}
}

type tagRow struct {
	EPC    string `json:"epc"`
	Status string `json:"status"`
}

func tagStatuses(t *testing.T, c *client, store storeOut, q string) map[string]string {
	t.Helper()
	var rows []tagRow
	c.mustOK("GET", "/rfid/tags?per_page=200&store_id="+store.Store.ID+q, nil, &rows)
	m := map[string]string{}
	for _, r := range rows {
		m[r.EPC] = r.Status
	}
	return m
}

type device struct {
	t     *testing.T
	token string
}

func (d *device) do(method, path string, body any) (int, envelope) {
	d.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, testSrv.URL+"/api/v1/edge"+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	var env envelope
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env
}

func (d *device) mustOK(method, path string, body any, into any) {
	d.t.Helper()
	st, env := d.do(method, path, body)
	if st >= 300 {
		d.t.Fatalf("%s %s -> %d %+v", method, path, st, env.Error)
	}
	if into != nil {
		if err := json.Unmarshal(env.Data, into); err != nil {
			d.t.Fatalf("decode: %v (%s)", err, env.Data)
		}
	}
}

type readerOut struct {
	Reader struct {
		ID        string `json:"id"`
		Code      string `json:"code"`
		TokenHash string `json:"token_hash"`
		TokenHint string `json:"token_hint"`
	} `json:"reader"`
	Token string `json:"token"`
}

func (tn *tenant) reader(t *testing.T, c *client, store storeOut, code, kind string) (*device, readerOut) {
	t.Helper()
	var r readerOut
	c.mustOK("POST", "/rfid/readers", map[string]any{"store_id": store.Store.ID, "code": code, "name": "Reader " + code, "kind": kind}, &r)
	return &device{t: t, token: r.Token}, r
}

type gateEvent struct {
	ClientEventID string `json:"client_event_id"`
	EPC           string `json:"epc"`
	ReadAt        string `json:"read_at"`
	Direction     string `json:"direction,omitempty"`
	LocalDecision string `json:"local_decision,omitempty"`
}

func ev(epc string, at time.Time) gateEvent {
	return gateEvent{ClientEventID: uuid.NewString(), EPC: epc, ReadAt: at.UTC().Format(time.RFC3339Nano), Direction: "out"}
}

type evResult struct {
	ClientEventID   string `json:"client_event_id"`
	Decision        string `json:"decision"`
	Reason          string `json:"reason"`
	Duplicate       bool   `json:"duplicate"`
	SecurityEventID string `json:"security_event_id"`
}

func (d *device) send(events ...gateEvent) []evResult {
	d.t.Helper()
	var out struct {
		Results []evResult `json:"results"`
	}
	d.mustOK("POST", "/events", map[string]any{"events": events, "agent_version": "test"}, &out)
	return out.Results
}

type secRow struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Title    string `json:"title"`
	EPC      string `json:"epc"`
	Store    string `json:"store_name"`
}

func secEvents(t *testing.T, c *client, q string) []secRow {
	t.Helper()
	var rows []secRow
	c.mustOK("GET", "/security/events?per_page=100"+q, nil, &rows)
	return rows
}

// ---------------------------------------------------------------- tests

func TestTagRegistrationAndStatusMachine(t *testing.T) {
	tn := newTenant(t)
	a := tn.store(t, "RA-"+uniq("s"), "")
	b := tn.store(t, "RB-"+uniq("s"), "")
	plain := tn.product(t, uniq("PL"), "10000")
	tracked := tn.rfidProduct(t, uniq("RT"), "50000")
	mgrA := tn.user(t, "store_manager", a.Store.OrgUnitID)
	cashier := tn.user(t, "cashier", a.Store.OrgUnitID)

	// only products flagged RFID-tracked take tags
	tn.Owner.wantErr("POST", "/rfid/tags/register", map[string]any{"variant_id": plain.VariantID, "store_id": a.Store.ID, "epcs": newEPCs(1)}, 409, "PRODUCT_NOT_RFID_TRACKED")
	tn.Owner.wantErr("POST", "/rfid/tags/register", map[string]any{"variant_id": tracked.VariantID, "store_id": a.Store.ID, "epcs": []string{"not-hex"}}, 422, "VALIDATION_FAILED")

	epcs := newEPCs(5)
	tn.registerTags(t, tn.Owner, tracked, a, "IN_STOCK", epcs)
	// registering again reports duplicates and leaves the originals alone
	var dup struct {
		Created    int      `json:"created"`
		Duplicates []string `json:"duplicates"`
	}
	tn.Owner.mustOK("POST", "/rfid/tags/register", map[string]any{"variant_id": tracked.VariantID, "store_id": a.Store.ID, "epcs": []string{strings.ToLower(epcs[0]), newEPCs(1)[0]}}, &dup)
	if dup.Created != 1 || len(dup.Duplicates) != 1 || dup.Duplicates[0] != epcs[0] {
		t.Fatalf("duplicates %+v", dup)
	}

	// permissions and scope
	cashier.wantErr("POST", "/rfid/tags/register", map[string]any{"variant_id": tracked.VariantID, "store_id": a.Store.ID, "epcs": newEPCs(1)}, 403, "FORBIDDEN")
	mgrA.wantErr("POST", "/rfid/tags/register", map[string]any{"variant_id": tracked.VariantID, "store_id": b.Store.ID, "epcs": newEPCs(1)}, 403, "FORBIDDEN")
	mgrA.wantErr("POST", "/rfid/tags/register", map[string]any{"variant_id": tracked.VariantID, "epcs": newEPCs(1)}, 403, "FORBIDDEN") // central needs company-wide

	// legal changes; LOST/DAMAGED need a reason
	var chg struct{ Changed int }
	mgrA.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": epcs[:2], "status": "ON_SHELF"}, &chg)
	if chg.Changed != 2 {
		t.Fatalf("changed %d", chg.Changed)
	}
	mgrA.wantErr("POST", "/rfid/tags/status", map[string]any{"epcs": epcs[2:3], "status": "LOST"}, 422, "VALIDATION_FAILED")
	mgrA.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": epcs[2:3], "status": "LOST", "reason": "not found in cycle count"}, nil)
	mgrA.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": epcs[2:3], "status": "ON_SHELF"}, nil) // found again
	// all-or-nothing: one illegal tag blocks the whole batch
	mgrA.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": epcs[3:4], "status": "DAMAGED", "reason": "water damage"}, nil)
	st, env := mgrA.do("POST", "/rfid/tags/status", map[string]any{"epcs": []string{epcs[0], epcs[3]}, "status": "IN_STOCK"})
	if st != 409 || env.Error == nil || env.Error.Code != "TAG_CHANGE_REJECTED" {
		t.Fatalf("batch with a damaged tag: %d %+v", st, env.Error)
	}
	if got := tagStatuses(t, mgrA, a, "")[epcs[0]]; got != "ON_SHELF" {
		t.Fatalf("rejected batch changed a tag: %s", got)
	}
	// SOLD can never be set by hand
	tn.Owner.wantErr("POST", "/rfid/tags/status", map[string]any{"epcs": epcs[:1], "status": "SOLD"}, 422, "VALIDATION_FAILED")

	// manager A cannot see or touch what store B holds
	bEPC := newEPCs(1)
	tn.registerTags(t, tn.Owner, tracked, b, "IN_STOCK", bEPC)
	mgrA.wantErr("POST", "/rfid/tags/status", map[string]any{"epcs": bEPC, "status": "ON_SHELF"}, 409, "TAG_CHANGE_REJECTED")
	mgrA.wantErr("GET", "/rfid/tags/"+bEPC[0], nil, 404, "NOT_FOUND")
	var seen []tagRow
	mgrA.mustOK("GET", "/rfid/tags?per_page=200", nil, &seen)
	for _, r := range seen {
		if r.EPC == bEPC[0] {
			t.Fatal("store B tag leaked into manager A's list")
		}
	}
	// moving needs rights on the destination too
	mgrA.wantErr("POST", "/rfid/tags/move", map[string]any{"epcs": epcs[:1], "store_id": b.Store.ID}, 403, "FORBIDDEN")
	var mv struct{ Moved int }
	tn.Owner.mustOK("POST", "/rfid/tags/move", map[string]any{"epcs": epcs[:1], "store_id": b.Store.ID, "location": "dock"}, &mv)
	if mv.Moved != 1 {
		t.Fatalf("moved %d", mv.Moved)
	}
	var detail struct {
		Status    string `json:"status"`
		StoreName string `json:"store_name"`
	}
	tn.Owner.mustOK("GET", "/rfid/tags/"+epcs[0], nil, &detail)
	if detail.StoreName != "Toko "+b.Store.Code {
		t.Fatalf("tag detail after move: %+v", detail)
	}
}

func TestRFIDSaleVoidReturnAndRace(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "RS-"+uniq("s"), "")
	other := tn.store(t, "RO-"+uniq("s"), "")
	it := tn.rfidProduct(t, uniq("RT"), "50000")
	tn.stock(t, st, it, 20, "20000")
	tags := newEPCs(8)
	tn.registerTags(t, tn.Owner, it, st, "ON_SHELF", tags)
	foreign := newEPCs(1)
	tn.registerTags(t, tn.Owner, it, other, "ON_SHELF", foreign)
	reserved := newEPCs(1)
	tn.registerTags(t, tn.Owner, it, st, "IN_STOCK", reserved)
	tn.Owner.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": reserved, "status": "RESERVED"}, nil)

	term := terminalOf(t, tn.Owner, st.Store.ID)
	sup := tn.user(t, "supervisor", st.Store.OrgUnitID)
	_ = sup
	var supEmail string
	_ = testPool.QueryRow(context.Background(), `select email from users where company_id=$1 and name='supervisor'`, uuid.MustParse(tn.CompanyID)).Scan(&supEmail)
	cashier := tn.user(t, "cashier", st.Store.OrgUnitID)
	openShift(t, cashier, term, 0)
	cash := func(n int) []map[string]any { return []map[string]any{{"method": "CASH", "amount": n}} }
	epcLine := func(e string) map[string]any { return map[string]any{"epc": e} }

	// scanning rules
	cashier.wantErr("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(newEPCs(1)[0])}}, 404, "UNKNOWN_TAG")
	cashier.wantErr("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(foreign[0])}}, 409, "TAG_NOT_SELLABLE")
	cashier.wantErr("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(reserved[0])}}, 409, "TAG_NOT_SELLABLE")
	cashier.wantErr("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(tags[0]), epcLine(strings.ToLower(tags[0]))}}, 409, "DUPLICATE_TAG")
	var q struct {
		GrandTotal string `json:"grand_total"`
		Lines      []struct {
			EPC string `json:"epc"`
			Qty string `json:"qty"`
		} `json:"lines"`
	}
	cashier.mustOK("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{{"epc": tags[0], "qty": 9}, epcLine(tags[1])}}, &q)
	if q.GrandTotal != "100000" || len(q.Lines) != 2 || q.Lines[0].Qty != "1" || q.Lines[0].EPC != tags[0] {
		t.Fatalf("epc quote (qty must be forced to 1): %+v", q)
	}

	// sale: both tags become SOLD in the same transaction
	var s1 saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{epcLine(tags[0]), epcLine(tags[1])}, cash(100000)), &s1)
	got := tagStatuses(t, tn.Owner, st, "")
	if got[tags[0]] != "SOLD" || got[tags[1]] != "SOLD" || got[tags[2]] != "ON_SHELF" {
		t.Fatalf("after sale: %v", got)
	}
	if tn.onHand(t, st, it) != "18" {
		t.Fatalf("stock after tagged sale: %s", tn.onHand(t, st, it))
	}
	cashier.wantErr("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(tags[0])}}, 409, "TAG_NOT_SELLABLE") // already sold

	// void puts exactly those tags back on the shelf
	cashier.mustOK("POST", "/pos/sales/"+s1.ID+"/void", map[string]any{"reason": "salah", "approval": map[string]any{"approver_email": supEmail, "pin": "1234"}}, nil)
	got = tagStatuses(t, tn.Owner, st, "")
	if got[tags[0]] != "ON_SHELF" || got[tags[1]] != "ON_SHELF" {
		t.Fatalf("after void: %v", got)
	}

	// sell 3 again, then return one (restock → RETURNED, sellable again) and one defective (→ DAMAGED)
	var s2 saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{epcLine(tags[2]), epcLine(tags[3]), epcLine(tags[4])}, cash(150000)), &s2)
	if len(s2.Items) != 3 {
		t.Fatalf("items %+v", s2.Items)
	}
	cashier.mustOK("POST", "/pos/sales/"+s2.ID+"/returns", map[string]any{"reason": "ukuran", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": s2.Items[0].ID, "qty": 1}}}, nil)
	cashier.mustOK("POST", "/pos/sales/"+s2.ID+"/returns", map[string]any{"reason": "cacat", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": s2.Items[1].ID, "qty": 1, "restock": false}}}, nil)
	got = tagStatuses(t, tn.Owner, st, "")
	counts := map[string]int{}
	for _, e := range tags[2:5] {
		counts[got[e]]++
	}
	if counts["RETURNED"] != 1 || counts["DAMAGED"] != 1 || counts["SOLD"] != 1 {
		t.Fatalf("after returns: %v", got)
	}
	// the returned tag is sellable again; the damaged one is not
	var returned, damaged string
	for _, e := range tags[2:5] {
		switch got[e] {
		case "RETURNED":
			returned = e
		case "DAMAGED":
			damaged = e
		}
	}
	cashier.wantErr("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(damaged)}}, 409, "TAG_NOT_SELLABLE")
	cashier.mustOK("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(returned)}}, nil)

	// race: 10 terminals' worth of checkouts for the same tag → exactly one wins
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	target := tags[6]
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, env := cashier.do("POST", "/pos/sales", checkoutBody(term, []map[string]any{epcLine(target)}, cash(50000)))
			switch {
			case code == 201:
				wins.Add(1)
			case env.Error != nil && env.Error.Code == "TAG_NOT_SELLABLE":
				conflicts.Add(1)
			default:
				t.Errorf("unexpected %d %+v", code, env.Error)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 9 {
		t.Fatalf("race: wins=%d conflicts=%d", wins.Load(), conflicts.Load())
	}
	// stock: 20 → 18 (s1) → 20 (void) → 17 (s2) → 18 (restocked return) → 18 (defective return) → 17 (race winner)
	if tn.onHand(t, st, it) != "17" {
		t.Fatalf("stock after race: %s", tn.onHand(t, st, it))
	}

	// require_tag_at_pos: tracked products must then be sold by tag
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "rfid.require_tag_at_pos", "value": true}, nil)
	cashier.wantErr("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{{"barcode": it.Barcode, "qty": 1}}}, 409, "TAG_REQUIRED")
	cashier.mustOK("POST", "/pos/quote", map[string]any{"terminal_id": term, "items": []map[string]any{epcLine(tags[7])}}, nil)
}

func TestReaderEnrollmentAndDeviceAuth(t *testing.T) {
	tn := newTenant(t)
	a := tn.store(t, "DA-"+uniq("s"), "")
	b := tn.store(t, "DB-"+uniq("s"), "")
	mgrA := tn.user(t, "store_manager", a.Store.OrgUnitID)
	cashier := tn.user(t, "cashier", a.Store.OrgUnitID)

	cashier.wantErr("POST", "/rfid/readers", map[string]any{"store_id": a.Store.ID, "code": "GATE1", "name": "x", "kind": "gate"}, 403, "FORBIDDEN")
	mgrA.wantErr("POST", "/rfid/readers", map[string]any{"store_id": b.Store.ID, "code": "GATE1", "name": "x", "kind": "gate"}, 403, "FORBIDDEN")
	mgrA.wantErr("POST", "/rfid/readers", map[string]any{"store_id": a.Store.ID, "code": "x", "name": "x", "kind": "gate"}, 422, "VALIDATION_FAILED")
	dev, r := tn.reader(t, mgrA, a, "gate1", "gate")
	if !strings.HasPrefix(r.Token, "rdr_") || r.Reader.Code != "GATE1" || r.Reader.TokenHash != "" || r.Reader.TokenHint != r.Token[len(r.Token)-4:] {
		t.Fatalf("enrollment: %+v", r)
	}
	mgrA.wantErr("POST", "/rfid/readers", map[string]any{"store_id": a.Store.ID, "code": "GATE1", "name": "dup", "kind": "gate"}, 409, "READER_EXISTS")
	// the token is never shown again
	var list []map[string]any
	mgrA.mustOK("GET", "/rfid/readers", nil, &list)
	if len(list) != 1 || list[0]["token_hash"] != nil || list[0]["token"] != nil {
		t.Fatalf("reader list leaks credentials: %+v", list)
	}

	// device auth
	(&device{t: t}).do("GET", "/snapshot", nil)
	if st, _ := (&device{t: t}).do("GET", "/snapshot", nil); st != 401 {
		t.Fatalf("no token: %d", st)
	}
	if st, _ := (&device{t: t, token: "rdr_nope"}).do("GET", "/snapshot", nil); st != 401 {
		t.Fatalf("bad token: %d", st)
	}
	if st, _ := (&device{t: t, token: tn.Owner.token}).do("GET", "/snapshot", nil); st != 401 {
		t.Fatalf("a user JWT must not work as a reader token: %d", st)
	}
	var hb struct {
		Rules struct {
			SoldGrace int `json:"sold_grace_seconds"`
		} `json:"rules"`
	}
	dev.mustOK("POST", "/heartbeat", map[string]any{"agent_version": "1.2.3", "buffered": 4, "local_alarms": 2, "snapshot_tags": 10, "snapshot_age_seconds": 5}, &hb)
	if hb.Rules.SoldGrace != 120 {
		t.Fatalf("rules %+v", hb)
	}
	mgrA.mustOK("GET", "/rfid/readers", nil, &list)
	if list[0]["last_seen_at"] == nil || list[0]["agent_version"] != "1.2.3" {
		t.Fatalf("heartbeat not recorded: %+v", list[0])
	}

	// disabling stops the device; re-enabling restores it; rotating kills the old token
	mgrA.mustOK("PATCH", "/rfid/readers/"+r.Reader.ID, map[string]any{"status": "inactive"}, nil)
	if st, _ := dev.do("POST", "/heartbeat", map[string]any{}); st != 403 {
		t.Fatalf("inactive reader: %d", st)
	}
	mgrA.mustOK("PATCH", "/rfid/readers/"+r.Reader.ID, map[string]any{"status": "active"}, nil)
	var rot struct{ Token string }
	mgrA.mustOK("POST", "/rfid/readers/"+r.Reader.ID+"/rotate-token", nil, &rot)
	if st, _ := dev.do("POST", "/heartbeat", map[string]any{}); st != 401 {
		t.Fatalf("old token after rotation: %d", st)
	}
	if st, _ := (&device{t: t, token: rot.Token}).do("POST", "/heartbeat", map[string]any{}); st != 200 {
		t.Fatalf("new token: %d", st)
	}
	var audits int
	_ = testPool.QueryRow(context.Background(), `select count(*) from audit_logs where company_id=$1 and action in ('RFID_READER_ENROLLED','RFID_READER_TOKEN_ROTATED')`, uuid.MustParse(tn.CompanyID)).Scan(&audits)
	if audits != 2 {
		t.Fatalf("audit rows %d", audits)
	}
}

func TestGateDecisionsAlarmsAndNotifications(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "GT-"+uniq("s"), "")
	other := tn.store(t, "GO-"+uniq("s"), "")
	it := tn.rfidProduct(t, uniq("RT"), "50000")
	tn.stock(t, st, it, 10, "20000")
	tags := newEPCs(6)
	tn.registerTags(t, tn.Owner, it, st, "ON_SHELF", tags)
	foreign := newEPCs(1)
	tn.registerTags(t, tn.Owner, it, other, "ON_SHELF", foreign)
	mgr := tn.user(t, "store_manager", st.Store.OrgUnitID)
	mgrOther := tn.user(t, "store_manager", other.Store.OrgUnitID)
	cashier := tn.user(t, "cashier", st.Store.OrgUnitID)
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, cashier, term, 0)
	var sold saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"epc": tags[0]}}, []map[string]any{{"method": "CASH", "amount": 50000}}), &sold)
	tn.Owner.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": tags[1:2], "status": "LOST", "reason": "hilang"}, nil)

	gate, _ := tn.reader(t, mgr, st, "GATE-1", "gate")
	shelf, _ := tn.reader(t, mgr, st, "SHELF-1", "shelf")
	now := time.Now().Truncate(time.Minute).Add(5 * time.Second) // inside one dedupe minute

	res := gate.send(
		ev(tags[0], now),       // paid → allow
		ev(tags[2], now),       // unpaid on shelf → alarm
		ev(tags[1], now),       // lost tag leaving → mismatch
		ev(foreign[0], now),    // another store's item → mismatch
		ev(newEPCs(1)[0], now), // never seen → allowed (not reported by default)
		gateEvent{ClientEventID: uuid.NewString(), EPC: tags[3], ReadAt: now.Format(time.RFC3339Nano), Direction: "in"}, // walking in → ignored
	)
	decisions := make([]string, len(res))
	for i, r := range res {
		decisions[i] = r.Decision
	}
	// results come back in read-time order; with identical read times keep the submitted order
	want := []string{"allow", "alarm", "alarm", "alarm", "allow", "ignored"}
	for i := range want {
		if decisions[i] != want[i] {
			t.Fatalf("decisions %v want %v (%+v)", decisions, want, res)
		}
	}
	evs := secEvents(t, mgr, "")
	types := map[string]int{}
	for _, e := range evs {
		types[e.Type]++
	}
	if len(evs) != 3 || types["UNAUTHORIZED_EXIT"] != 1 || types["RFID_MISMATCH"] != 2 {
		t.Fatalf("security events: %+v", evs)
	}
	for _, e := range evs {
		if e.Type == "UNAUTHORIZED_EXIT" && (e.Severity != "critical" || e.EPC != tags[2] || !strings.Contains(e.Title, "RFID")) {
			t.Fatalf("unauthorized exit event: %+v", e)
		}
	}
	// the other store's manager sees nothing of this
	if n := len(secEvents(t, mgrOther, "")); n != 0 {
		t.Fatalf("other store sees %d events", n)
	}
	cashier.wantErr("GET", "/security/events", nil, 403, "FORBIDDEN")

	// notifications went to security.view holders in scope (manager + owner), as critical alerts
	settle(t)
	var notes []struct {
		Type, Severity, Title string
	}
	mgr.mustOK("GET", "/notifications", nil, &notes)
	alerts := 0
	for _, n := range notes {
		if n.Type == "security_alert" {
			alerts++
		}
	}
	if alerts != 3 {
		t.Fatalf("manager alerts: %d (%+v)", alerts, notes)
	}
	var others []struct{ Type string }
	mgrOther.mustOK("GET", "/notifications", nil, &others)
	for _, n := range others {
		if n.Type == "security_alert" {
			t.Fatal("alert leaked to another store's manager")
		}
	}

	// idempotent replay: same client_event_id → duplicate, no second security event
	again := gate.send(gateEvent{ClientEventID: res[1].ClientEventID, EPC: tags[2], ReadAt: now.UTC().Format(time.RFC3339Nano), Direction: "out"})
	if !again[0].Duplicate {
		t.Fatalf("replay not flagged: %+v", again)
	}
	// the same tag read again within the minute (loitering at the gate) is one alarm, not a stream
	gate.send(ev(tags[2], now.Add(10*time.Second)))
	if n := len(secEvents(t, mgr, "&type=UNAUTHORIZED_EXIT")); n != 1 {
		t.Fatalf("loitering produced %d alarms", n)
	}
	// a read more than a minute later is a new incident
	gate.send(ev(tags[2], now.Add(3*time.Minute)))
	if n := len(secEvents(t, mgr, "&type=UNAUTHORIZED_EXIT")); n != 2 {
		t.Fatalf("later read should alarm again, got %d", n)
	}
	// the edge said "allow" (stale snapshot) but the server knows better → flagged as a local miss
	gate.send(gateEvent{ClientEventID: uuid.NewString(), EPC: tags[4], ReadAt: now.Add(5 * time.Minute).Format(time.RFC3339Nano), Direction: "out", LocalDecision: "allow"})
	var detail struct {
		Detail map[string]any `json:"detail"`
	}
	for _, e := range secEvents(t, mgr, "&type=UNAUTHORIZED_EXIT") {
		if e.EPC == tags[4] {
			mgr.mustOK("GET", "/security/events/"+e.ID, nil, &detail)
		}
	}
	if detail.Detail["local_missed"] != true {
		t.Fatalf("local_missed not recorded: %+v", detail)
	}

	// non-gate readers never raise alarms
	r2 := shelf.send(ev(tags[5], now))
	if r2[0].Decision != "ignored" {
		t.Fatalf("shelf reader decision: %+v", r2)
	}
	var seen *string
	_ = testPool.QueryRow(context.Background(), `select last_seen_at::text from rfid_tags where epc=$1`, tags[5]).Scan(&seen)
	if seen == nil {
		t.Fatal("tag last_seen_at not updated by the shelf read")
	}

	// opting in to unknown-tag reports; a reader with a wrong clock cannot post-date its reads
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "rfid.report_unknown_tags", "value": true}, nil)
	gate.send(ev(newEPCs(1)[0], now))
	if n := len(secEvents(t, mgr, "&type=UNKNOWN_TAG")); n != 1 {
		t.Fatalf("unknown tag events: %d", n)
	}
	future := gate.send(ev(tags[3], now.Add(48*time.Hour)))
	if future[0].Decision != "alarm" { // clamped to "now": still an unpaid exit rather than silently ignored
		t.Fatalf("future-dated read: %+v", future)
	}
	// readers of one store cannot post for another: the device only ever acts for its own store
	var evStore string
	_ = testPool.QueryRow(context.Background(), `select store_id::text from rfid_events where epc=$1 limit 1`, tags[2]).Scan(&evStore)
	if evStore != st.Store.ID {
		t.Fatalf("event stored for store %s", evStore)
	}
}

func TestSnapshotDeltaIsPerStoreAndPaged(t *testing.T) {
	tn := newTenant(t)
	a := tn.store(t, "SA-"+uniq("s"), "")
	b := tn.store(t, "SB-"+uniq("s"), "")
	it := tn.rfidProduct(t, uniq("RT"), "10000")
	ta, tb := newEPCs(7), newEPCs(3)
	tn.registerTags(t, tn.Owner, it, a, "IN_STOCK", ta)
	tn.registerTags(t, tn.Owner, it, b, "IN_STOCK", tb)
	devA, _ := tn.reader(t, tn.Owner, a, "GATE", "gate")

	type snap struct {
		ServerTime string `json:"server_time"`
		Tags       []struct {
			EPC       string `json:"epc"`
			Status    string `json:"status"`
			UpdatedAt string `json:"updated_at"`
		} `json:"tags"`
		More      bool   `json:"more"`
		NextSince string `json:"next_since"`
		NextAfter string `json:"next_after"`
		Rules     struct {
			Report bool `json:"report_unknown_tags"`
		} `json:"rules"`
	}
	var s snap
	devA.mustOK("GET", "/snapshot", nil, &s)
	if len(s.Tags) != 7 || s.More {
		t.Fatalf("full snapshot: %d tags more=%v", len(s.Tags), s.More)
	}
	for _, tg := range s.Tags {
		for _, e := range tb {
			if tg.EPC == e {
				t.Fatal("snapshot leaked another store's tag")
			}
		}
	}
	// paging: limit 3 → more=true and next_since moves forward; the union covers everything
	got := map[string]bool{}
	since, after := "", ""
	for i := 0; i < 20; i++ {
		var p snap
		path := "/snapshot?limit=3"
		if since != "" {
			path += "&since=" + since + "&after=" + after
		}
		devA.mustOK("GET", path, nil, &p)
		for _, tg := range p.Tags {
			got[tg.EPC] = true
		}
		if !p.More {
			break
		}
		since, after = strings.ReplaceAll(p.NextSince, "+", "%2B"), p.NextAfter
	}
	if len(got) != 7 {
		t.Fatalf("paged snapshot covered %d of 7", len(got))
	}
	// delta: a later change shows up from the previous server_time on
	time.Sleep(20 * time.Millisecond)
	tn.Owner.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": ta[:2], "status": "ON_SHELF"}, nil)
	var d snap
	devA.mustOK("GET", "/snapshot?since="+strings.ReplaceAll(s.ServerTime, "+", "%2B"), nil, &d)
	changed := map[string]string{}
	for _, tg := range d.Tags {
		changed[tg.EPC] = tg.Status
	}
	if len(changed) != 2 || changed[ta[0]] != "ON_SHELF" {
		t.Fatalf("delta %+v", changed)
	}
	devA.mustOK("GET", "/snapshot?since="+strings.ReplaceAll(d.ServerTime, "+", "%2B"), nil, &d)
	if len(d.Tags) != 0 {
		t.Fatalf("nothing changed since, got %d", len(d.Tags))
	}
}

func TestSecurityWorkflowOverviewAndRules(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "SW-"+uniq("s"), "")
	other := tn.store(t, "SX-"+uniq("s"), "")
	it := tn.rfidProduct(t, uniq("RT"), "100000")
	tn.stock(t, st, it, 20, "20000")
	tags := newEPCs(3)
	tn.registerTags(t, tn.Owner, it, st, "ON_SHELF", tags)
	mgr := tn.user(t, "store_manager", st.Store.OrgUnitID)
	mgrOther := tn.user(t, "store_manager", other.Store.OrgUnitID)
	lp := tn.user(t, "loss_prevention", "")
	gate, _ := tn.reader(t, mgr, st, "GATE", "gate")
	now := time.Now().Truncate(time.Minute).Add(5 * time.Second) // inside one dedupe minute
	gate.send(ev(tags[0], now), ev(tags[1], now))
	evs := secEvents(t, lp, "")
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	id := evs[0].ID

	// workflow: acknowledge → resolve needs a note → closed events cannot be reopened
	mgrOther.wantErr("POST", "/security/events/"+id+"/status", map[string]any{"status": "acknowledged"}, 403, "FORBIDDEN") // no security.resolve
	lp.mustOK("POST", "/security/events/"+id+"/status", map[string]any{"status": "acknowledged"}, nil)
	lp.wantErr("POST", "/security/events/"+id+"/status", map[string]any{"status": "acknowledged"}, 409, "ALREADY_ACKNOWLEDGED")
	lp.wantErr("POST", "/security/events/"+id+"/status", map[string]any{"status": "resolved"}, 422, "VALIDATION_FAILED")
	var done struct {
		Status     string `json:"status"`
		ResolvedBy string `json:"resolved_by_name"`
		Note       string `json:"resolution_note"`
	}
	lp.mustOK("POST", "/security/events/"+id+"/status", map[string]any{"status": "false_alarm", "note": "staff carried stock to the back room"}, &done)
	if done.Status != "false_alarm" || done.ResolvedBy == "" || !strings.Contains(done.Note, "back room") {
		t.Fatalf("closed event %+v", done)
	}
	lp.wantErr("POST", "/security/events/"+id+"/status", map[string]any{"status": "acknowledged"}, 409, "ALREADY_CLOSED")
	if n := len(secEvents(t, lp, "&status=active")); n != 1 {
		t.Fatalf("active events %d", n)
	}
	mgrOther.wantErr("GET", "/security/events/"+id, nil, 404, "NOT_FOUND")

	// loss-prevention rules from sales: a 50% manual discount (needs a supervisor PIN) is flagged
	tn.user(t, "supervisor", st.Store.OrgUnitID)
	var supEmail string
	_ = testPool.QueryRow(context.Background(), `select email from users where company_id=$1 and name='supervisor'`, uuid.MustParse(tn.CompanyID)).Scan(&supEmail)
	cashier := tn.user(t, "cashier", st.Store.OrgUnitID)
	term := terminalOf(t, tn.Owner, st.Store.ID)
	openShift(t, cashier, term, 0)
	body := checkoutBody(term, []map[string]any{{"barcode": it.Barcode, "qty": 2, "discount_percent": 50}}, []map[string]any{{"method": "CASH", "amount": 100000}})
	body["approval"] = map[string]any{"approver_email": supEmail, "pin": "1234"}
	cashier.mustOK("POST", "/pos/sales", body, nil)
	// …and a small one is not
	small := checkoutBody(term, []map[string]any{{"barcode": it.Barcode, "qty": 1, "discount_percent": 10}}, []map[string]any{{"method": "CASH", "amount": 100000}})
	cashier.mustOK("POST", "/pos/sales", small, nil)
	settle(t)
	disc := secEvents(t, lp, "&type=HIGH_DISCOUNT")
	if len(disc) != 1 || disc[0].Severity != "warning" || !strings.Contains(disc[0].Title, "50") {
		t.Fatalf("high discount events: %+v", disc)
	}
	var notes []struct{ Type string }
	mgr.mustOK("GET", "/notifications", nil, &notes)
	n := 0
	for _, x := range notes {
		if x.Type == "high_discount" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("manager high_discount notifications: %d", n)
	}

	// void/refund/shift rules also land in the loss-prevention record
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "pos.void_alert_per_day", "value": 1}, nil)
	tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": tn.RootUnit, "key": "notify.refund_amount", "value": 0}, nil)
	var s2 saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": it.Barcode, "qty": 1}}, []map[string]any{{"method": "CASH", "amount": 100000}}), &s2)
	cashier.mustOK("POST", "/pos/sales/"+s2.ID+"/void", map[string]any{"reason": "batal", "approval": map[string]any{"approver_email": supEmail, "pin": "1234"}}, nil)
	var s3 saleOut
	cashier.mustOK("POST", "/pos/sales", checkoutBody(term, []map[string]any{{"barcode": it.Barcode, "qty": 2}}, []map[string]any{{"method": "CASH", "amount": 200000}}), &s3)
	cashier.mustOK("POST", "/pos/sales/"+s3.ID+"/returns", map[string]any{"reason": "x", "refund_method": "CASH", "items": []map[string]any{{"sale_item_id": s3.Items[0].ID, "qty": 1}}}, nil)
	var cur struct {
		ID string `json:"id"`
	}
	cashier.mustOK("GET", "/pos/shifts/current", nil, &cur)
	cashier.mustOK("POST", "/pos/shifts/"+cur.ID+"/close", map[string]any{"counted_cash": 1000, "note": "kurang"}, nil)
	settle(t)
	have := map[string]int{}
	for _, e := range secEvents(t, lp, "&per_page=100") {
		have[e.Type]++
	}
	for _, typ := range []string{"EXCESSIVE_VOIDS", "LARGE_REFUND", "CASH_VARIANCE", "HIGH_DISCOUNT", "UNAUTHORIZED_EXIT"} {
		if have[typ] < 1 {
			t.Fatalf("missing %s in %v", typ, have)
		}
	}

	// overview: counts by type, per store, reader health, tag mix
	var ov struct {
		Total          int64 `json:"total"`
		Active         int64 `json:"active"`
		ActiveCritical int64 `json:"active_critical"`
		ByType         []struct {
			Type  string `json:"type"`
			Total int64  `json:"total"`
		} `json:"by_type"`
		ByDay   []struct{ Day string } `json:"by_day"`
		ByStore []struct {
			StoreName string `json:"store_name"`
			Total     int64  `json:"total"`
		} `json:"by_store"`
		Readers *struct{ Total, Online, Offline int } `json:"readers"`
		Tags    map[string]int64                      `json:"tags"`
	}
	lp.mustOK("GET", "/security/overview", nil, &ov)
	if ov.Total < 6 || ov.Active < 5 || ov.ActiveCritical < 1 || len(ov.ByType) != 8 || len(ov.ByDay) != 7 || len(ov.ByStore) != 1 {
		t.Fatalf("overview %+v", ov)
	}
	if ov.Readers == nil || ov.Readers.Total != 1 || ov.Readers.Online != 1 || ov.Tags["ON_SHELF"] != 3 {
		t.Fatalf("overview readers/tags: %+v %v", ov.Readers, ov.Tags)
	}
	lp.wantErr("GET", "/security/overview?under_path=a%3Bb", nil, 400, "BAD_REQUEST")
	if mo := mgrOther.overviewSecurity(t); mo != 0 {
		t.Fatalf("other store overview total %d", mo)
	}
}

func (c *client) overviewSecurity(t *testing.T) int64 {
	var ov struct {
		Total int64 `json:"total"`
	}
	c.mustOK("GET", "/security/overview", nil, &ov)
	return ov.Total
}

func TestReaderOfflineWatchdog(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "WD-"+uniq("s"), "")
	mgr := tn.user(t, "store_manager", st.Store.OrgUnitID)
	gate, r := tn.reader(t, mgr, st, "GATE", "gate")
	tn.reader(t, mgr, st, "SHELF", "shelf") // only gates are watched
	ctx := context.Background()
	// a gate that was just enrolled gets a grace period
	if n, err := rfid.CheckReaders(ctx, testApp.Tx, time.Now()); err != nil || n != 0 {
		t.Fatalf("fresh reader flagged: %d %v", n, err)
	}
	// it enrolled long ago and never reported → alarm, once
	_, _ = testPool.Exec(ctx, `update rfid_readers set created_at = now() - interval '1 hour' where id=$1`, uuid.MustParse(r.Reader.ID))
	for i := 0; i < 2; i++ {
		if _, err := rfid.CheckReaders(ctx, testApp.Tx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	evs := secEvents(t, mgr, "&type=READER_OFFLINE")
	if len(evs) != 1 || evs[0].Severity != "critical" {
		t.Fatalf("offline events: %+v", evs)
	}
	// it reports in (no new alarm), goes silent again later → a new incident
	gate.mustOK("POST", "/heartbeat", map[string]any{}, nil)
	if _, err := rfid.CheckReaders(ctx, testApp.Tx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := len(secEvents(t, mgr, "&type=READER_OFFLINE")); n != 1 {
		t.Fatalf("healthy reader flagged: %d", n)
	}
	_, _ = testPool.Exec(ctx, `update rfid_readers set last_seen_at = now() - interval '30 minutes' where id=$1`, uuid.MustParse(r.Reader.ID))
	if _, err := rfid.CheckReaders(ctx, testApp.Tx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := len(secEvents(t, mgr, "&type=READER_OFFLINE")); n != 2 {
		t.Fatalf("second outage should alarm again: %d", n)
	}
	var ov struct {
		Readers struct{ Total, Online, Offline int } `json:"readers"`
	}
	mgr.mustOK("GET", "/security/overview", nil, &ov)
	if ov.Readers.Total != 2 || ov.Readers.Offline != 2 && ov.Readers.Offline != 1 {
		t.Fatalf("health %+v", ov.Readers)
	}
}

func TestRFIDStockCountAndVariance(t *testing.T) {
	tn := newTenant(t)
	st := tn.store(t, "SC-"+uniq("s"), "")
	other := tn.store(t, "SD-"+uniq("s"), "")
	it := tn.rfidProduct(t, uniq("RT"), "10000")
	tn.stock(t, st, it, 6, "1000")
	tags := newEPCs(6)
	tn.registerTags(t, tn.Owner, it, st, "ON_SHELF", tags)
	foreign := newEPCs(1)
	tn.registerTags(t, tn.Owner, it, other, "ON_SHELF", foreign)
	tn.Owner.mustOK("POST", "/rfid/tags/status", map[string]any{"epcs": tags[5:6], "status": "LOST", "reason": "hilang"}, nil)

	// tags (5 present) vs system stock (6) → a visible difference
	var vr []struct {
		SKU       string `json:"sku"`
		TagQty    int64  `json:"tag_qty"`
		SystemQty string `json:"system_qty"`
	}
	tn.Owner.mustOK("GET", "/rfid/variance?store_id="+st.Store.ID, nil, &vr)
	if len(vr) != 1 || vr[0].TagQty != 5 || vr[0].SystemQty != "6" {
		t.Fatalf("variance %+v", vr)
	}

	var cnt struct{ ID string }
	tn.Owner.mustOK("POST", "/stock-counts", map[string]any{"warehouse_id": st.Warehouse.ID, "kind": "full"}, &cnt)
	type scanOut struct {
		Added        int               `json:"added"`
		AlreadyCount int               `json:"already_counted"`
		Unknown      []string          `json:"unknown"`
		Foreign      []string          `json:"foreign"`
		Flagged      []string          `json:"flagged"`
		Totals       map[string]string `json:"totals"`
	}
	var s1, s2 scanOut
	unknown := newEPCs(1)
	tn.Owner.mustOK("POST", "/stock-counts/"+cnt.ID+"/rfid", map[string]any{"epcs": append(append([]string{}, tags[:3]...), unknown[0], foreign[0], tags[5])}, &s1)
	if s1.Added != 3 || len(s1.Unknown) != 1 || len(s1.Foreign) != 1 || len(s1.Flagged) != 1 || s1.Totals[it.VariantID] != "3" {
		t.Fatalf("first batch %+v", s1)
	}
	// overlapping second batch: nothing is double counted
	tn.Owner.mustOK("POST", "/stock-counts/"+cnt.ID+"/rfid", map[string]any{"epcs": tags[2:5]}, &s2)
	if s2.Added != 2 || s2.AlreadyCount != 1 || s2.Totals[it.VariantID] != "5" {
		t.Fatalf("second batch %+v", s2)
	}
	var sub struct {
		Summary struct {
			Shortage string `json:"shortage_qty"`
		} `json:"summary"`
	}
	tn.Owner.mustOK("POST", "/stock-counts/"+cnt.ID+"/submit", map[string]any{"uncounted": "zero"}, &sub)
	if sub.Summary.Shortage != "1" {
		t.Fatalf("count result %+v", sub)
	}
	tn.Owner.wantErr("POST", "/stock-counts/"+cnt.ID+"/rfid", map[string]any{"epcs": tags[:1]}, 409, "COUNT_NOT_OPEN")
}
