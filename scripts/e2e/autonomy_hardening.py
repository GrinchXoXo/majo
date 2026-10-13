"""
Phase 4C hardening — recovery, delivery failures, request understanding and
operator control, through the real webhook path.

Real backend, real PostgreSQL, real Python extraction service; WhatsApp is
scripts/e2e/fake_graph_api.py (synthetic; NOT live WhatsApp). The backend
runs with short timings so deadlines pass inside the test:
VENDOR_REQUEST_TTL=12s, AUTO_ROUTE_MAX_VENDORS=1, AUTO_RETRY_FIRST_DELAY=1s.

Scenarios:
  I  a vendor goes quiet -> deadline -> another vendor contacted -> quiet too
     -> buyer asked to search other areas -> yes -> offer -> transaction;
     the first vendor's late (unquoted) reply still becomes an offer
  J  nobody else can help -> the buyer is told the honest status (never "unavailable")
  K  WhatsApp reports a vendor request undelivered (status webhook, 131026)
     -> request stops waiting, operator told the number is wrong, another vendor contacted
  L  a buyer update blocked by the 24h window (131047) -> kept -> delivered when the buyer writes
  M  two products in one message -> which first? -> "both" -> two requests, nothing dropped
  N  small talk and questions about nothing shown create no request
  O  restricted item -> held for review, never routed
  P  operator pause / resume (authenticated, reasoned, audited) stops and restarts automation
  Q  contradictory condition -> asked, not guessed
  R  layered metrics with explicit denominators (synthetic)
"""
import json
import os
import time

from discovery_common import RUN, api, ok, done, sql, say, outbound, wait, vendor, _post_webhook, published_listing

FAIL_PLAN = os.environ["FAIL_PLAN"]
T = 30.0
TTL = 12  # seconds (VENDOR_REQUEST_TTL of this run)


def texts(to):
    return [m["body"]["text"]["body"] for m in outbound(to) if m["body"].get("type") == "text" and not m.get("failed")]


def last_text(to):
    t = texts(to)
    return t[-1] if t else ""


def wait_text(to, needle, timeout=T):
    return wait(lambda: any(needle.lower() in t.lower() for t in texts(to)), timeout)


def req_of(buyer, product_like=None):
    q = f"""SELECT br.id FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '{buyer}'"""
    if product_like:
        q += f" AND br.product ILIKE '%{product_like}%'"
    return sql(q + " ORDER BY br.created_at DESC LIMIT 1")


def nreq(buyer):
    return int(sql(f"SELECT count(*) FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '{buyer}'"))


def vrs(rid):
    return sql(f"""SELECT COALESCE(string_agg(v.whatsapp_external_id || ':' || r.status, ',' ORDER BY r.created_at), '')
                   FROM vendor_requests r JOIN vendors v ON v.id = r.vendor_id WHERE r.buyer_request_id = '{rid}'""")


def recovery(rid):
    return sql(f"""SELECT COALESCE(string_agg(outcome || ':' || reason, ',' ORDER BY created_at), '') FROM workflow_decisions
                   WHERE buyer_request_id = '{rid}' AND decision = 'RECOVER_REQUEST'""")


def vendor_says(phone, text, quoted=None):
    n = int(time.time() * 1000) % 10**9
    msg = {"from": phone, "id": f"wamid.{RUN}.v{n}{phone[-3:]}", "timestamp": str(int(time.time())), "type": "text", "text": {"body": text}}
    if quoted:
        msg["context"] = {"from": "PHONE", "id": quoted}
    return _post_webhook({"messaging_product": "whatsapp", "contacts": [{"wa_id": phone, "profile": {"name": "V"}}], "messages": [msg]})


def status_failed(wamid, recipient, code, title):
    return _post_webhook({"messaging_product": "whatsapp", "statuses": [{"id": wamid, "status": "failed", "timestamp": str(int(time.time())),
                                                                        "recipient_id": recipient, "errors": [{"code": code, "title": title}]}]})


