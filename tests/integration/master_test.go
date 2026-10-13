package integration

import (
	"context"
	"testing"

	"github.com/ritel35k/backend/internal/product"
)

func TestEAN13(t *testing.T) {
	if !product.ValidEAN13("4006381333931") || product.ValidEAN13("4006381333932") || product.ValidEAN13("123") {
		t.Fatal("ean13 checksum")
	}
}

func TestProductMasterAndBarcodes(t *testing.T) {
	tn := newTenant(t)
	var uoms []struct{ ID, Code string }
	tn.Owner.mustOK("GET", "/uoms", nil, &uoms)
	var pcs string
	for _, u := range uoms {
		if u.Code == "PCS" {
			pcs = u.ID
		}
	}
	if pcs == "" {
		t.Fatal("default UOM missing")
	}
	var cat struct{ ID string }
	tn.Owner.mustOK("POST", "/categories", map[string]any{"code": "SHOES", "name": "Shoes"}, &cat)
	tn.Owner.wantErr("POST", "/categories", map[string]any{"code": "SHOES", "name": "dup"}, 409, "CATEGORY_CODE_TAKEN")

	mk := func(code, sku, bc string) map[string]any {
		return map[string]any{"code": code, "name": "Item " + code, "uom_id": pcs, "category_id": cat.ID,
			"variants": []map[string]any{{"sku": sku, "purchase_price": "12500.5", "cost": "12500.5",
				"barcodes": []map[string]any{{"barcode": bc}, {"barcode": "INT-" + sku, "kind": "INTERNAL"}}}}}
	}
	var p struct {
		ID       string `json:"id"`
		Version  int    `json:"version"`
		Variants []struct {
			ID            string `json:"id"`
			PurchasePrice string `json:"purchase_price"`
			Barcodes      []struct{ Barcode string }
		}
	}
	tn.Owner.mustOK("POST", "/products", mk("P1", "SKU-1", "4006381333931"), &p)
	if len(p.Variants) != 1 || len(p.Variants[0].Barcodes) != 2 || p.Variants[0].PurchasePrice != "12500.5" {
		t.Fatalf("product: %+v", p)
	}

	// uniqueness is company-wide: a scan resolves to exactly one SKU
	tn.Owner.wantErr("POST", "/products", mk("P2", "SKU-2", "4006381333931"), 409, "BARCODE_TAKEN")
	tn.Owner.wantErr("POST", "/products", mk("P1", "SKU-9", "4006381333948"), 409, "PRODUCT_CODE_TAKEN")
	tn.Owner.wantErr("POST", "/products", mk("P3", "SKU-1", "4006381333948"), 409, "SKU_TAKEN")
	tn.Owner.wantErr("POST", "/products", mk("P4", "SKU-4", "4006381333930"), 422, "VALIDATION_FAILED") // bad checksum
	var n int
	_ = testPool.QueryRow(context.Background(), `select count(*) from products where company_id=$1`, tn.CompanyID).Scan(&n)
	if n != 1 {
		t.Fatalf("failed creates must roll back, products=%d", n)
	}

	var hit struct {
		VariantID string `json:"variant_id"`
		SKU       string `json:"sku"`
	}
	tn.Owner.mustOK("GET", "/products/lookup?barcode=INT-SKU-1", nil, &hit)
	if hit.SKU != "SKU-1" {
		t.Fatalf("lookup: %+v", hit)
	}
	tn.Owner.wantErr("GET", "/products/lookup?barcode=0000", nil, 404, "NOT_FOUND")

	// optimistic locking
	tn.Owner.mustOK("PATCH", "/products/"+p.ID, map[string]any{"version": p.Version, "name": "Renamed"}, nil)
	tn.Owner.wantErr("PATCH", "/products/"+p.ID, map[string]any{"version": p.Version, "name": "Stale"}, 409, "VERSION_CONFLICT")

	// search finds by barcode and name
	var list []struct{ ID string }
	tn.Owner.mustOK("GET", "/products?q=4006381333931", nil, &list)
	if len(list) != 1 {
		t.Fatalf("search by barcode: %d", len(list))
	}

	// global master is writable only by company-wide product.manage; store users read-only
	a := tn.store(t, "ST-A", "")
	mgr := tn.user(t, "store_manager", a.Store.OrgUnitID)
	mgr.mustOK("GET", "/products", nil, nil)
	mgr.wantErr("POST", "/products", mk("P5", "SKU-5", "4006381333948"), 403, "FORBIDDEN")
}

func TestAuditAppendOnly(t *testing.T) {
	tn := newTenant(t)
	tn.store(t, "AUD", "")
	var logs []struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	tn.Owner.mustOK("GET", "/audit-logs?action=STORE_CREATED", nil, &logs)
	if len(logs) != 1 {
		t.Fatalf("audit rows=%d", len(logs))
	}
	if _, err := testPool.Exec(context.Background(), `update audit_logs set action='X' where id=$1`, logs[0].ID); err == nil {
		t.Fatal("audit_logs must be immutable")
	}
	if _, err := testPool.Exec(context.Background(), `delete from audit_logs where id=$1`, logs[0].ID); err == nil {
		t.Fatal("audit_logs must be undeletable")
	}
}
