#!/usr/bin/env python3
"""
Phase 4A.3 end-to-end check: vendor WhatsApp content -> SupplySignal ->
DRAFT SupplyListing, through the real backend binary and the real Python
extraction service, with Meta-format webhooks signed with
X-Hub-Signature-256. Media downloads go to scripts/e2e/fake_graph_api.py
(MEDIA_DIR). This is a test-equivalent run, NOT a live WhatsApp run.

Proves: signal created -> listing created automatically as DRAFT (never
ACTIVE) -> correct product/variant/condition/price/category -> references
the stored media in order -> location (vendor fallback, and explicit
location winning) -> L-reference -> last_verified_at -> found by
normalized product and location search -> 'hello' gets no listing.

Env: BASE, APP_SECRET, DATABASE_URL (psql for read-only checks), MEDIA_DIR.
"""
import hashlib
import hmac
import json
import os
import random
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("BASE", "http://localhost:8080")
SECRET = os.environ.get("APP_SECRET", "")
DB = os.environ["DATABASE_URL"]
MEDIA_DIR = os.environ["MEDIA_DIR"]
RUN = f"{random.randint(100000, 999999)}"
checks = 0


def ok(cond, label, detail=""):
    global checks
    checks += 1
    print(f"  [{'PASS' if cond else 'FAIL'}] {label}" + (f" -> {detail}" if detail else ""))
    if not cond:
        sys.exit(1)


def sql(q):
    return subprocess.run(["psql", DB, "-At", "-c", q], capture_output=True, text=True, check=True).stdout.strip()


def api(method, path, body=None):
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None,
                                 method=method, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, None


def wait(cond, timeout=30.0):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.25)
    return cond()


def webhook(frm, wamid, kind, body="", media_id=""):
    msg = {"from": frm, "id": wamid, "timestamp": str(int(time.time())), "type": kind}
    if kind == "text":
        msg["text"] = {"body": body}
    else:
        msg[kind] = {"id": media_id, "mime_type": "image/jpeg"}
    payload = json.dumps({"object": "whatsapp_business_account", "entry": [{"changes": [{"field": "messages", "value": {
        "messaging_product": "whatsapp", "contacts": [{"wa_id": frm, "profile": {"name": "V"}}], "messages": [msg]}}]}]}).encode()
    sig = "sha256=" + hmac.new(SECRET.encode(), payload, hashlib.sha256).hexdigest()
    req = urllib.request.Request(BASE + "/webhooks/whatsapp", data=payload, method="POST",
                                 headers={"Content-Type": "application/json", "X-Hub-Signature-256": sig})
    with urllib.request.urlopen(req) as r:
        return r.status


def vendor(name, phone, location):
    _, v = api("POST", "/api/vendors", {"business_name": name, "whatsapp_external_id": phone, "primary_location": location})
    api("PATCH", f"/api/vendors/{v['id']}", {"status": "ACTIVE"})
    return v["id"]


def listings(vid):
    _, ls = api("GET", f"/api/vendors/{vid}/supply-listings")
    return ls or []


def asset_of(wamid):
    return sql(f"SELECT COALESCE(media_asset_id::text,'') FROM messages WHERE external_message_id = '{wamid}'")


print(f"Majo Phase 4A.3 listing E2E · run {RUN} · backend {BASE}")

print("\n1. Vendor sends three photos + 'iPhone 13 128GB ₦620,000 Brand new'")
M = "23470" + RUN + "51"
vm = vendor("Mike Gadgets " + RUN, M, "Benin City")
for i in (1, 2, 3):
    open(os.path.join(MEDIA_DIR, f"l{RUN}{i}.bin"), "wb").write(b"\xff\xd8\xff\xe0" + f"iphone-{i}-{RUN}".encode() + os.urandom(48))
for i in (1, 2, 3):
    ok(webhook(M, f"wamid.{RUN}.m{i}", "image", media_id=f"l{RUN}{i}") == 200, f"photo {i} accepted")
    time.sleep(0.3)
ok(webhook(M, f"wamid.{RUN}.m4", "text", "iPhone 13 128GB ₦620,000 Brand new") == 200, "caption accepted")