def set_fail_plan(plan):
    json.dump(plan, open(FAIL_PLAN, "w"))


def vr_wamid(rid, vendor_phone):
    return sql(f"""SELECT m.external_message_id FROM messages m JOIN vendor_requests r ON m.metadata->>'entity_id' = r.id::text
                   JOIN vendors v ON v.id = r.vendor_id WHERE r.buyer_request_id = '{rid}' AND v.whatsapp_external_id = '{vendor_phone}'
                   AND m.direction = 'OUTBOUND' ORDER BY m.created_at DESC LIMIT 1""")


print("== setup: vendors (control plane) ==")
code, st = api("GET", "/api/autonomy/settings")
ok(code == 200 and st["flags"]["AUTO_RECOVER_STALLED_REQUESTS"], "stalled-request recovery ON by default", json.dumps(st["flags"]))
ok(st["templates"]["buyer_update_template_configured"] is False, "no buyer-update template is assumed", st["templates"]["note"])
B1, B2, B3, B4, L1, U1 = (f"2348200{RUN[:3]}{i:03d}" for i in range(1, 7))
vb1 = vendor("Benin Gadgets One", B1, "Benin City", ["electronics"])
vb2 = vendor("Benin Gadgets Two", B2, "Benin City", ["electronics"])
vl1 = vendor("Lagos Gadgets", L1, "Lagos", ["electronics"])
vu1 = vendor("Uyo Furniture", U1, "Uyo", ["furniture"])
LL = published_listing(vl1, L1, "ps5l", "PS5 disc edition 430k Lagos")  # product evidence outside Benin City

# ---------------------------------------------------------------- I
print("\n== I: quiet vendor -> deadline -> recovery -> transaction ==")
BI = f"23491{RUN}01"
ok(say(BI, "I need a PS5 in Benin City, 450k") == 200, "buyer: PS5 in Benin City")
ri = wait(lambda: req_of(BI), T)
ok(wait(lambda: vrs(ri).count(":SENT") == 1, T), "one vendor contacted (AUTO_ROUTE_MAX_VENDORS=1)", vrs(ri))
first = vrs(ri).split(":")[0]
ok(wait(lambda: ":EXPIRED" in vrs(ri) and vrs(ri).count(":SENT") == 1, TTL + T), "deadline passed: first request EXPIRED, another vendor contacted",
   vrs(ri))
ok(wait_text(BI, "hasn't replied yet, so I'm checking with 1 other seller"), "buyer told honestly", last_text(BI))
ok(not any("unavailable" in t.lower() for t in texts(BI)), "never told the item is unavailable")
ok(wait(lambda: vrs(ri).count(":EXPIRED") == 2, TTL + T), "the second vendor goes quiet too", vrs(ri))
ok(wait_text(BI, "I can check sellers in other areas too"), "no local seller left: buyer asked about other areas", last_text(BI))
ok(L1 not in vrs(ri), "nobody outside the area contacted before the buyer agrees")
ok(say(BI, "yes") == 200, "buyer: yes")
ok(wait(lambda: f"{L1}:SENT" in vrs(ri), T), "Lagos vendor contacted (after the buyer's yes)", vrs(ri))
ok(vrs(ri).count(B1) + vrs(ri).count(B2) == 2, "no vendor asked twice", vrs(ri))
ok(vendor_says(L1, "Yes, PS5 available 430k") == 200, "Lagos vendor replies")
ok(wait_text(BI, "430,000"), "offer reaches the buyer", last_text(BI).replace("\n", " | ")[:100])
n0 = len(texts(BI))
ok(vendor_says(first, "Sorry for the delay, the PS5 is available for 440k") == 200, "the FIRST vendor replies late, without quoting")
ok(wait_text(BI, "440,000"), "the late reply still becomes an option for the buyer", last_text(BI).replace("\n", " | ")[:100])
ok(sql(f"""SELECT r.extracted_data->>'association' FROM vendor_responses r JOIN vendor_requests q ON q.id = r.vendor_request_id
           JOIN vendors v ON v.id = q.vendor_id WHERE q.buyer_request_id = '{ri}' AND v.whatsapp_external_id = '{first}'""") == "late_reply_after_expiry",
   "associated as a late reply after expiry")
