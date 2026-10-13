package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ritel35k/backend/db/gen"
	"github.com/ritel35k/backend/internal/numbering"
	"github.com/ritel35k/backend/internal/platform/tx"
)

type storeRow struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}

func storeCodes(t *testing.T, c *client) map[string]bool {
	t.Helper()
	var rows []storeRow
	c.mustOK("GET", "/organization/stores", nil, &rows)
	m := map[string]bool{}
	for _, r := range rows {
		m[r.Code] = true
	}
	return m
}

// The core architectural rule: adding stores is data only; scope isolates who sees what.
func TestAddStoreNeedsNoCodeChangeAndScopeIsolates(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	jabar := tn.unit(t, "region", "JABAR", tn.RootUnit)
	jktB := tn.store(t, "JKTB", jabo)
	jktS := tn.store(t, "JKTS", jabo)
	bdg := tn.store(t, "BDG", jabar)

	if jktB.Warehouse.Code != "WH-JKTB" || jktB.Warehouse.ID == "" {
		t.Fatalf("default warehouse not provisioned: %+v", jktB.Warehouse)
	}

	owner := storeCodes(t, tn.Owner)
	if len(owner) != 3 {
		t.Fatalf("owner sees %v", owner)
	}

	regional := tn.user(t, "regional_manager", jabo)
	if got := storeCodes(t, regional); len(got) != 2 || !got["JKTB"] || !got["JKTS"] {
		t.Fatalf("regional manager sees %v", got)
	}
	manager := tn.user(t, "store_manager", jktB.Store.OrgUnitID)
	if got := storeCodes(t, manager); len(got) != 1 || !got["JKTB"] {
		t.Fatalf("store manager sees %v", got)
	}
	cashier := tn.user(t, "cashier", jktB.Store.OrgUnitID)
	manager.wantErr("GET", "/organization/stores/"+bdg.Store.ID, nil, 404, "NOT_FOUND")
	regional.wantErr("GET", "/organization/stores/"+bdg.Store.ID, nil, 404, "NOT_FOUND")
	manager.wantErr("GET", "/organization/stores/"+jktS.Store.ID, nil, 404, "NOT_FOUND")

	// scope tree: manager gets own store as accessible, ancestors only for context
	var tree []struct {
		Type       string `json:"type"`
		Code       string `json:"code"`
		Accessible bool   `json:"accessible"`
	}
	manager.mustOK("GET", "/organization/scope-tree", nil, &tree)
	acc := 0
	for _, u := range tree {
		if u.Accessible {
			acc++
			if u.Code != "JKTB" {
				t.Fatalf("unexpected accessible unit %s", u.Code)
			}
		}
		if u.Code == "BDG" || u.Code == "JKTS" {
			t.Fatalf("leaked unit %s", u.Code)
		}
	}
	if acc != 1 {
		t.Fatalf("accessible=%d", acc)
	}

	// permissions: no escalation, no cross-scope writes
	manager.wantErr("POST", "/organization/stores", map[string]any{"code": "NEW1", "name": "x"}, 403, "FORBIDDEN")
	regional.wantErr("POST", "/organization/stores", map[string]any{"code": "NEW2", "name": "x"}, 403, "FORBIDDEN")
	cashier.wantErr("GET", "/users", nil, 403, "FORBIDDEN")
	manager.wantErr("PUT", "/settings", map[string]any{"org_unit_id": jktB.Store.OrgUnitID, "key": "pos.max_discount_percent", "value": 1}, 403, "FORBIDDEN")

	// warehouses are scoped like stores
	var whs []struct{ Code string }
	manager.mustOK("GET", "/warehouses", nil, &whs)
	if len(whs) != 1 || whs[0].Code != "WH-JKTB" {
		t.Fatalf("manager warehouses: %+v", whs)
	}

	// Store #4 "just by data": usable immediately, inherits everything, no other step.
	n := tn.store(t, "NEW4", jabar)
	if len(storeCodes(t, tn.Owner)) != 4 || n.Warehouse.ID == "" {
		t.Fatal("4th store missing")
	}
	// Owner can scope a brand-new user to the new store without anything else.
	tn.user(t, "store_manager", n.Store.OrgUnitID)
}

