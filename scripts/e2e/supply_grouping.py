#!/usr/bin/env python3
"""
Phase 4A.2 end-to-end check: vendor media + captions + text -> grouped
SupplySignal, driven through the real backend binary and the real Python
extraction service, with Meta-format webhooks signed with
X-Hub-Signature-256. Media downloads go to scripts/e2e/fake_graph_api.py
(MEDIA_DIR). This is a test-equivalent run, NOT a live WhatsApp run.

Scenarios (Phase 4A.2 prompt §70-§74):
  A. three photos then "Imported ladies bags ₦18,500"  -> ONE signal, 3 assets
  B. "iPhone 13 128GB, 620k"                          -> immediate text signal
  C. active VendorRequest (buyer request -> confirm -> route):
       photo + "Yes"                                  -> not supply (ambiguous)
       photo + "Imported ladies bags ₦18,500"         -> supply signal
       "Yes boss, I get am for 620k"                  -> VendorResponse, not supply
       then a photo with no caption right after the reply -> stays with the reply
                                                         (VENDOR_MESSAGE_UNRESOLVED), not supply
  D. two photos, no text (proactive)                  -> ONE incomplete signal, nothing inferred

Env: BASE, APP_SECRET, DATABASE_URL (psql for read-only checks), MEDIA_DIR.
Stdlib + psql only. Exits non-zero on the first failed check.
"""
import hashlib
import hmac
import json
import os
import random
import subprocess
import sys
import time
import urllib.error
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


def wait(cond, timeout=20.0):
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
        if body:
            msg[kind]["caption"] = body
    payload = json.dumps({"object": "whatsapp_business_account", "entry": [{"changes": [{"field": "messages", "value": {
        "messaging_product": "whatsapp", "contacts": [{"wa_id": frm, "profile": {"name": "V" + frm[-2:]}}], "messages": [msg]}}]}]}).encode()
    sig = "sha256=" + hmac.new(SECRET.encode(), payload, hashlib.sha256).hexdigest()
    req = urllib.request.Request(BASE + "/webhooks/whatsapp", data=payload, method="POST",
                                 headers={"Content-Type": "application/json", "X-Hub-Signature-256": sig})
    with urllib.request.urlopen(req) as r:
        return r.status


def photo(media_id, tag):
    open(os.path.join(MEDIA_DIR, media_id + ".bin"), "wb").write(b"\xff\xd8\xff\xe0" + tag.encode() + os.urandom(48))


def vendor(name, phone):
    _, v = api("POST", "/api/vendors", {"business_name": name, "whatsapp_external_id": phone, "primary_location": "Benin City",
                                         "categories": ["fashion", "phones"]})
    api("PATCH", f"/api/vendors/{v['id']}", {"status": "ACTIVE"})
    return v["id"]


def signals(vid):
    rows = sql(f"""SELECT s.id, COALESCE(s.product,''), COALESCE(s.category,''), COALESCE(s.price::text,''), s.currency,
                          s.confidence, s.raw_message, COALESCE(s.extracted_data->>'grouping','')
                   FROM supply_signals s WHERE s.vendor_id = '{vid}' ORDER BY s.created_at""")
    out = []
    for line in rows.splitlines():
        sid, product, category, price, cur, conf, raw, grouping = line.split("|", 7)
        out.append(dict(id=sid, product=product, category=category, price=price, currency=cur, confidence=conf, raw=raw, grouping=grouping))
    return out


def evidence(sid):
    msgs = sql(f"""SELECT m.external_message_id FROM supply_signal_sources ss JOIN messages m ON m.id = ss.message_id
                   WHERE ss.supply_signal_id = '{sid}' ORDER BY ss.position""").splitlines()
    assets = sql(f"""SELECT ss.media_asset_id FROM supply_signal_sources ss WHERE ss.supply_signal_id = '{sid}'
                     AND ss.media_asset_id IS NOT NULL ORDER BY ss.position""").splitlines()
    return msgs, assets


def asset_of(wamid):
    return sql(f"SELECT COALESCE(media_asset_id::text,'') FROM messages WHERE external_message_id = '{wamid}'")


print(f"Majo Phase 4A.2 supply grouping E2E · run {RUN} · backend {BASE}")

# --- A ---------------------------------------------------------------------
print("\nA. Three photos + caption -> one SupplySignal (§70)")
A = "23470" + RUN + "41"
va = vendor("Ada Bags " + RUN, A)
for i in (1, 2, 3):
    photo(f"a{RUN}{i}", f"bag-{i}-{RUN}")
