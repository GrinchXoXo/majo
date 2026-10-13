#!/usr/bin/env python3
"""
Phase 3.1 end-to-end check: Buyer -> Request -> Vendor -> Offer delivered on
WhatsApp -> Buyer replies "1" -> Selection -> Transaction (UNPAID), plus the
§3A correctness checks, driven entirely through the real backend's public
surfaces: Meta-format webhooks signed with X-Hub-Signature-256, and the
operator REST API.

Two modes:
  * Simulated (default): run the backend with WHATSAPP_API_URL pointing at
    scripts/e2e/fake_graph_api.py and pass OUTBOUND_LOG so outbound messages
    can be asserted.
  * Live: point BASE at a backend wired to the real Cloud API. Inbound
    webhooks are still simulated here, so for a true live run use real phones
    for the buyer/vendor messages and use this script only for the checks
    that don't need OUTBOUND_LOG.

Env: BASE (default http://localhost:8080), APP_SECRET (must match backend's
WHATSAPP_APP_SECRET), OUTBOUND_LOG, FAIL_VENDOR_PHONE (a number the fake
Graph API rejects with 131047).

Stdlib only. Exits non-zero on the first failed check.
"""
import hashlib
import hmac
import json
import os
import random
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("BASE", "http://localhost:8080")
SECRET = os.environ.get("APP_SECRET", "")
OUTBOUND_LOG = os.environ.get("OUTBOUND_LOG", "")
RUN = f"{random.randint(100000, 999999)}"
BUYER = "23480" + RUN + "01"
MIKE = "23470" + RUN + "11"
OJ = "23470" + RUN + "22"
DEAD = os.environ.get("FAIL_VENDOR_PHONE", "23470" + RUN + "99")

checks = 0


def ok(cond, label, detail=""):
    global checks
    checks += 1
    mark = "PASS" if cond else "FAIL"
    print(f"  [{mark}] {label}" + (f" -> {detail}" if detail else ""))
    if not cond:
        sys.exit(1)


def _hdrs():
    # Phase 4C correction: operator overrides are authenticated control-plane calls.
    h = {"Content-Type": "application/json"}
    if os.environ.get("OPERATOR_TOKEN"):
        h["Authorization"] = "Bearer " + os.environ["OPERATOR_TOKEN"]
    return h


def api(method, path, body=None, expect=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=_hdrs())
    try:
        with urllib.request.urlopen(req) as r:
            code, raw = r.status, r.read()
    except urllib.error.HTTPError as e:
        code, raw = e.code, e.read()
    out = json.loads(raw) if raw else None
    if expect is not None and code != expect:
        print(f"    unexpected {code} for {method} {path}: {out}")
    return code, out


def webhook(frm, wamid, text, sign=True, signature=None):
    payload = json.dumps({"object": "whatsapp_business_account", "entry": [{"changes": [{"field": "messages", "value": {
        "messaging_product": "whatsapp",
        "contacts": [{"wa_id": frm, "profile": {"name": "E2E " + frm[-2:]}}],
        "messages": [{"from": frm, "id": wamid, "timestamp": str(int(time.time())), "type": "text", "text": {"body": text}}],
    }}]}]}).encode()
    headers = {"Content-Type": "application/json"}
    if signature is not None:
        headers["X-Hub-Signature-256"] = signature
    elif sign:
        headers["X-Hub-Signature-256"] = "sha256=" + hmac.new(SECRET.encode(), payload, hashlib.sha256).hexdigest()
    req = urllib.request.Request(BASE + "/webhooks/whatsapp", data=payload, method="POST", headers=headers)
    try:
        with urllib.request.urlopen(req) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def outbound(to=None):
    if not OUTBOUND_LOG or not os.path.exists(OUTBOUND_LOG):
        return []
    rows = [json.loads(l) for l in open(OUTBOUND_LOG)]
    return [r for r in rows if to is None or r["to"] == to]


def last_text(to):
    rows = [r for r in outbound(to) if not r["failed"]]
    return rows[-1]["body"]["text"]["body"] if rows else ""


def wait(cond, timeout=10.0):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.2)
    return cond()


def show(label, text):
    print(f"    ── {label} ──")
    for line in text.splitlines():
        print("    │ " + line)


def vendor(name, phone, categories):
    _, v = api("POST", "/api/vendors", {"business_name": name, "whatsapp_external_id": phone,
                                         "primary_location": "Benin City", "categories": categories}, 201)
    api("PATCH", f"/api/vendors/{v['id']}", {"status": "ACTIVE"}, 200)
    return v["id"]