func TestSettingsInheritance(t *testing.T) {
	tn := newTenant(t)
	jabo := tn.unit(t, "region", "JABO", tn.RootUnit)
	jabar := tn.unit(t, "region", "JABAR", tn.RootUnit)
	a := tn.store(t, "ST-A", jabo)
	b := tn.store(t, "ST-B", jabo)
	c := tn.store(t, "ST-C", jabar)

	type eff struct {
		Key       string          `json:"key"`
		Value     json.RawMessage `json:"value"`
		Source    string          `json:"source"`
		Inherited bool            `json:"inherited"`
	}
	get := func(unit, key string) eff {
		var all []eff
		tn.Owner.mustOK("GET", "/settings/effective?org_unit_id="+unit, nil, &all)
		for _, e := range all {
			if e.Key == key {
				return e
			}
		}
		t.Fatalf("key %s missing", key)
		return eff{}
	}
	set := func(unit string, v any) {
		tn.Owner.mustOK("PUT", "/settings", map[string]any{"org_unit_id": unit, "key": "pos.max_discount_percent", "value": v}, nil)
	}
	const key = "pos.max_discount_percent"

	if e := get(a.Store.OrgUnitID, key); e.Source != "default" || string(e.Value) != "20" {
		t.Fatalf("default: %+v", e)
	}
	set(tn.RootUnit, 15)
	if e := get(c.Store.OrgUnitID, key); e.Source != "company" || string(e.Value) != "15" || !e.Inherited {
		t.Fatalf("company: %+v", e)
	}
	set(jabo, 10)
	if e := get(a.Store.OrgUnitID, key); e.Source != "region" || string(e.Value) != "10" {
		t.Fatalf("region: %+v", e)
	}
	if e := get(c.Store.OrgUnitID, key); string(e.Value) != "15" {
		t.Fatalf("other region unaffected: %+v", e)
	}
	set(a.Store.OrgUnitID, 5)
	if e := get(a.Store.OrgUnitID, key); e.Source != "store" || string(e.Value) != "5" || e.Inherited {
		t.Fatalf("store: %+v", e)
	}
	if e := get(b.Store.OrgUnitID, key); string(e.Value) != "10" {
		t.Fatalf("sibling keeps region value: %+v", e)
	}
	tn.Owner.mustOK("DELETE", "/settings", map[string]any{"org_unit_id": a.Store.OrgUnitID, "key": key}, nil)
	if e := get(a.Store.OrgUnitID, key); string(e.Value) != "10" {
		t.Fatalf("revert falls back to region: %+v", e)
	}

	tn.Owner.wantErr("PUT", "/settings", map[string]any{"org_unit_id": jabo, "key": "nope.key", "value": 1}, 422, "VALIDATION_FAILED")
	tn.Owner.wantErr("PUT", "/settings", map[string]any{"org_unit_id": jabo, "key": key, "value": "abc"}, 422, "VALIDATION_FAILED")

	// per-store overrides can be supplied at provisioning
	var s storeOut
	tn.Owner.mustOK("POST", "/organization/stores", map[string]any{"code": "OVR", "name": "Override", "parent_org_unit_id": jabo,
		"settings": map[string]any{"tax.ppn_percent": 12}}, &s)
	if e := get(s.Store.OrgUnitID, "tax.ppn_percent"); e.Source != "store" || string(e.Value) != "12" {
		t.Fatalf("provision override: %+v", e)
	}
}

func TestProvisioningIsAtomic(t *testing.T) {
	tn := newTenant(t)
	tn.store(t, "DUP", "")
	tn.Owner.wantErr("POST", "/organization/stores", map[string]any{"code": "DUP", "name": "again"}, 409, "STORE_CODE_TAKEN")
	tn.Owner.wantErr("POST", "/organization/stores", map[string]any{"code": "BAD!", "name": "x"}, 422, "VALIDATION_FAILED")
	tn.Owner.wantErr("POST", "/organization/stores", map[string]any{"code": "TZ", "name": "x", "timezone": "Mars/Base"}, 422, "VALIDATION_FAILED")
	tn.Owner.wantErr("POST", "/organization/stores", map[string]any{"code": "BADSET", "name": "x", "settings": map[string]any{"tax.ppn_percent": "x"}}, 422, "VALIDATION_FAILED")

	var units, stores, whs int
	cid := uuid.MustParse(tn.CompanyID)
	_ = testPool.QueryRow(context.Background(), `select count(*) from org_units where company_id=$1 and type='store'`, cid).Scan(&units)
	_ = testPool.QueryRow(context.Background(), `select count(*) from stores where company_id=$1`, cid).Scan(&stores)
	_ = testPool.QueryRow(context.Background(), `select count(*) from warehouses where company_id=$1`, cid).Scan(&whs)
	if units != 1 || stores != 1 || whs != 1 {
		t.Fatalf("partial rows left behind: units=%d stores=%d warehouses=%d", units, stores, whs)
	}
}

func TestOutboxRelayDeliversOnce(t *testing.T) {
	tn := newTenant(t)
	s := tn.store(t, "EVT", "")
	var n int
	_ = testPool.QueryRow(context.Background(), `select count(*) from outbox where aggregate_id=$1 and event_type='StoreProvisioned'`, uuid.MustParse(s.Store.ID)).Scan(&n)
	if n != 1 {
		t.Fatalf("outbox rows=%d", n)
	}
	relay := testApp.Relay()
	for i := 0; i < 3; i++ { // drain
		if _, err := relay.RunOnce(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	var pub *time.Time
	_ = testPool.QueryRow(context.Background(), `select published_at from outbox where aggregate_id=$1`, uuid.MustParse(s.Store.ID)).Scan(&pub)
	if pub == nil {
		t.Fatal("event not published")
	}
}

func TestDocumentNumberingConcurrent(t *testing.T) {
	tn := newTenant(t)
	s := tn.store(t, "NUM", "")
	cid, sid := uuid.MustParse(tn.CompanyID), uuid.MustParse(s.Store.ID)
	const N = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]bool{}
	mgr := tx.NewManager(testPool)
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := mgr.Do(context.Background(), func(q *gen.Queries) error {
				no, err := numbering.Next(context.Background(), q, cid, sid, "sale", at)
				mu.Lock()
				got[no] = true
				mu.Unlock()
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(got) != N {
		t.Fatalf("expected %d unique numbers, got %d", N, len(got))
	}
	if !got["SO/NUM/2026/10/000001"] || !got["SO/NUM/2026/10/000040"] {
		t.Fatalf("sequence not contiguous: %v", got)
	}
}
