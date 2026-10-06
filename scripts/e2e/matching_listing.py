#!/usr/bin/env python3
"""
Phase 4A (Architecture Lock V2 §M-4A.6/7) end-to-end check: matching reads
operator-published SupplyListings, and the funnel shows supply metrics.
Real backend binary, real PostgreSQL, real Python extraction service,
Meta-format signed webhooks; media downloads go to fake_graph_api.py
(MEDIA_DIR). Test-equivalent run, NOT live WhatsApp.

The Lock's definition of done, step by step:
  * a vendor sends 3 photos + "Imported ladies bags ₦18,500" -> one signal
    with 3 stored assets -> a DRAFT listing with media, price, product and
    location;
  * a buyer asks for ladies bags; the operator confirms the request;
  * before publication the vendor is matched on the raw signal;
  * the operator corrects the display product and publishes (L- reference);
  * matching now ranks that vendor on the published listing (reasons, L-ref,
    budget from the listing), and routing stores that provenance;
  * the supply metrics show the submission, the media-complete listing and
    the approval.

Env: BASE, APP_SECRET, DATABASE_URL (psql), MEDIA_DIR, OPERATOR_TOKEN.
Use a fresh database: the supply-metric checks count everything in the window.
"""
import hashlib
import hmac
import json
import os
import random
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.request
import zlib

BASE = os.environ.get("BASE", "http://localhost:8080")
SECRET = os.environ.get("APP_SECRET", "")
DB = os.environ["DATABASE_URL"]
MEDIA_DIR = os.environ["MEDIA_DIR"]
TOKEN = os.environ["OPERATOR_TOKEN"]
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
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None, method=method,
                                 headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOKEN})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"null")


def wait(cond, timeout=30.0):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.25)
    return cond()


def png(rgb):
    w = h = 16
    raw = b"".join(b"\x00" + bytes(rgb) * w for _ in range(h))

    def chunk(t, d):
        return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def webhook(frm, wamid, kind, body="", media_id=""):
    msg = {"from": frm, "id": wamid, "timestamp": str(int(time.time())), "type": kind}
    if kind == "text":
        msg["text"] = {"body": body}
    else:
        msg[kind] = {"id": media_id, "mime_type": "image/png"}
    payload = json.dumps({"object": "whatsapp_business_account", "entry": [{"changes": [{"field": "messages", "value": {
        "messaging_product": "whatsapp", "contacts": [{"wa_id": frm, "profile": {"name": "U"}}], "messages": [msg]}}]}]}).encode()
    sig = "sha256=" + hmac.new(SECRET.encode(), payload, hashlib.sha256).hexdigest()
    req = urllib.request.Request(BASE + "/webhooks/whatsapp", data=payload, method="POST",
                                 headers={"Content-Type": "application/json", "X-Hub-Signature-256": sig})
    with urllib.request.urlopen(req) as r:
        return r.status


def vendor(name, phone, categories):
    _, v = api("POST", "/api/vendors", {"business_name": name, "whatsapp_external_id": phone,
                                         "primary_location": "Benin City", "categories": categories})
    api("PATCH", f"/api/vendors/{v['id']}", {"status": "ACTIVE"})
    return v["id"]


def supply_rows():
    _, rep = api("GET", "/api/metrics/funnel?days=1")
    return {m["key"]: m for m in (rep or {}).get("supply") or []}


print(f"Majo Phase 4A matching + supply metrics E2E · run {RUN} · backend {BASE}")

print("\n1. Vendor sends 3 photos + 'Imported ladies bags ₦18,500'")
Q = "23472" + RUN + "71"
queen = vendor("Bag Queen " + RUN, Q, ["fashion"])
twin = vendor("Bag Twin " + RUN, "23472" + RUN + "72", ["fashion"])
for i, c in enumerate([(220, 40, 90), (40, 40, 220), (240, 200, 30)], 1):
    open(os.path.join(MEDIA_DIR, f"b{RUN}{i}.bin"), "wb").write(png(c))
    open(os.path.join(MEDIA_DIR, f"b{RUN}{i}.mime"), "w").write("image/png")
for i in (1, 2, 3):
    ok(webhook(Q, f"wamid.{RUN}.b{i}", "image", media_id=f"b{RUN}{i}") == 200, f"photo {i} accepted")
    time.sleep(0.3)
