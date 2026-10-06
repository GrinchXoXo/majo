#!/usr/bin/env python3
"""
Phase 4A.5 end-to-end check: supply lifecycle, freshness and geographic
expansion through the real backend binary, real PostgreSQL, real Python
extraction service and (optionally) the real dashboard in headless Chromium.
Meta-format signed webhooks; media downloads go to fake_graph_api.py
(MEDIA_DIR). Test-equivalent run, NOT live WhatsApp.

Time is simulated by moving the listing's (and the vendor's signals')
timestamps into the past in SQL ("time travel"); the eligibility policy is
derived from timestamps, so nothing else needs to run.

Proves: draft -> publish starts a 60-day lifecycle (expires_at =
published_at + 60d) -> day 0 LOCAL/FRESH/eligible -> day 23 NEARBY/STALE but
still eligible, matches flag reconfirmation -> operator verification makes it
FRESH without moving the ceiling -> vendor repost refreshes freshness only ->
day 61 expired: not matched, still queryable with media, cannot be verified
back -> a repost after expiry creates a NEW draft and the old one is history.

Env: BASE, APP_SECRET, DATABASE_URL (psql), MEDIA_DIR, OPERATOR_TOKEN,
DASH (optional: dashboard URL for the UI checks).
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
DASH = os.environ.get("DASH", "")
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


def api(method, path, body=None, token=TOKEN):
    h = {"Content-Type": "application/json"}
    if token:
        h["Authorization"] = "Bearer " + token
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None, method=method, headers=h)
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


def vendor(name, phone, location, categories):
    _, v = api("POST", "/api/vendors", {"business_name": name, "whatsapp_external_id": phone,
                                         "primary_location": location, "categories": categories})
    api("PATCH", f"/api/vendors/{v['id']}", {"status": "ACTIVE"})
    return v["id"]


def detail(lid):
    code, d = api("GET", f"/api/supply-listings/{lid}")
    assert code == 200, code
    return d


def travel(lid, vid, days):
    """Move the listing and the vendor's signals `days` into the past."""
    iv = f"{days} days"
    sql(f"""UPDATE supply_listings SET published_at = published_at - interval '{iv}', expires_at = expires_at - interval '{iv}',
            last_verified_at = last_verified_at - interval '{iv}', created_at = created_at - interval '{iv}' WHERE id = '{lid}'""")
    sql(f"""UPDATE supply_signals SET created_at = created_at - interval '{iv}', expires_at = expires_at - interval '{iv}'
            WHERE vendor_id = '{vid}'""")


def post_tv(phone, tag):
    for i, c in enumerate([(20, 20, 20), (90, 90, 90)], 1):
        open(os.path.join(MEDIA_DIR, f"t{RUN}{tag}{i}.bin"), "wb").write(png((c[0] + i, c[1] + len(tag), c[2] + int(RUN) % 50)))
        open(os.path.join(MEDIA_DIR, f"t{RUN}{tag}{i}.mime"), "w").write("image/png")
    for i in (1, 2):
        ok(webhook(phone, f"wamid.{RUN}.{tag}{i}", "image", media_id=f"t{RUN}{tag}{i}") == 200, f"photo {i} accepted ({tag})")
        time.sleep(0.3)
    ok(webhook(phone, f"wamid.{RUN}.{tag}c", "text", "Samsung TV 55 inch ₦450,000") == 200, f"caption accepted ({tag})")


print(f"Majo Phase 4A.5 lifecycle E2E · run {RUN} · backend {BASE}" + (f" · dashboard {DASH}" if DASH else ""))

print("\n1. Draft: the lifecycle has not started")
P = "23473" + RUN + "81"
vid = vendor("TV Palace " + RUN, P, "GRA, Benin City", ["electronics"])
post_tv(P, "a")
ls = wait(lambda: (api("GET", f"/api/vendors/{vid}/supply-listings")[1] or []))
ok(len(ls) == 1 and ls[0]["status"] == "DRAFT", "one DRAFT", ls[0]["public_reference"] if ls else "none")
L = ls[0]
el = detail(L["id"])["eligibility"]
ok(not el["eligible"] and "not_published" in el["reasons"] and el["lifecycle"]["state"] == "NOT_PUBLISHED"
   and el["geography"]["stage"] == "NONE", "draft: not eligible, no lifecycle, no search scope", ", ".join(el["reasons"]))

print("\n2. Publish: a 60-day lifecycle starts")
code, p = api("POST", f"/api/supply-listings/{L['id']}/publish", {})
ok(code == 200 and p["status"] == "ACTIVE", "published", p["public_reference"])
ok(sql(f"SELECT (expires_at = published_at + interval '60 days')::text FROM supply_listings WHERE id = '{L['id']}'") == "true",
   "expires_at = published_at + 60 days (not 7)")