ok(say(BI, "1") == 200, "buyer selects 1")
ok(wait(lambda: sql(f"SELECT payment_status FROM transactions WHERE buyer_request_id = '{ri}'") == "UNPAID", T), "transaction created (UNPAID)")
rec = recovery(ri)
ok(rec.count("APPLIED:rerouted_to_alternative_vendors") == 1 and "APPLIED:asked_buyer_to_broaden_area" in rec, "recovery decisions recorded", rec)
ok(int(sql(f"SELECT count(*) FROM events WHERE actor_type = 'OPERATOR' AND entity_id = '{ri}'")) == 0, "zero operator actions")

# ---------------------------------------------------------------- J
print("\n== J: nobody else can help -> honest status ==")
BJ = f"23491{RUN}02"
ok(say(BJ, "I need a mattress in Uyo") == 200, "buyer: mattress in Uyo (one furniture vendor anywhere)")
rj = wait(lambda: req_of(BJ), T)
ok(wait(lambda: f"{U1}:SENT" in vrs(rj), T), "the Uyo vendor is contacted", vrs(rj))
ok(wait_text(BJ, "I haven't got a confirmed option for mattress yet", TTL + T), "after the deadline the buyer hears the real status", last_text(BJ))
ok("pass on any reply" in last_text(BJ).lower() and "unavailable" not in last_text(BJ).lower(), "late replies still count; never 'unavailable'")
ok(sql(f"SELECT status FROM buyer_requests WHERE id = '{rj}'") == "CONFIRMED", "the request stays open")
ok(sql(f"SELECT count(*) FROM workflow_exceptions WHERE buyer_request_id = '{rj}'") == "0", "not an operator event")

# ---------------------------------------------------------------- K
print("\n== K: provider reports the vendor request undelivered ==")
BK = f"23491{RUN}03"
ok(say(BK, "I need an Xbox in Benin City") == 200, "buyer: Xbox")
rk = wait(lambda: req_of(BK), T)
ok(wait(lambda: vrs(rk).count(":SENT") == 1, T), "one vendor contacted", vrs(rk))
kv = vrs(rk).split(":")[0]
wam = wait(lambda: vr_wamid(rk, kv), T)
ok(status_failed(wam, kv, 131026, "Message undeliverable") == 200, "delivery-status webhook: failed 131026", wam)
ok(wait(lambda: f"{kv}:SEND_FAILED" in vrs(rk) and vrs(rk).count(":SENT") == 1, T), "request stops waiting; another vendor contacted", vrs(rk))
ok(wait(lambda: sql(f"SELECT reason FROM workflow_exceptions WHERE buyer_request_id = '{rk}' AND status = 'OPEN'") == "invalid_recipient", T),
   "operator told the vendor's number cannot receive WhatsApp")
ok(status_failed(wam, kv, 131026, "Message undeliverable") == 200, "the same failure reported again")
time.sleep(2)
ok(vrs(rk).count(":SENT") == 1 and len(vrs(rk).split(",")) == 2, "duplicate report changes nothing", vrs(rk))

# ---------------------------------------------------------------- L
print("\n== L: update blocked by the 24h window -> delivered when the buyer writes ==")
BL = f"23491{RUN}04"
ok(say(BL, "I need a mattress in Uyo please") == 200, "buyer: mattress in Uyo")
rl = wait(lambda: req_of(BL), T)
ok(wait(lambda: f"{U1}:SENT" in vrs(rl), T), "vendor contacted")
ok(wait_text(BL, "checking with 1 seller"), "buyer told Majo is checking")
set_fail_plan({BL: ["400:131047"]})
ok(wait(lambda: sql(f"SELECT status || ':' || failure_code FROM pending_notices WHERE buyer_request_id = '{rl}'") == "PENDING:131047", TTL + T),
   "the status update was refused (window closed) and kept, not retried as free text")
