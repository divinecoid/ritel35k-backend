#!/usr/bin/env python3
"""Seed a demo org through the public API (proves stores/users/products are all just data).

usage: python3 scripts/seed_demo.py [api_base] [email] [password]
"""
import json, sys, urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080/api/v1"
EMAIL = sys.argv[2] if len(sys.argv) > 2 else "owner@ritel35k.test"
PASSWORD = sys.argv[3] if len(sys.argv) > 3 else "ChangeMe123!"
token = None


def call(method, path, body=None, ok_conflict=False):
    return _call(method, path, body, ok_conflict)


def _call(method, path, body=None, ok_conflict=False):
    req = urllib.request.Request(BASE + path, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("content-type", "application/json")
    if token:
        req.add_header("authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as r:
            return json.load(r)["data"] if r.status != 204 else None
    except urllib.error.HTTPError as e:
        err = json.load(e)["error"]
        if ok_conflict and e.code == 409:
            return None
        sys.exit(f"{method} {path}: {e.code} {err['code']} {err['message']} {err.get('details')}")


def ean13(prefix12):
    s = sum(int(c) * (3 if i % 2 else 1) for i, c in enumerate(prefix12))
    return prefix12 + str((10 - s % 10) % 10)


token = call("POST", "/auth/login", {"email": EMAIL, "password": PASSWORD})["access_token"]
tree = call("GET", "/organization/scope-tree")
root = next(u for u in tree if u["type"] == "company")["id"]
existing = {u["code"]: u["id"] for u in tree}


def unit(code, name):
    if code in existing:
        return existing[code]
    return call("POST", "/organization/units", {"parent_id": root, "type": "region", "code": code, "name": name})["id"]


jabo, jabar = unit("JABODETABEK", "Jabodetabek"), unit("JAWA-BARAT", "Jawa Barat")
have = {s["code"] for s in call("GET", "/organization/stores")}
for code, name, parent in [("JKT-BARAT", "Jakarta Barat", jabo), ("JKT-SELATAN", "Jakarta Selatan", jabo),
                           ("BEKASI", "Bekasi", jabo), ("BANDUNG", "Bandung", jabar), ("BOGOR", "Bogor", jabar)]:
    if code not in have:
        call("POST", "/organization/stores", {"code": code, "name": name, "parent_org_unit_id": parent})

# Config inheritance demo: Jawa Barat caps discounts lower than the company default.
call("PUT", "/settings", {"org_unit_id": jabar, "key": "pos.max_discount_percent", "value": 10})

uoms = {u["code"]: u["id"] for u in call("GET", "/uoms")}
cat = next((c for c in call("GET", "/categories") if c["code"] == "SHOES"), None) or call("POST", "/categories", {"code": "SHOES", "name": "Sepatu"})
SELL = {"SNK-001": 350000, "SND-001": 89000, "KAOS-001": 59000}
for i, (code, name, cost) in enumerate([("SNK-001", "Sneakers Classic", "210000"), ("SND-001", "Sandal Harian", "50000"), ("KAOS-001", "Kaos Polos", "30000")]):
    call("POST", "/products", {"code": code, "name": name, "uom_id": uoms["PCS"], "category_id": cat["id"],
         "variants": [{"sku": code, "purchase_price": cost, "cost": cost, "selling_price": SELL[code], "barcodes": [{"barcode": ean13("899300000%03d" % (i + 1))}]}]}, ok_conflict=True)

# Regional price override (Jawa Barat pays 5k more for sneakers) — stores there inherit it automatically.
variants = {v["sku"]: v["id"] for p in call("GET", "/products?per_page=100") for v in p["variants"]}
for sku, price in SELL.items():  # company-wide default price (idempotent: re-setting closes the previous row)
    call("PUT", "/prices", {"org_unit_id": root, "variant_id": variants[sku], "price": price})

call("PUT", "/prices", {"org_unit_id": jabar, "variant_id": variants["SNK-001"], "price": 355000})

# Opening stock for every store that has none yet.
for sc, st in {s["code"]: s["id"] for s in call("GET", "/organization/stores")}.items():
    if not call("GET", f"/inventory/balances?store_id={st}&per_page=1"):
        call("POST", "/inventory/adjustments", {"store_id": st, "type": "OPENING", "reason": "demo opening stock",
             "lines": [{"variant_id": vid, "qty_delta": 50, "unit_cost": {"SNK-001": 210000, "SND-001": 50000, "KAOS-001": 30000}[sku]} for sku, vid in variants.items()]})

call("POST", "/suppliers", {"code": "SUP-001", "name": "PT Sumber Makmur", "pic": "Budi", "phone": "0812000111", "payment_term_days": 30, "lead_time_days": 7}, ok_conflict=True)

roles = {r["code"]: r["id"] for r in call("GET", "/roles")}
stores = {u["code"]: u["id"] for u in call("GET", "/organization/scope-tree") if u["type"] == "store"}
for email, name, role, org in [("andi@ritel35k.test", "Andi (Regional Jabodetabek)", "regional_manager", jabo),
                               ("budi@ritel35k.test", "Budi (Manajer Jakarta Barat)", "store_manager", stores["JKT-BARAT"]),
                               ("siti@ritel35k.test", "Siti (Kasir Jakarta Barat)", "cashier", stores["JKT-BARAT"])]:
    call("POST", "/users", {"email": email, "name": name, "password": "ChangeMe123!", "pin": "1234",
         "assignments": [{"role_id": roles[role], "org_unit_id": org}]}, ok_conflict=True)
print("seeded: 2 regions, 5 stores, 3 products, 1 supplier, 3 scoped users (password ChangeMe123!)")

# Central warehouse (no store) so transfers and purchasing into a distribution centre can be tried.
if not any(w["code"] == "WH-PUSAT" for w in call("GET", "/warehouses")):
    call("POST", "/warehouses", {"code": "WH-PUSAT", "name": "Gudang Pusat", "type": "main"})
print("central warehouse ready")

# ---- Phase 4: members, promotion, voucher, member price list
lv = {l["code"]: l["id"] for l in call("GET", "/membership-levels")}
if not call("GET", "/customers?q=081234567890"):
    c = call("POST", "/customers", {"name": "Rina Wijaya", "phone": "081234567890", "email": "rina@example.com"})
    call("POST", f"/customers/{c['id']}/points", {"points": 300, "note": "bonus pendaftaran demo"})
if not any(p["code"] == "KAOS20" for p in call("GET", "/promotions")):
    call("POST", "/promotions", {"code": "KAOS20", "name": "Diskon Kaos 20%", "type": "percent", "config": {"percent": 20}, "scopes": {"variant_ids": [variants["KAOS-001"]]}})
if not call("GET", "/vouchers?q=HEMAT10K"):
    call("POST", "/vouchers", {"code": "HEMAT10K", "kind": "fixed", "value": 10000, "min_spend": 100000, "batch": "Demo"})
if not any(l["code"] == "MEMBER-GOLD" for l in call("GET", "/price-lists")):
    pl = call("POST", "/price-lists", {"code": "MEMBER-GOLD", "name": "Harga Member Gold", "membership_level_id": lv["GOLD"]})
    call("PUT", "/prices", {"org_unit_id": root, "variant_id": variants["SNK-001"], "price_list_id": pl["id"], "price": 330000})
print("commercial demo data ready")