sig = wait(lambda: sql(f"SELECT id FROM supply_signals WHERE vendor_id = '{vm}'"))
ok(bool(sig), "2. Step 2 created the SupplySignal", sig)
ls = wait(lambda: listings(vm))
ok(len(ls) == 1, "3-4. the listing service created a listing automatically", str(len(ls)))
l = ls[0]
ok(l["status"] == "DRAFT" and l["published_at"] is None, "5. it is a DRAFT (not published)", l["status"])
ok(l["supply_signal_id"] == sig, "provenance: listing -> signal")
ok(l["product"] == "iphone 13" and l["variant"] == "128GB" and l["condition"] == "new" and l["price"] == 620000
   and l["currency"] == "NGN" and l["category"] == "electronics",
   "6. product / variant / condition / price / category from the real extractor",
   f"{l['product']} {l['variant']} {l['condition']} {l['price']} {l['currency']} {l['category']}")
want = [asset_of(f"wamid.{RUN}.m{i}") for i in (1, 2, 3)]
got = [m.get("asset_id") for m in l["media_references"]]
ok(got == want and all(want), "7. references the 3 stored MediaAssets, in send order", ", ".join(a[:8] for a in got))
ok(all(m.get("storage_key", "").startswith(f"vendors/{vm}/media/") for m in l["media_references"]),
   "media references point at the existing stored objects (no copy)")
ok(sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{vm}'") == "3", "no new MediaAssets were created")
ok(l["location"] == "Benin City", "8. location: vendor's primary location (none in the message)", l["location"])
ok(re.match(r"^L-[0-9A-HJKMNP-TV-Z]{6}$", l["public_reference"] or "") is not None, "9. public L-reference", l["public_reference"])
ok(l["last_verified_at"] is not None, "10. last_verified_at set", l["last_verified_at"])
same = sql(f"""SELECT (l.last_verified_at = s.created_at AND l.expires_at = s.created_at + interval '7 days')::text
               FROM supply_listings l JOIN supply_signals s ON s.id = l.supply_signal_id WHERE l.id = '{l['id']}'""")
ok(same == "true", "    = when the supply evidence arrived (signal time); expires 7 days later")

print("\n11. Deterministic search (operator search, status=DRAFT; nothing is published)")
for q in ("iphone 13", "iPhone13", "IPHONE 13"):
    _, res = api("GET", "/api/supply-listings?" + urllib.parse.urlencode({"product": q, "status": "DRAFT", "location": "BENIN CITY"}))
    ok(any(x["id"] == l["id"] for x in (res or [])), f"product={q!r} + location='BENIN CITY' finds it")
_, res = api("GET", "/api/supply-listings?" + urllib.parse.urlencode({"product": "iphone 13"}))
ok(not any(x["id"] == l["id"] for x in (res or [])), "default (marketplace) search does not show the DRAFT")

print("\nExplicit location wins over the vendor's primary location")
P = "23470" + RUN + "52"
vp = vendor("Port Vendor " + RUN, P, "Benin City")
ok(webhook(P, f"wamid.{RUN}.p1", "text", "PS5 850k Port Harcourt") == 200, "vendor (primary: Benin City) sends 'PS5 850k Port Harcourt'")
lp = wait(lambda: listings(vp))
ok(len(lp) == 1 and lp[0]["location"] == "Port Harcourt" and lp[0]["status"] == "DRAFT", "listing location = Port Harcourt", lp[0]["location"] if lp else "none")

print("\nIneligible content gets no listing")
H = "23470" + RUN + "53"
vh = vendor("Hello Vendor " + RUN, H, "Benin City")
ok(webhook(H, f"wamid.{RUN}.h1", "text", "hello") == 200, "vendor sends 'hello'")
ok(bool(wait(lambda: sql(f"SELECT id FROM supply_signals WHERE vendor_id = '{vh}'"))), "it is stored as a signal (evidence)")
time.sleep(2)
ok(listings(vh) == [], "but no listing is created")
code, _ = api("POST", "/api/supply-listings", {"supply_signal_id": sql(f"SELECT id FROM supply_signals WHERE vendor_id = '{vh}'")})
ok(code == 422, "the operator API refuses it too (422)", str(code))

print("\nScope")
ok(sql(f"SELECT count(*) FROM supply_listings WHERE vendor_id IN ('{vm}','{vp}','{vh}') AND status = 'ACTIVE'") == "0", "nothing ACTIVE")
ok(sql(f"SELECT count(*) FROM offers o JOIN supply_listings l ON l.id = o.supply_listing_id WHERE l.vendor_id IN ('{vm}','{vp}')") == "0", "no offers")
print(f"\nALL {checks} CHECKS PASSED")