ok(not wait_text(BL, "I haven't got a confirmed option", 1), "nothing reached the buyer yet")
ok(sql(f"SELECT count(*) FROM workflow_exceptions WHERE buyer_request_id = '{rl}'") == "0", "a compliant later route exists: no operator exception")
ok(say(BL, "hello?") == 200, "the buyer writes (window open again)")
ok(wait_text(BL, "I haven't got a confirmed option for mattress"), "the kept update is delivered", last_text(BL))
ok(sql(f"SELECT status FROM pending_notices WHERE buyer_request_id = '{rl}'") == "SENT", "notice closed")

# ---------------------------------------------------------------- M
print("\n== M: two products in one message ==")
BM = f"23491{RUN}05"
ok(say(BM, "I need an iPhone 13 and a laptop in Benin City") == 200, "buyer names two products")
ok(wait_text(BM, "Which should I start with?"), "Majo asks which first (nothing dropped)", last_text(BM))
ok(nreq(BM) == 1, "one request so far")
ok(say(BM, "both") == 200, "buyer: both")
ok(wait_text(BM, "Starting with iphone 13. I'll look for laptop right after"), "Majo confirms the order", last_text(BM))
ok(wait(lambda: nreq(BM) == 2, T), "the laptop became its own request (canonical: one request per product)")
ok(wait_text(BM, "Which laptop model"), "the laptop request continues (its question comes when the slot is free)", last_text(BM))
ok(wait(lambda: vrs(req_of(BM, "iphone")) != "", T), "the iPhone request is routed")

# ---------------------------------------------------------------- N
print("\n== N: no junk requests ==")
BN = f"23491{RUN}06"
ok(say(BN, "hello") == 200, "buyer: hello")
ok(wait_text(BN, "Tell me what you're looking for"), "greeting answered with a prompt", last_text(BN))
ok(say(BN, "thanks") == 200 and say(BN, "👍") == 200, "buyer: thanks, 👍")
ok(say(BN, "how much is it?") == 200, "buyer: how much is it? (nothing was shown)")
ok(wait_text(BN, "I haven't sent you any options yet"), "Majo says nothing was shown", last_text(BN))
time.sleep(2)
ok(nreq(BN) == 0, "no BuyerRequest created by small talk or a question about nothing")
ok(int(sql(f"SELECT count(*) FROM workflow_decisions WHERE decision = 'ACKNOWLEDGE_MESSAGE' AND inputs->>'text' IN ('hello', 'thanks', '👍')")) == 3,
   "each recorded in the decision log")

# ---------------------------------------------------------------- O
print("\n== O: restricted item ==")
BO = f"23491{RUN}07"
ok(say(BO, "I need a pistol in Benin City") == 200, "buyer: restricted item")
ro = wait(lambda: req_of(BO), T)
ok(wait(lambda: sql(f"SELECT class || ':' || reason FROM workflow_exceptions WHERE buyer_request_id = '{ro}'") == "SAFETY_EXCEPTION:restricted_item_review", T),
   "held for operator review (SAFETY)")
ok(sql(f"SELECT status FROM buyer_requests WHERE id = '{ro}'") == "NEEDS_REVIEW" and vrs(ro) == "", "never confirmed, nobody contacted")