print(f"Majo Phase 3.1 E2E · run {RUN} · backend {BASE}")

print("\n§3A-1 Webhook authenticity")
ok(webhook(BUYER, f"wamid.{RUN}.unsigned", "I need a laptop", sign=False) == 401, "unsigned webhook rejected", "401")
ok(webhook(BUYER, f"wamid.{RUN}.forged", "I need a laptop", signature="sha256=" + "0" * 64) == 401, "forged signature rejected", "401")

print("\nSetup: vendors + supply")
mike = vendor("Mike Gadgets " + RUN, MIKE, ["electronics", "phones"])
oj = vendor("OJ Phones " + RUN, OJ, ["phones"])
dead = vendor("Dead Number " + RUN, DEAD, ["phones"])
ok(webhook(MIKE, f"wamid.{RUN}.supply", "I get iPhone 13 128gb, clean one, 620k.") == 200, "Mike's supply message accepted (signed)")

print("\n1-2. Buyer asks; BuyerRequest created + extracted")
ok(webhook(BUYER, f"wamid.{RUN}.ask", "I need an iPhone 13 128GB, budget 650k.") == 200, "buyer message accepted")
ok(wait(lambda: "looking into that" in last_text(BUYER)), "buyer acknowledged", last_text(BUYER))


def find_request():
    _, reqs = api("GET", "/api/requests")
    mine = [r for r in (reqs or []) if r.get("raw_message") == "I need an iPhone 13 128GB, budget 650k." and r["status"] == "NEEDS_REVIEW"]
    return mine[0] if mine else None


req = wait(find_request)
ok(req is not None, "request extracted to NEEDS_REVIEW",
   f"product={req['product']} variant={req['variant']} budget_max={req['budget_max']}")
rid = req["id"]

print("\n§3A-2 Confirmed-request invariant")
code, _ = api("GET", f"/api/requests/{rid}/matches")
ok(code == 409, "matching refused while NEEDS_REVIEW", str(code))
code, _ = api("POST", f"/api/requests/{rid}/route", {"vendor_ids": [mike]})
ok(code == 409, "routing refused while NEEDS_REVIEW", str(code))
code, _ = api("POST", f"/api/requests/{rid}/offers/deliver")
ok(code == 409, "offer delivery refused while NEEDS_REVIEW", str(code))
code, r = api("POST", f"/api/requests/{rid}/confirm")
ok(code == 200 and r["status"] == "CONFIRMED", "operator confirms (unchanged Phase 1 step)")

print("\n3. Matching + routing (operator-approved, unchanged)")
code, matches = api("GET", f"/api/requests/{rid}/matches")
ok(code == 200 and any(m["vendor_id"] == mike for m in matches), "matching returns Mike",
   ", ".join(f"{m['vendor_name']}={m['score']}" for m in matches))
code, vrs = api("POST", f"/api/requests/{rid}/route", {"vendor_ids": [mike, oj, dead]})
ok(code == 200 and len(vrs) == 3, "routed to 3 vendors")
by_vendor = {v["vendor_id"]: v for v in vrs}
ok(all(len(v["public_reference"]) == len("MM-123456") for v in vrs), "§3A-5 new references are 6-digit",
   ", ".join(v["public_reference"] for v in vrs))

print("\n§3A-4 Vendor-request delivery failure")
dvr = by_vendor[dead]
ok(dvr["status"] == "SEND_FAILED", "rejected send recorded as SEND_FAILED", dvr["status"])
_, vrs_now = api("GET", f"/api/requests/{rid}/vendor-requests")
dead_row = [v for v in vrs_now if v["vendor_id"] == dead][0]
ok(dead_row["send_error"] and "131047" in dead_row["send_error"], "Meta error detail persisted", dead_row["send_error"][:90])
ok(webhook(DEAD, f"wamid.{RUN}.dead", "I get Samsung S21 300k") == 200, "unreachable vendor later messages Majo")
time.sleep(1.5)
_, dsupply = api("GET", f"/api/vendors/{dead}/supply")
_, unmatched = api("GET", "/api/vendor-responses/unmatched")
ok(len(dsupply or []) == 1 and not any(u["vendor_id"] == dead for u in (unmatched or [])),
   "message became a SupplySignal, NOT a reply to the request they never received")