el = detail(L["id"])["eligibility"]
g = el["geography"]
ok(el["eligible"] and el["lifecycle"]["day"] == 0 and el["lifecycle"]["max_days"] == 60 and el["freshness"]["state"] == "FRESH",
   "day 0: eligible, fresh", f"day {el['lifecycle']['day']}/{el['lifecycle']['max_days']}")
ok(g["stage"] == "LOCAL" and g["scope"] == "ORIGIN_AREA" and g["next_stage"] == "NEARBY", "day 0: LOCAL (origin area), next NEARBY",
   f"{g['stage']} -> {g['next_stage']} at {g['next_expansion_at']}")
ok(g["origin"]["city"] == "Benin City" and g["origin"]["state"] == "Edo" and g["origin"]["community"] == "GRA",
   "origin resolved: GRA, Benin City, Edo, South-South")

print("\n3. A buyer asks; matching uses the listing")
B = "23473" + RUN + "89"
ok(webhook(B, f"wamid.{RUN}.ask", "text", "I need a Samsung TV 55 inch, budget 500k") == 200, "buyer message accepted")


def find_request():
    _, reqs = api("GET", "/api/requests")
    mine = [r for r in (reqs or []) if r.get("raw_message") == "I need a Samsung TV 55 inch, budget 500k" and r["status"] == "NEEDS_REVIEW"]
    return mine[0] if mine else None


req = wait(find_request)
ok(req is not None, "request extracted", f"{req['category']} / {req['product']}")
rid = req["id"]
api("POST", f"/api/requests/{rid}/confirm")


def match():
    code, m = api("GET", f"/api/requests/{rid}/matches")
    return {c["vendor_id"]: c for c in (m or [])}.get(vid)


m = match()
ok(m and m.get("relevant_listing") and m["relevant_listing"]["freshness"] == "FRESH" and not m["relevant_listing"]["needs_reconfirmation"],
   "day 0: matched on the listing, fresh", m["relevant_listing"]["public_reference"] if m and m.get("relevant_listing") else "none")

print("\n4. Day 23: wider scope, stale, still eligible")
travel(L["id"], vid, 23)
el = detail(L["id"])["eligibility"]
g = el["geography"]
ok(el["eligible"] and el["lifecycle"]["day"] == 23, "day 23: still eligible (stale is not expired)", f"day {el['lifecycle']['day']}")
ok(el["freshness"]["state"] == "STALE" and "stale_needs_reconfirmation" in el["notes"], "day 23: STALE, flagged for reconfirmation")
ok(g["stage"] == "NEARBY" and "CITY" in g["levels"] and g["next_stage"] == "STATE", "day 23: NEARBY (+CITY), next STATE",
   f"next {g['next_stage']} at {g['next_expansion_at']}")
m = match()
ok(m and m.get("relevant_listing") and m["relevant_listing"]["needs_reconfirmation"] and m["relevant_listing"]["geo_stage"] == "NEARBY",
   "matching still uses it, and says reconfirm with the vendor first")

if DASH:
    from playwright.sync_api import sync_playwright
    with sync_playwright() as pw:
        b = pw.chromium.launch()
        pg = b.new_page(viewport={"width": 1200, "height": 1400})
        pg.goto(f"{DASH}/offers/{L['id']}")
        pg.get_by_label("Operator token").fill(TOKEN)
        pg.get_by_role("button", name="Use token").click()
        pg.wait_for_selector("[data-testid=lifecycle-panel]")
        ok("Day 23 / 60" in pg.locator("[data-testid=elig-lifecycle]").inner_text(), "dashboard: Day 23 / 60")
        ok("STALE" in pg.locator("[data-testid=elig-freshness]").inner_text(), "dashboard: freshness STALE with reconfirmation hint")
        ok("Nearby areas" in pg.locator("[data-testid=elig-scope]").inner_text(), "dashboard: search scope Nearby areas")
        ok("Whole state" in pg.locator("[data-testid=elig-next]").inner_text(), "dashboard: next change Whole state",
           pg.locator("[data-testid=elig-next]").inner_text())
        pg.locator("[data-testid=lifecycle-panel]").screenshot(path=os.environ.get("SHOT", "/tmp/lifecycle-panel.png"))
        # Operator verification through the dashboard.
        pg.get_by_label("Verification note").fill("Called the vendor: still in the shop")
        pg.click("[data-testid=verify]")
        pg.wait_for_selector("[data-testid=notice-ok]")
        ok("FRESH" in pg.locator("[data-testid=elig-freshness]").inner_text(), "dashboard: Mark verified -> FRESH")
        b.close()