# ---------------------------------------------------------------- P
print("\n== P: operator pause / resume ==")
BP = f"23491{RUN}08"
ok(say(BP, "I need a PS4 in Benin City") == 200, "buyer: PS4")
rp = wait(lambda: req_of(BP), T)
ok(wait(lambda: vrs(rp).count(":SENT") == 1, T), "routed", vrs(rp))
code, _ = api("POST", f"/api/requests/{rp}/pause", {"reason": "buyer phoned support"}, token="")
ok(code == 401, "pause without the operator token -> 401", str(code))
code, _ = api("POST", f"/api/requests/{rp}/pause", {"reason": ""})
ok(code == 400, "pause without a reason -> 400", str(code))
code, b = api("POST", f"/api/requests/{rp}/pause", {"reason": "buyer phoned support"})
ok(code == 200 and b["held"], "paused", json.dumps(b))
ok(wait(lambda: "DEFERRED:workflow_held" in recovery(rp), TTL + T), "deadline passes while paused: no automatic step", recovery(rp))
ok(vrs(rp).count(",") == 0, "nobody else contacted while paused", vrs(rp))
code, b = api("POST", f"/api/requests/{rp}/resume", {"reason": "resolved with the buyer"})
ok(code == 200 and b["resumed"], "resumed", json.dumps(b))
ok(wait(lambda: vrs(rp).count(":SENT") == 1 and ":EXPIRED" in vrs(rp), T), "after resume Majo continues (another vendor contacted)", vrs(rp))
ok(int(sql(f"SELECT count(*) FROM events WHERE entity_id = '{rp}' AND event_type IN ('WORKFLOW_HELD', 'WORKFLOW_RESUMED') AND actor_type = 'OPERATOR'")) == 2,
   "pause and resume audited")
code, d = api("GET", f"/api/requests/{rp}/autonomy")
ok(code == 200 and len(d["holds"]) == 1 and d["messages"], "operator view: holds and the message trail with delivery state",
   f"{len(d['messages'])} messages")

# ---------------------------------------------------------------- Q
print("\n== Q: contradictory details are asked, not guessed ==")
BQ = f"23491{RUN}09"
ok(say(BQ, "brand new fairly used ps5 in Benin City") == 200, "buyer: contradictory condition")
ok(wait_text(BQ, "brand new or fairly used?"), "Majo asks", last_text(BQ))
ok(say(BQ, "brand new") == 200, "buyer: brand new")
rq = wait(lambda: req_of(BQ), T)
ok(wait(lambda: sql(f"SELECT status || ':' || condition FROM buyer_requests WHERE id = '{rq}'") == "CONFIRMED:new", T), "confirmed with the buyer's answer")

# ---------------------------------------------------------------- R
print("\n== R: layered metrics (synthetic run) ==")
code, m = api("GET", "/api/autonomy/metrics")
ok(code == 200 and "workflow" in m and "questions" in m and "definitions" in m, "metrics served with definitions")
print(json.dumps({k: m[k] for k in ["workflow", "questions", "recovery", "delivery"]}, indent=1))
w = m["workflow"]
ok(w["started"] == int(sql("SELECT count(*) FROM buyer_requests")), "workflow denominator = requests created", str(w["started"]))
ok(all(w[k]["of"] == w["started"] for k in ["completed", "requiring_clarification", "escalated_to_operator", "pending"]),
   "every workflow rate shares that denominator")
ok(m["recovery"]["rerouted"] >= 2 and m["recovery"]["vendor_requests_expired"] >= 4, "recovery measured from persisted decisions",
   json.dumps({"rerouted": m["recovery"]["rerouted"], "expired": m["recovery"]["vendor_requests_expired"]}))
ok(m["delivery"]["notices_delivered"] >= 1 and m["delivery"]["provider_failures_reported_async"] == 1,
   "delivery health measured (the duplicate failure receipt was deduplicated at the webhook)",
   json.dumps(m["delivery"]))
ok(m["questions"]["delivered"] >= 4 and m["questions"]["answered"]["of"] == m["questions"]["delivered"], "question-level denominator",
   json.dumps(m["questions"]["answered"]))
done()