print("\n4-5. Vendor replies -> Offer generated -> delivered to buyer's WhatsApp")
before = len(outbound(BUYER))
ok(webhook(MIKE, f"wamid.{RUN}.mike", "Yes, 620k.") == 200, "Mike replies")
ok(wait(lambda: len(outbound(BUYER)) > before), "offer message sent to buyer automatically (no operator action)")
offer_msg = last_text(BUYER)
show("buyer receives", offer_msg)
ok("1." in offer_msg and "620,000" in offer_msg and "Reply 1" in offer_msg, "message lists option 1 with price + instruction")
for leak in ["score", "confidence", mike, rid, by_vendor[mike]["public_reference"], "+30"]:
    ok(leak not in offer_msg, f"no internal metadata leaked ({leak[:12]}…)")

before = len(outbound(BUYER))
ok(webhook(OJ, f"wamid.{RUN}.oj", "128 no dey but 256gb dey 700k.") == 200, "OJ replies with an alternative")
ok(wait(lambda: len(outbound(BUYER)) > before), "follow-up offer delivered")
follow = last_text(BUYER)
show("buyer receives", follow)
ok("2." in follow and "620,000" not in follow, "only the NEW option is sent, numbered 2")

print("\n§5 Read endpoints have no side effects")
before = len(outbound())
for _ in range(3):
    api("GET", f"/api/requests/{rid}/offers", expect=200)
    api("GET", f"/api/requests/{rid}/offers/preview", expect=200)
code, res = api("POST", f"/api/requests/{rid}/offers/deliver")
ok(len(outbound()) == before and code == 200 and res["delivered"] == 0,
   "viewing offers 3x + explicit re-deliver sends nothing new", json.dumps(res))
_, offers = api("GET", f"/api/requests/{rid}/offers")
ok([o["presentation_ordinal"] for o in offers] == [1, 2] and all(o["status"] == "SENT" for o in offers),
   "offers SENT with stable ordinals 1,2")

print("\n6-8. Buyer replies; context resolution")
ok(webhook(BUYER, f"wamid.{RUN}.yes", "yes") == 200, 'buyer says "yes"')
ok(wait(lambda: "Just to be sure" in last_text(BUYER)), "ambiguous -> clarification, no selection", last_text(BUYER))
ok(webhook(BUYER, f"wamid.{RUN}.five", "5") == 200, 'buyer says "5"')
ok(wait(lambda: "option 5" in last_text(BUYER)), "out of range -> clarification", last_text(BUYER))
_, sel = api("GET", f"/api/requests/{rid}/selection")
ok(sel["selection"] is None, "still no selection")

ok(webhook(BUYER, f"wamid.{RUN}.one", "1") == 200, 'buyer replies "1"')
ok(wait(lambda: "recorded your choice" in last_text(BUYER)), "selection confirmed to buyer")
show("buyer receives", last_text(BUYER))
conf = last_text(BUYER)
ok("Nothing has been paid" in conf and by_vendor[mike]["public_reference"] in conf,
   "confirmation uses the existing MM reference and makes no payment claim")

print("\n9-11. BuyerSelection + Transaction")
_, sel = api("GET", f"/api/requests/{rid}/selection")
chosen = [o for o in offers if o["presentation_ordinal"] == 1][0]
ok(sel["selection"] and sel["selection"]["offer_id"] == chosen["id"], "BuyerSelection references offer #1")
tx = sel["transaction"]
ok(tx and tx["payment_status"] == "UNPAID" and tx["gross_amount"] == 620000, "Transaction created, payment UNPAID",
   f"tx={tx['id']} gross={tx['gross_amount']} fee={tx['platform_fee']} status={tx['status']}")

print("\n13. Duplicate / repeated replies")
ok(webhook(BUYER, f"wamid.{RUN}.one", "1") == 200, "exact duplicate webhook (same wamid) re-delivered")
ok(webhook(BUYER, f"wamid.{RUN}.one-again", "1") == 200, 'buyer sends "1" again')
ok(wait(lambda: "already chosen option 1" in last_text(BUYER)), "idempotent re-confirmation", last_text(BUYER).splitlines()[0])
ok(webhook(BUYER, f"wamid.{RUN}.two", "2") == 200, 'buyer tries "2" after choosing')
ok(wait(lambda: "can't switch" in last_text(BUYER)), "switch refused")
_, sel2 = api("GET", f"/api/requests/{rid}/selection")
ok(sel2["selection"]["id"] == sel["selection"]["id"] and sel2["transaction"]["id"] == tx["id"],
   "still exactly the same single selection + transaction")