for i in (1, 2, 3):
    ok(webhook(A, f"wamid.{RUN}.a{i}", "image", media_id=f"a{RUN}{i}") == 200, f"photo {i} accepted")
    time.sleep(0.3)
ok(webhook(A, f"wamid.{RUN}.a4", "text", "Imported ladies bags ₦18,500") == 200, "caption text accepted")
ok(webhook(A, f"wamid.{RUN}.a2", "image", media_id=f"a{RUN}2") == 200, "photo 2 redelivered by Meta")
sig = wait(lambda: signals(va))
ok(len(sig) == 1, "exactly one SupplySignal", str(len(sig)))
s = sig[0]
ok(s["product"] == "bag" and s["category"] == "fashion" and s["price"] == "18500" and s["currency"] == "NGN",
   "real extractor: product / category / price / currency", f"{s['product']} / {s['category']} / {s['price']} {s['currency']}")
ok(s["raw"] == "Imported ladies bags ₦18,500" and s["confidence"] == "0.8", "raw evidence and existing confidence kept",
   f"confidence={s['confidence']}")
msgs, assets = evidence(s["id"])
ok(msgs == [f"wamid.{RUN}.a{i}" for i in (1, 2, 3, 4)], "source_message_ids = the 4 messages in send order", ", ".join(m[-3:] for m in msgs))
ok(assets == [asset_of(f"wamid.{RUN}.a{i}") for i in (1, 2, 3)] and all(assets), "media_asset_ids = the 3 stored assets, in order")
ev = sql(f"""SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_CREATED' AND entity_id = '{s['id']}'
             AND jsonb_array_length(payload->'media_asset_ids') = 3 AND jsonb_array_length(payload->'source_message_ids') = 4""")
ok(ev == "1", "one SUPPLY_SIGNAL_CREATED carrying the provenance")
time.sleep(3)
ok(len(signals(va)) == 1, "still one signal after every grouping job ran (no duplicates)")

# --- B ---------------------------------------------------------------------
print("\nB. Text-only supply still immediate (§71)")
B = "23470" + RUN + "42"
vb = vendor("Mike Gadgets " + RUN, B)
ok(webhook(B, f"wamid.{RUN}.b1", "text", "iPhone 13 128GB, 620k") == 200, "text accepted")
sig = wait(lambda: signals(vb), timeout=5)
ok(len(sig) == 1 and sig[0]["product"] == "iphone 13" and sig[0]["price"] == "620000" and sig[0]["grouping"] == "text_only",
   "one text signal, existing extraction", f"{sig[0]['product']} {sig[0]['price']}")
ok(evidence(sig[0]["id"])[0] == [f"wamid.{RUN}.b1"], "its source message is recorded")

# --- C ---------------------------------------------------------------------
print("\nC. Active VendorRequest: replies stay replies, unrelated supply is supply (§72-74)")
C = "23470" + RUN + "43"
vc = vendor("Chi Phones " + RUN, C)
BUYER = "23480" + RUN + "01"
ask = "I need an iPhone 13 128GB, budget 650k."
ok(webhook(BUYER, f"wamid.{RUN}.ask", "text", ask) == 200, "buyer asks for an iPhone 13")


def find_request():
    _, reqs = api("GET", "/api/requests")
    mine = [r for r in (reqs or []) if r.get("raw_message") == ask]
    return mine[0] if mine and mine[0]["status"] == "NEEDS_REVIEW" else None


req = wait(find_request)
ok(req is not None, "buyer request extracted")
code, _ = api("POST", f"/api/requests/{req['id']}/confirm")
ok(code == 200, "operator confirms")
code, vrs = api("POST", f"/api/requests/{req['id']}/route", {"vendor_ids": [vc]})
ok(code == 200 and vrs[0]["status"] == "SENT", "routed: vendor C has an active VendorRequest", vrs[0]["public_reference"])
photo(f"c{RUN}yes", "yes-" + RUN)
photo(f"c{RUN}bag", "bag-" + RUN)
ok(webhook(C, f"wamid.{RUN}.c1", "image", "Yes", media_id=f"c{RUN}yes") == 200, "vendor: photo + 'Yes'")
ok(webhook(C, f"wamid.{RUN}.c2", "image", "Imported ladies bags ₦18,500", media_id=f"c{RUN}bag") == 200, "vendor: photo + unrelated bags caption")
cand = sql(f"""SELECT m.external_message_id || ':' || c.reason FROM supply_candidate_messages c JOIN messages m ON m.id = c.message_id
               WHERE c.vendor_id = '{vc}' ORDER BY c.message_at""")
