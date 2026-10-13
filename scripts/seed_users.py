#!/usr/bin/env python3
"""One store manager and one cashier per store, each scoped to their own store. Random passwords are generated and printed
once (not stored anywhere); existing emails are skipped, so re-running never resets a password.

  R35K_API=… R35K_EMAIL=owner@x.id R35K_PASSWORD=… python3 scripts/seed_users.py
"""
import json, os, secrets, string, sys, urllib.request

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


def password():
    alphabet = string.ascii_letters.replace("l", "").replace("I", "").replace("O", "") + string.digits.replace("0", "").replace("1", "")
    return "".join(secrets.choice(alphabet) for _ in range(12))


token = call("POST", "/auth/login", {"email": os.environ["R35K_EMAIL"], "password": os.environ["R35K_PASSWORD"]})["access_token"]
roles = {r["code"]: r["id"] for r in call("GET", "/roles")}
domain = os.environ.get("R35K_USER_DOMAIN", "ritel35k.com")
stores = [u for u in call("GET", "/organization/scope-tree") if u["type"] == "store"]
have = {u["email"] for u in call("GET", "/users?per_page=200")}
rows = []
for st in sorted(stores, key=lambda s: s["code"]):
    slug = st["code"].lower()
    for kind, role, label in (("manajer", "store_manager", "Manajer"), ("kasir", "cashier", "Kasir")):
        email = f"{kind}.{slug}@{domain}"
        if email in have:
            rows.append((st["name"], role, email, "(sudah ada, tidak diubah)", ""))
            continue
        pw, pin = password(), "".join(secrets.choice(string.digits) for _ in range(6))
        call("POST", "/users", {"email": email, "name": f"{label} {st['name']}", "password": pw, "pin": pin,
                                "assignments": [{"role_id": roles[role], "org_unit_id": st["id"]}]})
        rows.append((st["name"], role, email, pw, pin))
w = max(len(r[2]) for r in rows)
print(f"{'Toko':22} {'Role':14} {'Email':{w}}  Password      PIN")
for r in rows:
    print(f"{r[0]:22} {r[1]:14} {r[2]:{w}}  {r[3]:13} {r[4]}")