code, _ = api("POST", f"/api/requests/{rid}/select", {"offer_id": chosen["id"], "buyer_id": req["buyer_id"]})
ok(code == 200, "operator re-select of same offer is idempotent (200, not a new tx)")

print("\n§3A-3 Vendor-response association")
ok(webhook(OJ, f"wamid.{RUN}.ojx", "MM-000001 yes I get am 650k") == 200, "OJ cites a reference that isn't theirs")
um = wait(lambda: [u for u in (api("GET", "/api/vendor-responses/unmatched")[1] or []) if u["vendor_id"] == oj])
ok(um, "persisted as unmatched")
code, body = api("POST", f"/api/vendor-responses/{um[0]['id']}/associate", {"vendor_request_id": by_vendor[mike]["id"]})
ok(code == 409, "attaching OJ's reply to Mike's request is refused", f"{code} {body}")

print("\nNew unrelated request still works while a context exists")
before = len(api("GET", "/api/requests")[1])
ok(webhook(BUYER, f"wamid.{RUN}.ps5", "Abeg I need PS5 slim for Lagos, 550k") == 200, "buyer starts a new request")
ok(wait(lambda: len(api("GET", "/api/requests")[1]) == before + 1), "new BuyerRequest created (not trapped in selection)")

print("\n§23A Funnel data: delivery/read receipts + funnel counts")


def status_webhook(wamid, status, recipient):
    payload = json.dumps({"object": "whatsapp_business_account", "entry": [{"changes": [{"field": "messages", "value": {
        "messaging_product": "whatsapp",
        "statuses": [{"id": wamid, "status": status, "timestamp": str(int(time.time())), "recipient_id": recipient}],
    }}]}]}).encode()
    sig = "sha256=" + hmac.new(SECRET.encode(), payload, hashlib.sha256).hexdigest()
    req = urllib.request.Request(BASE + "/webhooks/whatsapp", data=payload, method="POST",
                                 headers={"Content-Type": "application/json", "X-Hub-Signature-256": sig})
    with urllib.request.urlopen(req) as r:
        return r.status


def funnel():
    _, f = api("GET", "/api/metrics/funnel?days=1", expect=200)
    return {st["key"]: st["count"] for st in f["stages"]}


offer_wamid = [r["wamid"] for r in outbound(BUYER) if "I found 1 option" in r["body"]["text"]["body"]][0]
vendor_wamid = [r["wamid"] for r in outbound(MIKE) if "Majo Request" in r["body"]["text"]["body"]][0]
ok(offer_wamid.startswith("wamid.") and vendor_wamid.startswith("wamid."), "outbound WhatsApp message IDs captured")

before = funnel()
for wamid, status, to in [(vendor_wamid, "delivered", MIKE), (vendor_wamid, "read", MIKE),
                          (offer_wamid, "sent", BUYER), (offer_wamid, "delivered", BUYER),
                          (offer_wamid, "delivered", BUYER), (offer_wamid, "read", BUYER), (offer_wamid, "read", BUYER)]:
    ok(status_webhook(wamid, status, to) == 200, f"signed status callback {status} for {'offer' if wamid == offer_wamid else 'vendor request'} message")
after = funnel()
ok(after["vendor_request_delivered"] - before["vendor_request_delivered"] == 1, "vendor request delivered counted once")
ok(after["offer_message_delivered"] - before["offer_message_delivered"] == 1, "offer delivered counted once (duplicate receipt ignored)")
ok(after["offer_message_read"] - before["offer_message_read"] == 1, "offer read counted once")

for key in ["buyer_request_created", "buyer_request_confirmed", "vendor_request_created", "vendor_request_sent",
            "vendor_request_send_failed", "vendor_response_received", "offer_created", "offer_sent",
            "offer_selected", "offer_selected_via_whatsapp", "transaction_created"]:
    ok(after[key] >= 1, f"funnel stage {key}", str(after[key]))
ok(after["transaction_completed"] == 0, "transaction_completed stays 0 (no fulfilment yet)")
print("    ── funnel (last 1 day) ──")
for k, v in after.items():
    print(f"    │ {k:<30} {v}")

print(f"\nALL {checks} CHECKS PASSED")