ok(cand == f"wamid.{RUN}.c2:caption_has_product_and_price_unrelated_to_active_request",
   "only the bags photo is supply content; 'Yes' photo is not", cand.replace(f"wamid.{RUN}.", ""))
print("    (a captioned photo stays open for the grouping window in case more photos follow ...)")
sig = wait(lambda: signals(vc), timeout=float(os.environ.get("WINDOW_WAIT", "200")))
ok(len(sig) == 1 and sig[0]["product"] == "bag" and evidence(sig[0]["id"])[0] == [f"wamid.{RUN}.c2"],
   "one supply signal from the bags photo only")
ok(webhook(C, f"wamid.{RUN}.c3", "text", "Yes boss, I get am for 620k") == 200, "vendor replies to the request")
resp = wait(lambda: sql(f"""SELECT r.status || ' ' || vr.public_reference FROM vendor_responses r
                            JOIN vendor_requests vr ON vr.id = r.vendor_request_id
                            WHERE r.raw_message = 'Yes boss, I get am for 620k' AND vr.vendor_id = '{vc}'"""))
ok(bool(resp), "the reply is a VendorResponse on the active request", resp)
photo(f"c{RUN}after", "after-" + RUN)
ok(webhook(C, f"wamid.{RUN}.c4", "image", media_id=f"c{RUN}after") == 200, "vendor: photo (no caption) right after the reply")
unres = wait(lambda: sql(f"""SELECT e.payload->>'reason' FROM events e JOIN messages m ON m.id = e.entity_id
                             WHERE e.event_type = 'VENDOR_MESSAGE_UNRESOLVED' AND m.external_message_id = 'wamid.{RUN}.c4'"""))
ok(unres == "uncaptioned_media_right_after_a_vendor_reply", "the photo stays with the reply (unresolved, for review)", unres)
time.sleep(1)
ok(len(signals(vc)) == 1, "neither the reply nor its photo created a SupplySignal")
ok(bool(wait(lambda: asset_of(f"wamid.{RUN}.c1"))), "the ambiguous 'Yes' photo is still stored (4A.1)")

# --- D ---------------------------------------------------------------------
print("\nD. Proactive photos without any text -> one incomplete signal, nothing invented")
D = "23470" + RUN + "44"
vd = vendor("Dami " + RUN, D)
photo(f"d{RUN}1", "d1-" + RUN)
photo(f"d{RUN}2", "d2-" + RUN)
ok(webhook(D, f"wamid.{RUN}.d1", "image", media_id=f"d{RUN}1") == 200, "photo 1")
ok(webhook(D, f"wamid.{RUN}.d2", "image", media_id=f"d{RUN}2") == 200, "photo 2")
ok(bool(wait(lambda: asset_of(f"wamid.{RUN}.d1") and asset_of(f"wamid.{RUN}.d2"))), "both photos stored")
print("    (waiting for the grouping window to elapse ...)")
done = wait(lambda: sql(f"""SELECT count(*) FROM jobs j JOIN messages m ON j.idempotency_key = m.id::text || ':window'
                            WHERE m.external_message_id IN ('wamid.{RUN}.d1','wamid.{RUN}.d2') AND j.status = 'SUCCEEDED'""") == "2",
            timeout=float(os.environ.get("WINDOW_WAIT", "200")))
ok(bool(done), "window evaluation ran for both photos")
sd = signals(vd)
ok(len(sd) == 1 and sd[0]["product"] == "" and sd[0]["price"] == "" and sd[0]["confidence"] == "0" and sd[0]["raw"] == "",
   "one incomplete signal: no product, price or confidence invented", sd[0]["grouping"] if sd else "none")
ok(evidence(sd[0]["id"])[1] == [asset_of(f"wamid.{RUN}.d1"), asset_of(f"wamid.{RUN}.d2")], "it references both stored photos, in order")

print("\nScope")
ok(sql(f"SELECT count(*) FROM supply_listings WHERE vendor_id IN ('{va}','{vb}','{vc}','{vd}')") == "0", "no SupplyListing created")
print(f"\nALL {checks} CHECKS PASSED")
