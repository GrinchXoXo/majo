#!/usr/bin/env python3
"""
Phase 4A Step 4 end-to-end check: vendor WhatsApp content -> SupplySignal ->
DRAFT SupplyListing -> operator curation IN THE REAL DASHBOARD (headless
Chromium via Playwright) -> ACTIVE. Real backend binary, real PostgreSQL,
real Python extraction service, Meta-format signed webhooks; media downloads
go to scripts/e2e/fake_graph_api.py (MEDIA_DIR). Test-equivalent run, NOT
a live WhatsApp run. Stops at publication: no matching, offers, buyers or
transactions.

Proves:
  * the draft appears in the default (DRAFT) queue with a thumbnail;
  * the detail shows marketplace fields next to the source evidence, the
    media previews and the source messages;
  * an operator edit + primary-photo choice is saved; a validation error is
    shown and changes nothing; a stale tab is refused;
  * publishing without operator authorization fails, shows an error and the
    listing stays DRAFT; with authorization it becomes ACTIVE once
    (published_at, one SUPPLY_LISTING_PUBLISHED, audit rows);
  * rejecting another draft archives it with a reason;
  * the SupplySignal, source messages and MediaAssets are unchanged, and
    nothing was sent or created downstream.

Env: BASE (backend), DASH (dashboard), APP_SECRET, DATABASE_URL (psql),
MEDIA_DIR, OPERATOR_TOKEN (must match the backend's OPERATOR_API_TOKEN).
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
DASH = os.environ.get("DASH", "http://localhost:3000")
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
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None,
                                 method=method, headers=headers)
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
    """A valid 24x24 solid-colour PNG (no image library needed)."""
    w = h = 24
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


def evidence(listing):
    sid = listing["supply_signal_id"]
    return sql(f"""SELECT md5(string_agg(t, '|' ORDER BY t)) FROM (
        SELECT row_to_json(s)::text t FROM supply_signals s WHERE s.id = '{sid}'
        UNION ALL SELECT row_to_json(ss)::text FROM supply_signal_sources ss WHERE ss.supply_signal_id = '{sid}'
        UNION ALL SELECT row_to_json(m)::text FROM messages m JOIN supply_signal_sources ss ON ss.message_id = m.id WHERE ss.supply_signal_id = '{sid}'
        UNION ALL SELECT row_to_json(a)::text FROM media_assets a WHERE a.owner_vendor_id = '{listing['vendor_id']}') x""")


def downstream():
    return sql("""SELECT (SELECT count(*) FROM offers) || '/' || (SELECT count(*) FROM buyer_requests) || '/' ||
                  (SELECT count(*) FROM vendor_requests) || '/' || (SELECT count(*) FROM transactions) || '/' ||
                  (SELECT count(*) FROM messages WHERE direction = 'OUTBOUND')""")


print(f"Majo Phase 4A Step 4 curation E2E · run {RUN} · backend {BASE} · dashboard {DASH}")

print("\n1. Vendor content -> SupplySignal -> DRAFT (real pipeline)")
M = "23471" + RUN + "61"
vm = vendor("Mike Gadgets " + RUN, M, "Benin City")
colours = [(200, 30, 30), (30, 160, 60), (40, 70, 200)]
for i, c in enumerate(colours, 1):
    open(os.path.join(MEDIA_DIR, f"c{RUN}{i}.bin"), "wb").write(png(c))
    open(os.path.join(MEDIA_DIR, f"c{RUN}{i}.mime"), "w").write("image/png")
for i in (1, 2, 3):
    ok(webhook(M, f"wamid.{RUN}.c{i}", "image", media_id=f"c{RUN}{i}") == 200, f"photo {i} accepted")
    time.sleep(0.3)
ok(webhook(M, f"wamid.{RUN}.c4", "text", "iPhone 13 128GB ₦620,000 Brand new") == 200, "caption accepted")
ls = wait(lambda: listings(vm))
ok(len(ls) == 1 and ls[0]["status"] == "DRAFT" and len(ls[0]["media_references"]) == 3, "a DRAFT listing with 3 photos exists",
   ls[0]["public_reference"] if ls else "none")
L = ls[0]
REF, LID = L["public_reference"], L["id"]
ok(sql(f"SELECT count(*) FROM events WHERE event_type='SUPPLY_LISTING_PUBLISHED' AND entity_id='{LID}'") == "0",
   "draft creation emitted no SUPPLY_LISTING_PUBLISHED")
evidence_before = evidence(L)
assets_before = sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{vm}'")
downstream_before = downstream()

P = "23471" + RUN + "62"
vp = vendor("Port Vendor " + RUN, P, "")
ok(webhook(P, f"wamid.{RUN}.p1", "text", "PS5 850k Port Harcourt") == 200, "second vendor sends a text-only offer")
lp = wait(lambda: listings(vp))
ok(len(lp) == 1 and lp[0]["status"] == "DRAFT", "second DRAFT (no media)", lp[0]["public_reference"] if lp else "none")
L2 = lp[0]
evidence2_before = evidence(L2)

print("\n2. API boundary (no dashboard)")
code, body = api("POST", f"/api/supply-listings/{LID}/publish", {}, token="")
ok(code == 401 and body.get("code") == "UNAUTHORIZED", "publish without the operator token -> 401", str(code))
code, body = api("PATCH", f"/api/supply-listings/{LID}", {"status": "ACTIVE"})
ok(code == 400, "PATCH {status: ACTIVE} is refused (status is not an editable field)", str(code))
ok(sql(f"SELECT status FROM supply_listings WHERE id='{LID}'") == "DRAFT", "still DRAFT")

print("\n3. Operator curation in the dashboard (headless Chromium)")
try:
    from playwright.sync_api import sync_playwright
except ImportError:
    print("  playwright is not installed: pip install playwright (browsers: playwright install chromium)")
    sys.exit(2)

with sync_playwright() as pw:
    browser = pw.chromium.launch()
    page = browser.new_page()
    page.set_default_timeout(15000)

    page.goto(f"{DASH}/offers")
    page.get_by_label("Operator token").fill(TOKEN)
    page.get_by_role("button", name="Use token").click()
    page.wait_for_selector("text=Operator token set")
    row = page.locator("[data-testid=listing-row]", has_text=REF)
    row.wait_for()
    ok(row.count() == 1, "the draft is in the default queue (DRAFT)", REF)
    ok(page.locator("[data-testid=listing-row]", has_text=L2["public_reference"]).count() == 1, "the second draft too")
    row.locator("[data-testid=media-image]").wait_for()
    ok(row.locator("[data-testid=media-image]").count() == 1, "queue row shows a thumbnail")

    row.get_by_role("link", name=REF).click()
    page.wait_for_selector("[data-testid=public-ref]")
    ok(page.locator("[data-testid=public-ref]").inner_text() == REF, "detail opens on the listing", REF)
    page.locator("[data-testid=media-list] [data-testid=media-image]").nth(2).wait_for()
    ok(page.locator("[data-testid=media-list] [data-testid=media-image]").count() == 3, "all 3 photos render as previews")
    ok(page.locator("[data-testid=source-messages] tbody tr").count() == 4, "4 source messages shown (3 photos + caption)")
    ok(page.locator("[data-testid=signal-id]").inner_text() == L["supply_signal_id"], "source SupplySignal shown")
    ok(page.locator("text=iPhone 13 128GB ₦620,000 Brand new").count() >= 1, "the vendor's original words are shown")

    # Validation error: shown, nothing saved.
    page.fill("#f-price", "-5")
    page.click("[data-testid=save]")
    page.wait_for_selector("[data-testid=notice-error]")
    ok("price" in page.locator("[data-testid=notice-error]").inner_text(), "negative price -> validation error displayed")
    ok(sql(f"SELECT price FROM supply_listings WHERE id='{LID}'") == "620000", "and nothing was saved")
    page.get_by_role("button", name="Discard").click()

    # Edit + primary photo 2.
    page.fill("#f-product", "Apple iPhone 13 128GB")
    page.locator("[data-testid=media-item]").nth(1).locator("[data-testid=make-primary]").click()
    page.click("[data-testid=save]")
    page.wait_for_selector("[data-testid=notice-ok]")
    ok("saved" in page.locator("[data-testid=notice-ok]").inner_text(), "operator edit saved")
    _, d = api("GET", f"/api/supply-listings/{LID}")
    ok(d["product"] == "Apple iPhone 13 128GB" and d["status"] == "DRAFT", "product corrected, still DRAFT", d["product"])
    ok(d["primary_media_asset_id"] == L["media_references"][1]["asset_id"], "photo 2 is the primary image")
    ok([m["asset_id"] for m in d["media_references"]] == [m["asset_id"] for m in L["media_references"]],
       "media kept: same 3 assets, same order")

    # Stale tab.
    stale = browser.new_page()
    stale.goto(f"{DASH}/offers/{LID}")
    stale.wait_for_selector("[data-testid=public-ref]")
    stale.get_by_label("Operator token").fill(TOKEN)  # the token is per browser tab (sessionStorage)
    stale.get_by_role("button", name="Use token").click()
    page.fill("#f-condition", "Brand new, sealed")
    page.click("[data-testid=save]")
    page.wait_for_function("document.querySelector('[data-testid=notice-ok]')?.innerText.includes('saved')")
    stale.fill("#f-condition", "Used")
    stale.click("[data-testid=save]")
    stale.wait_for_selector("[data-testid=notice-error]")
    msg = stale.locator("[data-testid=notice-error]").inner_text()
    ok("another tab" in msg, "an older tab cannot overwrite newer edits", msg)
    ok(sql(f"SELECT condition FROM supply_listings WHERE id='{LID}'") == "Brand new, sealed", "newer edit kept")
    stale.close()

    # Publish without authorization: error shown, still DRAFT.
    page.get_by_role("button", name="Clear").click()
    page.reload()
    page.wait_for_selector("[data-testid=publish]")
    page.click("[data-testid=publish]")
    page.wait_for_selector("[data-testid=notice-error]")
    ok("authorization" in page.locator("[data-testid=notice-error]").inner_text().lower(), "unauthorized publish -> error shown")
    ok(page.locator("[data-testid=notice-ok]").count() == 0 and page.locator("[data-testid=status]").inner_text() == "DRAFT",
       "no false success; page still shows DRAFT")
    ok(sql(f"SELECT status FROM supply_listings WHERE id='{LID}'") == "DRAFT", "database still DRAFT")

    # Publish with authorization.
    page.get_by_label("Operator token").fill(TOKEN)
    page.get_by_role("button", name="Use token").click()
    page.click("[data-testid=publish]")
    page.wait_for_selector("[data-testid=notice-ok]")
    ok(f"Published {REF}" in page.locator("[data-testid=notice-ok]").inner_text(), "publish succeeded in the dashboard")
    ok(page.locator("[data-testid=status]").inner_text() == "ACTIVE", "page shows ACTIVE")
    ok(page.locator("[data-testid=published-at]").inner_text() != "no", "page shows the publication time")

    # Reject the second draft.
    page.goto(f"{DASH}/offers/{L2['id']}")
    page.wait_for_selector("[data-testid=public-ref]")
    ok(page.locator("text=This listing has no media.").count() == 1, "a listing without media renders cleanly")
    page.fill("#reject-reason", "Not enough detail")
    page.click("[data-testid=reject]")
    page.wait_for_selector("[data-testid=notice-ok]")
    ok("rejected" in page.locator("[data-testid=notice-ok]").inner_text().lower(), "reject succeeded in the dashboard")
    ok("REJECTED" in page.locator("[data-testid=status]").inner_text(), "page shows REJECTED (ARCHIVED)")

    # Queue afterwards.
    page.goto(f"{DASH}/offers")
    page.wait_for_selector("[data-testid=listing-queue]")
    time.sleep(1)
    ok(page.locator("[data-testid=listing-row]", has_text=REF).count() == 0
       and page.locator("[data-testid=listing-row]", has_text=L2["public_reference"]).count() == 0,
       "neither listing is in the DRAFT queue any more")
    page.get_by_label("Status filter").select_option("ACTIVE")
    page.locator("[data-testid=listing-row]", has_text=REF).wait_for()
    ok(True, "the published listing is under ACTIVE")
    browser.close()

print("\n4. Database after curation")
row = sql(f"SELECT status, published_at IS NOT NULL, product, price, location, category, public_reference FROM supply_listings WHERE id='{LID}'")
ok(row.startswith("ACTIVE|t|Apple iPhone 13 128GB|620000|Benin City|electronics|" + REF), "ACTIVE, published_at, fields, L-ref", row)
ok(sql(f"SELECT count(*) FROM events WHERE event_type='SUPPLY_LISTING_PUBLISHED' AND entity_id='{LID}'") == "1",
   "exactly one SUPPLY_LISTING_PUBLISHED")
ok(sql(f"SELECT count(*) FROM audit_logs WHERE action='SUPPLY_LISTING_PUBLISHED' AND entity_id='{LID}'") == "1", "publication audited")
ok(sql(f"SELECT count(*) FROM audit_logs WHERE action='SUPPLY_LISTING_CORRECTED' AND entity_id='{LID}'") == "2",
   "both edits audited (old/new values)")
ok(sql(f"SELECT previous_value->>'product' || ' -> ' || (new_value->>'product') FROM audit_logs WHERE action='SUPPLY_LISTING_CORRECTED' AND entity_id='{LID}' AND new_value ? 'product'")
   == "iphone 13 -> Apple iPhone 13 128GB", "audit records old and new product")
ok(sql(f"SELECT product FROM supply_signals WHERE id='{L['supply_signal_id']}'") == "iphone 13", "the SupplySignal still says 'iphone 13'")
ok(evidence(L) == evidence_before, "signal, source messages and MediaAssets unchanged (row hash)")
ok(sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{vm}'") == assets_before, "no MediaAsset created or deleted")
row2 = sql(f"SELECT status, rejected_at IS NOT NULL, rejection_reason FROM supply_listings WHERE id='{L2['id']}'")
ok(row2 == "ARCHIVED|t|Not enough detail", "second listing ARCHIVED with reason", row2)
ok(sql(f"SELECT count(*) FROM events WHERE event_type='SUPPLY_LISTING_REJECTED' AND entity_id='{L2['id']}'") == "1", "one SUPPLY_LISTING_REJECTED")
ok(evidence(L2) == evidence2_before, "rejected listing's evidence unchanged")
ok(downstream() == downstream_before, "no offers / requests / transactions / outbound messages", downstream())
code, again = api("POST", f"/api/supply-listings/{LID}/publish", {})
ok(code == 200 and sql(f"SELECT count(*) FROM events WHERE event_type='SUPPLY_LISTING_PUBLISHED' AND entity_id='{LID}'") == "1",
   "publishing again is a no-op")
print(f"\nALL {checks} CHECKS PASSED")