ok(webhook(Q, f"wamid.{RUN}.b4", "text", "Imported ladies bags ₦18,500") == 200, "caption accepted")
ls = wait(lambda: (api("GET", f"/api/vendors/{queen}/supply-listings")[1] or []))
ok(len(ls) == 1 and ls[0]["status"] == "DRAFT", "exactly one DRAFT listing", ls[0]["public_reference"] if ls else "none")
L = ls[0]
ok(sql(f"SELECT count(*) FROM supply_signals WHERE vendor_id = '{queen}'") == "1", "exactly one SupplySignal")
ok(sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{queen}' AND status = 'AVAILABLE'") == "3", "3 stored MediaAssets")
ok(len(L["media_references"]) == 3 and L["price"] == 18500 and L["product"] and L["location"] == "Benin City",
   "draft has media, price, product and location", f"{L['product']} ₦{L['price']} {L['location']}")

print("\n2. Buyer asks; operator confirms")
B = "23472" + RUN + "79"
ok(webhook(B, f"wamid.{RUN}.ask", "text", "I need ladies bags, budget 20k") == 200, "buyer message accepted")


def find_request():
    _, reqs = api("GET", "/api/requests")
    mine = [r for r in (reqs or []) if r.get("raw_message") == "I need ladies bags, budget 20k" and r["status"] == "NEEDS_REVIEW"]
    return mine[0] if mine else None


req = wait(find_request)
ok(req is not None, "request extracted", f"category={req['category']} product={req['product']} budget_max={req['budget_max']}")
rid = req["id"]
code, r = api("POST", f"/api/requests/{rid}/confirm")
ok(code == 200 and r["status"] == "CONFIRMED", "operator confirms")


def matches():
    code, m = api("GET", f"/api/requests/{rid}/matches")
    return {c["vendor_id"]: c for c in (m or [])} if code == 200 else {}


before = matches()
ok(queen in before and before[queen].get("relevant_supply") and not before[queen].get("relevant_listing"),
   "before publication: matched on the raw signal (draft listings are not evidence)", ", ".join(before[queen]["reasons"]))

print("\n3. Operator corrects and publishes")
code, d = api("GET", f"/api/supply-listings/{L['id']}")
code, e = api("PATCH", f"/api/supply-listings/{L['id']}", {"product": "Imported ladies bags", "expected_updated_at": d["updated_at"]})
ok(code == 200 and e["product"] == "Imported ladies bags", "display product corrected", f"{L['product']} -> {e['product']}")
code, p = api("POST", f"/api/supply-listings/{L['id']}/publish", {"expected_updated_at": e["updated_at"]})
ok(code == 200 and p["status"] == "ACTIVE" and p["published_at"], "published", p["public_reference"])

print("\n4. Matching now uses the published listing")
after = matches()
q = after.get(queen)
ok(q is not None and q.get("relevant_listing") and q["relevant_listing"]["public_reference"] == L["public_reference"],
   "the vendor is ranked on its published listing", q["relevant_listing"]["public_reference"] if q else "none")
ok("SUPPLY_CONFIRMED_active_listing" in q["reasons"] and "active_listing" in q["reasons"], "reasons name the listing evidence",
   ", ".join(q["reasons"]))
ok(q["breakdown"]["product"] >= before[queen]["breakdown"]["product"],
   "the corrected display product still matches as well as the raw signal did (source product used)",
   f"{before[queen]['breakdown']['product']} -> {q['breakdown']['product']}")
ok(q["within_budget"] is True and q["relevant_listing"]["price"] == 18500, "budget judged on the listing price (18,500 <= 20,000)")
ranked = sorted(after.values(), key=lambda c: -c["score"])
ok(ranked[0]["vendor_id"] == queen and (twin not in after or after[twin]["score"] < q["score"]),
   "the vendor with listing evidence ranks first", f"{q['score']} vs {after[twin]['score'] if twin in after else 'excluded'}")
ok(sql(f"SELECT count(*) FROM vendor_requests WHERE buyer_request_id = '{rid}'") == "0", "matching itself routes nothing")
code, vrs = api("POST", f"/api/requests/{rid}/route", {"vendor_ids": [queen]})
ok(code == 200, "operator routes to the vendor (existing step)")
bd = sql(f"SELECT match_breakdown->>'evidence' || '|' || (match_breakdown->>'listing_reference') FROM vendor_requests WHERE buyer_request_id = '{rid}'")
ok(bd == f"listing|{L['public_reference']}", "the vendor request records the listing evidence", bd)
ok(sql(f"SELECT payload->>'listing_evidence_count' FROM events WHERE event_type = 'VENDORS_SUGGESTED' AND entity_id = '{rid}' ORDER BY created_at DESC LIMIT 1") == "1",
   "VENDORS_SUGGESTED records the listing-evidence count")

print("\n5. Supply metrics (funnel, last day)")
m = supply_rows()
ok(len(m) == 15, "15 supply rows in the funnel report", str(len(m)))
ok(m["vendor_submissions"]["value"] >= 1, "submission counted", str(m["vendor_submissions"]["value"]))
ok(m["media_complete_rate"]["value"] >= 1, "media-complete listing counted",
   f"{m['media_complete_rate']['value']}/{m['media_complete_rate'].get('of')}")
ok(m["listings_published"]["value"] >= 1 and m["listing_approval_rate"]["value"] >= 1, "approval counted",
   f"{m['listing_approval_rate']['value']}/{m['listing_approval_rate'].get('of')}")
ok(m["usable_listings_per_active_vendor"]["value"] >= 1, "usable listing counted (now)")
print(f"\nALL {checks} CHECKS PASSED")
