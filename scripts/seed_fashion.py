#!/usr/bin/env python3
"""Initial catalogue for a fashion & accessories retailer (kaos, sepatu, jepit rambut, kaos kaki, ...), through the public API.
Idempotent: re-running skips what exists. Credentials come from the environment, never from argv.

  R35K_API=https://api.example.com/api/v1 R35K_EMAIL=owner@x.id R35K_PASSWORD=... python3 scripts/seed_fashion.py
"""
import hashlib, json, os, sys, urllib.request

BASE = os.environ.get("R35K_API", "http://localhost:8080/api/v1")
token = None


def call(method, path, body=None, ok_conflict=False):
    req = urllib.request.Request(BASE + path, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("content-type", "application/json")
    if token:
        req.add_header("authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return json.load(r)["data"] if r.status != 204 else None
    except urllib.error.HTTPError as e:
        err = json.load(e)["error"]
        if ok_conflict and e.code == 409:
            return None
        sys.exit(f"{method} {path}: {e.code} {err['code']} {err['message']} {err.get('details')}")


def ean13(p12):
    s = sum(int(c) * (3 if i % 2 else 1) for i, c in enumerate(p12))
    return p12 + str((10 - s % 10) % 10)


token = call("POST", "/auth/login", {"email": os.environ["R35K_EMAIL"], "password": os.environ["R35K_PASSWORD"]})["access_token"]

# ---- organisation: two regions, three stores
tree = call("GET", "/organization/scope-tree")
root = next(u for u in tree if u["type"] == "company")["id"]
existing = {u["code"]: u["id"] for u in tree}


def region(code, name):
    return existing.get(code) or call("POST", "/organization/units", {"parent_id": root, "type": "region", "code": code, "name": name})["id"]


jabo, jabar = region("JABODETABEK", "Jabodetabek"), region("JAWA-BARAT", "Jawa Barat")
have = {s["code"] for s in call("GET", "/organization/stores")}
for code, name, parent in [("JKT-BARAT", "Toko Jakarta Barat", jabo), ("JKT-SELATAN", "Toko Jakarta Selatan", jabo), ("BANDUNG", "Toko Bandung", jabar)]:
    if code not in have:
        call("POST", "/organization/stores", {"code": code, "name": name, "parent_org_unit_id": parent})
if not any(w["code"] == "WH-PUSAT" for w in call("GET", "/warehouses")):
    call("POST", "/warehouses", {"code": "WH-PUSAT", "name": "Gudang Pusat", "type": "main"})

# ---- master data
uoms = {u["code"]: u["id"] for u in call("GET", "/uoms")}
cats = {c["code"]: c["id"] for c in call("GET", "/categories")}
for code, name in [("PAKAIAN", "Pakaian"), ("ALAS-KAKI", "Alas Kaki"), ("AKS-RAMBUT", "Aksesori Rambut"), ("KAOS-KAKI", "Kaos Kaki"), ("AKSESORI", "Aksesori & Tas")]:
    if code not in cats:
        cats[code] = call("POST", "/categories", {"code": code, "name": name})["id"]
brands = {b["code"]: b["id"] for b in call("GET", "/brands")}
for code, name in [("URBAN-BASIC", "Urban Basic"), ("STEPUP", "StepUp"), ("CANTIKA", "Cantika")]:
    if code not in brands:
        brands[code] = call("POST", "/brands", {"code": code, "name": name})["id"]

C = ["Hitam", "Putih"]
# code, name, category, brand, uom, variant labels, cost, price, reorder point, supplier
PRODUCTS = [
    ("KAOS-POLOS", "Kaos Polos Katun Combed 30s", "PAKAIAN", "URBAN-BASIC", "PCS", [f"{s} / {c}" for c in C for s in ("S", "M", "L", "XL")], 32000, 69000, 10, "GARMEN"),
    ("KAOS-OVS", "Kaos Oversize Unisex", "PAKAIAN", "URBAN-BASIC", "PCS", [f"{s} / {c}" for c in ("Hitam", "Krem") for s in ("M", "L", "XL")], 48000, 99000, 8, "GARMEN"),
    ("KAOS-GRAF", "Kaos Grafis Sablon", "PAKAIAN", "URBAN-BASIC", "PCS", [f"{s} / Navy" for s in ("M", "L", "XL")], 45000, 95000, 6, "GARMEN"),
    ("SNK-CLS", "Sneakers Classic", "ALAS-KAKI", "STEPUP", "PCS", [f"{n} / Putih" for n in (39, 40, 41, 42, 43)], 210000, 369000, 4, "ALASKAKI"),
    ("SEP-SKL", "Sepatu Sekolah Hitam", "ALAS-KAKI", "STEPUP", "PCS", [f"{n}" for n in range(36, 43)], 95000, 175000, 5, "ALASKAKI"),
    ("SND-SLD", "Sandal Slide Karet", "ALAS-KAKI", "STEPUP", "PCS", ["S (38-39)", "M (40-41)", "L (42-43)"], 28000, 59000, 8, "ALASKAKI"),
    ("SND-JPT", "Sandal Jepit Harian", "ALAS-KAKI", "STEPUP", "PCS", ["S (38-39)", "M (40-41)", "L (42-43)"], 12000, 29000, 10, "ALASKAKI"),
    ("JPT-MUT", "Jepit Rambut Mutiara isi 6", "AKS-RAMBUT", "CANTIKA", "PACK", [""], 9000, 19000, 12, "AKSESORI"),
    ("JPT-CKR", "Jepit Cakar Rambut", "AKS-RAMBUT", "CANTIKA", "PCS", ["Hitam", "Coklat", "Pink"], 6000, 15000, 15, "AKSESORI"),
    ("JPT-HTM", "Jepit Rambut Hitam Set 12 pcs", "AKS-RAMBUT", "CANTIKA", "PACK", [""], 7000, 15000, 12, "AKSESORI"),
    ("BANDO", "Bando Kain", "AKS-RAMBUT", "CANTIKA", "PCS", ["Hitam", "Navy", "Pink"], 8000, 18000, 10, "AKSESORI"),
    ("SCRUNCH", "Ikat Rambut Scrunchie", "AKS-RAMBUT", "CANTIKA", "PCS", ["Hitam", "Krem", "Pink"], 4000, 10000, 20, "AKSESORI"),
    ("KK-SPORT", "Kaos Kaki Sport (pasang)", "KAOS-KAKI", None, "PCS", C, 8000, 19000, 20, "KAOSKAKI"),
    ("KK-SKL", "Kaos Kaki Sekolah (pasang)", "KAOS-KAKI", None, "PCS", C, 7000, 16000, 20, "KAOSKAKI"),
    ("KK-ANKL", "Kaos Kaki Ankle isi 3 pasang", "KAOS-KAKI", None, "PACK", [""], 15000, 32000, 15, "KAOSKAKI"),
    ("TOPI-BB", "Topi Baseball", "AKSESORI", None, "PCS", ["Hitam", "Navy"], 25000, 59000, 6, "GARMEN"),
    ("TAS-TOTE", "Tote Bag Kanvas", "AKSESORI", None, "PCS", [""], 22000, 55000, 6, "GARMEN"),
    ("IKAT-PGG", "Ikat Pinggang Sintetis", "AKSESORI", None, "PCS", ["Hitam", "Coklat"], 30000, 79000, 5, "GARMEN"),
    ("DOMPET", "Dompet Lipat Pria", "AKSESORI", None, "PCS", [""], 40000, 99000, 5, "GARMEN"),
]
SUPPLIERS = {"GARMEN": ("SUP-GARMEN", "CV Garmen Nusantara", 5), "ALASKAKI": ("SUP-ALASKAKI", "PT Alas Kaki Prima", 7),
             "AKSESORI": ("SUP-AKSESORI", "UD Aksesori Cantik", 3), "KAOSKAKI": ("SUP-KAOSKAKI", "CV Kaos Kaki Jaya", 3)}

seq = 0
variants = {}  # sku -> (cost, supplier key)
for code, name, cat, brand, uom, labels, cost, price, rop, sup in PRODUCTS:
    vs = []
    for i, label in enumerate(labels):
        seq += 1
        sku = f"{code}-{i + 1:02d}"
        vs.append({"sku": sku, "name": label, "purchase_price": cost, "cost": cost, "selling_price": price, "min_stock": max(2, rop // 3), "reorder_point": rop,
                   "barcodes": [{"barcode": ean13("89910%07d" % seq), "kind": "EAN13", "is_primary": True}]})
        variants[sku] = (cost, sup)
    call("POST", "/products", {"code": code, "name": name, "uom_id": uoms[uom], "category_id": cats[cat], "brand_id": brands.get(brand) if brand else None,
                               "tax_code": "PPN", "variants": vs}, ok_conflict=True)

vid = {v["sku"]: v["id"] for p in call("GET", "/products?per_page=200&status=active") for v in p["variants"] if v["sku"] in variants}
missing = set(variants) - set(vid)
if missing:
    sys.exit(f"variants not found after creation: {sorted(missing)[:5]}")

# ---- suppliers (+ which variants they supply: feeds the restock suggestions)
sup_id = {}
existing_sup = {s["code"]: s["id"] for s in call("GET", "/suppliers?per_page=100")}
for key, (code, name, lead) in SUPPLIERS.items():
    sup_id[key] = existing_sup.get(code) or call("POST", "/suppliers", {"code": code, "name": name, "payment_term_days": 14, "lead_time_days": lead})["id"]
for sku, (cost, key) in variants.items():
    call("PUT", f"/suppliers/{sup_id[key]}/products", {"variant_id": vid[sku], "purchase_price": cost, "moq": 6, "lead_time_days": SUPPLIERS[key][2]})

# ---- opening stock for every store that has none (deterministic spread, 10-48 per SKU)
for code, st in {s["code"]: s["id"] for s in call("GET", "/organization/stores")}.items():
    if call("GET", f"/inventory/balances?store_id={st}&per_page=1"):
        continue
    lines = [{"variant_id": vid[sku], "qty_delta": 10 + int(hashlib.md5((sku + code).encode()).hexdigest(), 16) % 39, "unit_cost": cost} for sku, (cost, _) in variants.items()]
    call("POST", "/inventory/adjustments", {"store_id": st, "type": "OPENING", "reason": "stok awal", "lines": lines})
print(f"seeded: 2 regions, 3 stores, {len(PRODUCTS)} products / {len(variants)} SKUs, {len(SUPPLIERS)} suppliers, opening stock per store")