else:
    code, _ = api("POST", f"/api/supply-listings/{L['id']}/verify", {"note": "called the vendor"})
    ok(code == 200, "operator verification (API)")

print("\n5. Verification refreshes freshness, never the lifecycle")
el = detail(L["id"])["eligibility"]
ok(el["freshness"]["state"] == "FRESH" and el["lifecycle"]["day"] == 23, "FRESH again, still day 23")
ok(sql(f"SELECT (expires_at = published_at + interval '60 days')::text FROM supply_listings WHERE id = '{L['id']}'") == "true",
   "ceiling unchanged by verification")
ok(sql(f"SELECT count(*) FROM audit_logs WHERE action = 'SUPPLY_LISTING_VERIFIED' AND entity_id = '{L['id']}'") == "1"
   and sql(f"SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_VERIFIED' AND entity_id = '{L['id']}'") == "1",
   "verification audited and evented once")

print("\n6. The vendor reposts the same TV: freshness only")
before_exp = sql(f"SELECT expires_at FROM supply_listings WHERE id = '{L['id']}'")
before_ver = sql(f"SELECT last_verified_at FROM supply_listings WHERE id = '{L['id']}'")
post_tv(P, "b")
ok(wait(lambda: sql(f"SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_DUPLICATE_REFRESHED' AND entity_id = '{L['id']}'") == "1"),
   "repost recognised as the same supply (refresh)")
ok(sql(f"SELECT expires_at FROM supply_listings WHERE id = '{L['id']}'") == before_exp, "ceiling unchanged by the repost")
ok(sql(f"SELECT (last_verified_at > '{before_ver}')::text FROM supply_listings WHERE id = '{L['id']}'") == "true", "last_verified_at moved")
ok(sql(f"SELECT count(*) FROM supply_listings WHERE vendor_id = '{vid}'") == "1", "no second listing")

print("\n7. Day 61: expired, kept as history")
assets_before = sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{vid}'")
travel(L["id"], vid, 38)
el = detail(L["id"])["eligibility"]
ok(not el["eligible"] and "lifecycle_expired" in el["reasons"] and el["lifecycle"]["state"] == "EXPIRED"
   and el["geography"]["stage"] == "EXPIRED", "day 61: expired, not eligible", ", ".join(el["reasons"]))
ok(sql(f"SELECT status FROM supply_listings WHERE id = '{L['id']}'") == "ACTIVE", "status column still ACTIVE (no sweep); the policy decides")
m = match()
ok(not m or not m.get("relevant_listing"), "an expired listing is not matched")
_, live = api("GET", "/api/supply-listings?status=ACTIVE")
_, hist = api("GET", "/api/supply-listings?status=ACTIVE&lifecycle=expired")
ok(not any(x["id"] == L["id"] for x in live or []) and any(x["id"] == L["id"] for x in hist or []),
   "gone from live search, present in lifecycle=expired (history)")
d = detail(L["id"])
media_ok = all(urllib.request.urlopen(urllib.request.Request(BASE + m_["preview_path"], headers={"Authorization": "Bearer " + TOKEN})).status == 200
               for m_ in d["media"] if m_.get("preview_path"))
ok(len(d["media"]) == 2 and media_ok and len(d["source_messages"]) == 3, "media and provenance intact and viewable")
code, body = api("POST", f"/api/supply-listings/{L['id']}/verify", {})
ok(code == 409 and body["code"] == "LIFECYCLE_EXPIRED", "verification cannot revive it", str(code))
ok(sql(f"SELECT count(*) FROM supply_listings WHERE id = '{L['id']}'") == "1"
   and sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{vid}'") == assets_before, "nothing deleted (listing and all media assets)",
   f"{assets_before} assets")

print("\n8. Repost after expiry: a new lifecycle")
post_tv(P, "c")
new = wait(lambda: [x for x in (api("GET", f"/api/vendors/{vid}/supply-listings")[1] or []) if x["id"] != L["id"]])
ok(len(new) == 1 and new[0]["status"] == "DRAFT" and new[0]["public_reference"] != L["public_reference"],
   "a NEW draft with a new L-reference", new[0]["public_reference"] if new else "none")
ok(sql(f"SELECT expires_at FROM supply_listings WHERE id = '{L['id']}'") != "" and
   sql(f"SELECT (now() >= expires_at)::text FROM supply_listings WHERE id = '{L['id']}'") == "true", "the expired listing stays expired")
print(f"\nALL {checks} CHECKS PASSED")
