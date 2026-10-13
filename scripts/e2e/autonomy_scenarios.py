"""
Phase 4C correction — autonomous runtime scenarios A–G.

Real backend, real PostgreSQL, real Python extraction service; WhatsApp is
scripts/e2e/fake_graph_api.py. Every buyer and vendor action is a signed
Meta-format webhook going through the same application path real WhatsApp
uses (webhook -> context resolution -> interpretation -> policy -> service ->
DB -> event -> outbound). No step of a buyer's transaction calls an operator
endpoint: operator calls here are control-plane SETUP only (vendor
onboarding, listing curation, staff discovery consent, sending a discovery
batch) and the final read-only metrics.

Scenarios:
  A  clear typed request -> auto confirm -> match -> route -> vendor reply -> offer -> selection -> transaction
  B  missing data (vague request) -> questions -> answers merge into the SAME request -> continues
  B' ambiguous reference ("I want that one") -> which one? -> answer -> enquiry continues
  C  database-aware answers ("how much is the blue one?" / two black ones -> which?)
  D  discovery "Ask about it" tap -> auto confirm -> listing vendor -> reply -> offer -> transaction
  E  vendor ambiguity: two open requests -> ask vendor which -> answer -> associated; unclear reply -> targeted question
  F  no local match -> ask to broaden -> yes -> routed further; nothing at all -> honest no-match
  G  provider failure -> automatic retry -> recovered; exhausted -> exception; 24h-window policy -> exception

Env as discovery_common, plus FAIL_PLAN (fake graph scripted failures).
"""
import json
import os
import time

from discovery_common import (RUN, api, ok, done, sql, say, tap_button, outbound, wait, vendor, post_listing, opt_in, batch,
                              interactive_ids, webhook)

FAIL_PLAN = os.environ["FAIL_PLAN"]
T = 25.0


def last_text(to):
    for m in reversed(outbound(to)):
        b = m["body"]
        if b.get("type") == "text":
            return b["text"]["body"]
    return ""


def texts(to):
    return [m["body"]["text"]["body"] for m in outbound(to) if m["body"].get("type") == "text" and not m.get("failed")]


def wait_text(to, needle, timeout=T):
    return wait(lambda: any(needle.lower() in t.lower() for t in texts(to)), timeout)


def req_of(buyer_phone, product_like=None):
    q = f"""SELECT br.id || '|' || br.status FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id
            WHERE b.external_user_id = '{buyer_phone}'"""
    if product_like:
        q += f" AND br.product ILIKE '%{product_like}%'"
    q += " ORDER BY br.created_at DESC LIMIT 1"
    v = sql(q)
    return v.split("|") if v else (None, None)


def status_is(rid, st):
    return sql(f"SELECT status FROM buyer_requests WHERE id = '{rid}'") == st


def vendor_reqs(rid):
    return sql(f"SELECT count(*) FROM vendor_requests WHERE buyer_request_id = '{rid}'")


def operator_actions(rid):
    return int(sql(f"""SELECT count(*) FROM events WHERE actor_type = 'OPERATOR' AND (
        (entity_type = 'buyer_request' AND entity_id = '{rid}')
        OR (entity_type = 'vendor_request' AND entity_id IN (SELECT id FROM vendor_requests WHERE buyer_request_id = '{rid}')))"""))


def system_confirmed_and_routed(rid):
    c = sql(f"SELECT actor_type FROM events WHERE entity_type = 'buyer_request' AND entity_id = '{rid}' AND event_type = 'REQUEST_CONFIRMED'")
    r = sql(f"""SELECT string_agg(DISTINCT e.actor_type, ',') FROM events e JOIN vendor_requests v ON v.id = e.entity_id
                WHERE e.entity_type = 'vendor_request' AND v.buyer_request_id = '{rid}' AND e.event_type = 'VENDORS_ROUTED'""")
    return c, r


def decisions(rid):
    code, d = api("GET", f"/api/requests/{rid}/autonomy")
    assert code == 200, d
    return d


def vendor_says(phone, text, quoted=None):
    n = int(time.time() * 1000) % 10**9
    payload = {"messaging_product": "whatsapp", "contacts": [{"wa_id": phone, "profile": {"name": "V"}}],
               "messages": [{"from": phone, "id": f"wamid.{RUN}.v{n}{phone[-3:]}", "timestamp": str(int(time.time())), "type": "text",
                             "text": {"body": text}}]}
    if quoted:
        payload["messages"][0]["context"] = {"from": "PHONE", "id": quoted}
    from discovery_common import _post_webhook
    return _post_webhook(payload)


def set_fail_plan(plan):
    json.dump(plan, open(FAIL_PLAN, "w"))


def publish(vid, phone, tag, caption, variant):
    post_listing(phone, tag, caption)
    lst = wait(lambda: [x for x in (api("GET", f"/api/vendors/{vid}/supply-listings")[1] or []) if x["status"] == "DRAFT"], T)
    ok(lst, f"draft listing created ({tag})")
    L = lst[0]
    code, d = api("GET", f"/api/supply-listings/{L['id']}")
    code, e = api("PATCH", f"/api/supply-listings/{L['id']}", {"variant": variant, "expected_updated_at": d["updated_at"]})
    ok(code == 200, f"curation sets variant ({tag})", str(code))
    code, p = api("POST", f"/api/supply-listings/{L['id']}/publish", {})
    ok(code == 200 and p["status"] == "ACTIVE", f"published ({tag})", p.get("public_reference", ""))
    return p


print("== control-plane setup (vendors, curated listings, staff consent) ==")
code, st = api("GET", "/api/autonomy/settings")
ok(code == 200 and all(st["flags"][k] for k in ["AUTO_CONFIRM_CLEAR_REQUESTS", "AUTO_ROUTE_MATCHED_REQUESTS", "AUTO_ROUTE_DISCOVERY_REQUESTS",
                                                  "AUTO_RESOLVE_VENDOR_CONTEXT", "AUTO_CLARIFY_REQUESTS", "AUTO_CLARIFY_VENDOR_RESPONSES", "AUTO_RETRY_DELIVERY"]),
   "autonomous runtime ON by default (every flag except trusted auto-publish)", json.dumps(st["flags"]))
ok(not st["flags"]["AUTO_PUBLISH_TRUSTED_VENDOR_LISTINGS"], "trusted-vendor auto-publish OFF until the lock's evidence exists")
code, _ = api("POST", "/api/requests/00000000-0000-0000-0000-000000000000/confirm", {}, token="")
ok(code == 401, "operator overrides are authenticated control-plane actions (no token -> 401)", str(code))

V1, V2, V3, V4, V5, V6 = (f"2348100{RUN[:3]}{i:03d}" for i in range(1, 7))
V6 = "2347099999999"  # the fake Graph API rejects sends to this number with 131047 (24h window closed)
v1 = vendor("Benin Phones", V1, "Benin City", ["electronics"])
v2 = vendor("Lagos Gadgets", V2, "Lagos", ["electronics"])
v3 = vendor("Uniben Gadgets", V3, "Benin City", ["electronics"])
v4 = vendor("Warri Electronics", V4, "Warri", ["electronics"])
v5 = vendor("Uyo Electronics", V5, "Uyo", ["electronics"])
v6 = vendor("Calabar Electronics", V6, "Calabar", ["electronics"])
L1 = publish(v3, V3, "blk120", "iPhone 13 128GB 120k Benin City", "128GB Black")
L2 = publish(v3, V3, "blu145", "iPhone 13 128GB 145k Benin City", "128GB Blue")
L3 = publish(v3, V3, "blk150", "iPhone 13 128GB 150k Benin City", "128GB Black")
LM = publish(v2, V2, "mba", "MacBook Air M1 650k Lagos", "M1 8GB")

# ---------------------------------------------------------------- A
print("\n== A: clear typed request -> transaction, no operator ==")
BA = f"23490{RUN}01"
ok(say(BA, "I need an iPhone 13 128GB, budget 650k, Benin City") == 200, "buyer sends a complete request")
rid, _ = wait(lambda: req_of(BA)[0] and req_of(BA), T)
ok(wait(lambda: vendor_reqs(rid) != "0", T), "request routed automatically", f"{vendor_reqs(rid)} vendor request(s)")
c, r = system_confirmed_and_routed(rid)
ok(c == "SYSTEM" and r == "SYSTEM", "confirmed and routed by SYSTEM", f"confirm={c} route={r}")
ok(wait_text(BA, "checking with"), "buyer is told Majo is checking with sellers", last_text(BA))
vrs = sql(f"SELECT string_agg(v.whatsapp_external_id, ',') FROM vendor_requests r JOIN vendors v ON v.id = r.vendor_id WHERE r.buyer_request_id = '{rid}'")
ok(V1 in vrs and V3 in vrs and V2 not in vrs, "routing policy: local electronics vendors contacted, Lagos vendor below the score floor", vrs)
d = decisions(rid)
route = [x for x in d["decisions"] if x["decision"] == "ROUTE_REQUEST"][0]
ok(route["outcome"] == "APPLIED" and route["inputs"]["candidates"], "why the route happened is recorded (candidates, scores, exclusions)", route["reason"])
ok(vendor_says(V1, "Yes boss, I get am for 620k") == 200, "vendor replies in pidgin")
ok(wait_text(BA, "620,000"), "offer reaches the buyer automatically", last_text(BA).replace("\n", " | ")[:90])
ok(say(BA, "1") == 200, "buyer selects option 1")
ok(wait(lambda: sql(f"SELECT payment_status FROM transactions WHERE buyer_request_id = '{rid}'") == "UNPAID", T), "transaction created (UNPAID)")
ok(operator_actions(rid) == 0, "zero operator actions on this transaction")

# ---------------------------------------------------------------- B (missing data)
print("\n== B: vague request -> clarification -> same request continues ==")
BB = f"23490{RUN}02"
ok(say(BB, "I need a TV for my room in Benin City") == 200, "buyer: vague request")
ok(wait_text(BB, "What size are you looking for"), "Majo asks ONE question (size)", last_text(BB))
rid_b, stb = req_of(BB)
ok(stb == "AWAITING_BUYER", "request waits for the buyer (not an operator queue)", stb)
nreq = sql(f"SELECT count(*) FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '{BB}'")
ok(say(BB, "55 inch") == 200, "buyer answers")
ok(wait_text(BB, "what's your budget"), "next question (budget) — one at a time", last_text(BB))
ok(say(BB, "around 300k") == 200, "buyer answers")
ok(wait(lambda: vendor_reqs(rid_b) != "0", T), "request continues to routing automatically")
ok(sql(f"SELECT count(*) FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '{BB}'") == nreq,
   "answers merged into the SAME request (no junk requests)")
ok(sql(f"SELECT variant || '|' || budget_max FROM buyer_requests WHERE id = '{rid_b}'") == "55 inch|300000", "fields: size + budget merged")
cl = decisions(rid_b)["clarifications"]
ok(len(cl) == 2 and all(x["status"] == "ANSWERED" for x in cl), "2 clarifications, both ANSWERED", ",".join(x["field"] for x in cl))
ok(operator_actions(rid_b) == 0, "zero operator actions")

# ---------------------------------------------------------------- C / B' (discovery context)
print("\n== C: database-aware answers / B': ambiguous reference ==")
S1, S2 = "2347300000001", "2347300000002"
for s in (S1, S2):
    ok(say(s, "hello majo") == 200, f"staff {s[-1]} messages Majo")
    wait(lambda: sql(f"SELECT count(*) FROM buyers WHERE external_user_id = '{s}'") == "1")
    code, p = opt_in(s, "Benin City", ["electronics"])
    ok(code == 200 and p["status"] == "OPTED_IN", f"staff {s[-1]} opted in (staff test record)")
code, b = batch(f"auto-{RUN}", [S1, S2], limit=3)
ok(code == 201, "discovery batch planned", str(b.get("batch", {}).get("plan", {}).get("deliveries")))
code, r = api("POST", f"/api/discovery/batches/{b['batch']['id']}/send")
ok(code == 200 and r["sent"] == 2, "batch sent to both staff buyers", json.dumps(r))

reqs_s1 = sql("SELECT count(*) FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '2347300000001'")
ok(say(S1, "How much is the blue one?") == 200, "S1: how much is the blue one?")
ok(wait_text(S1, "listed at ₦145,000"), "Majo answers from the listing (one match)", last_text(S1))
ok(say(S1, "How much is the black one?") == 200, "S1: how much is the black one?")
ok(wait_text(S1, "Do you mean the ₦120,000 one or the ₦150,000 one?"), "two black ones -> Majo asks which", last_text(S1).replace("\n", " | "))
ok(say(S1, "the 150k one") == 200, "S1 answers by price")
ok(wait_text(S1, "listed at ₦150,000"), "Majo answers for the chosen one", last_text(S1))
ok(sql("SELECT count(*) FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '2347300000001'") == reqs_s1,
   "questions about prices created no BuyerRequest")

ok(say(S1, "I want that one") == 200, "B': S1 'I want that one' (three phones shown, none in focus)")
ok(wait_text(S1, "which one do you mean"), "Majo asks which (context is genuinely ambiguous)", last_text(S1).replace("\n", " | ")[:120])
ok(say(S1, "2") == 200, "S1 answers 2")
rid_c = wait(lambda: sql("SELECT br.id FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '2347300000001' AND br.origin = 'discovery'"), T)
ok(rid_c, "the answer continues: discovery enquiry created")
ok(wait(lambda: status_is(rid_c, "CONFIRMED") and vendor_reqs(rid_c) == "1", T), "auto-confirmed and routed to the listing's vendor")
ok(sql(f"SELECT origin FROM buyer_requests WHERE id = '{rid_c}'") == "discovery", "attributed to the discovery delivery")

# ---------------------------------------------------------------- D
print("\n== D: discovery Ask about it -> transaction, no operator ==")
def ids_of(to):
    return [i for m in outbound(to) if m["body"].get("type") == "interactive" for i in interactive_ids(m)]


row = next((i for i in ids_of(S2) if i.endswith(f":l:{L1['public_reference']}")), None)
if row:
    from discovery_common import tap_list
    ok(tap_list(S2, row, "iPhone 13") == 200, "S2 opens the black iPhone card", row)
ask_id = wait(lambda: next((i for i in ids_of(S2) if i.endswith(f":l:{L1['public_reference']}:ask")), None), T)
ok(ask_id, "card offers Ask about it", str(ask_id))
ok(tap_button(S2, ask_id, "Ask about it") == 200, "S2 taps Ask about it", ask_id)
rid_d = wait(lambda: sql("SELECT br.id FROM buyer_requests br JOIN buyers b ON b.id = br.buyer_id WHERE b.external_user_id = '2347300000002' AND br.origin = 'discovery'"), T)
ok(wait(lambda: status_is(rid_d, "CONFIRMED") and vendor_reqs(rid_d) == "1", T), "auto-confirmed; routed to the listing's vendor")
vr_ref = sql(f"SELECT public_reference FROM vendor_requests WHERE buyer_request_id = '{rid_d}'")
quoted = sql(f"SELECT external_message_id FROM messages WHERE metadata->>'entity_type' = 'vendor_request' AND metadata->>'reference' = '{vr_ref}' AND direction = 'OUTBOUND'")
ok(quoted, "vendor received the reconfirmation request", vr_ref)
ok(vendor_says(V3, "Yes, still available at 120k", quoted=quoted) == 200, "vendor replies with WhatsApp Reply (quoted message) — V3 has several open requests")
ok(wait(lambda: sql(f"""SELECT count(*) FROM vendor_responses r JOIN vendor_requests v ON v.id = r.vendor_request_id
                         WHERE v.buyer_request_id = '{rid_d}' AND r.extracted_data->>'association' = 'quoted_message'""") == "1", T),
   "reply associated by the quoted message (structured context)")
ok(wait(lambda: any("1." in t and "₦" in t for t in texts(S2)), T), "offer delivered to S2 (listed price carried as availability evidence)")
ok(say(S2, "1") == 200, "S2 selects 1")
ok(wait(lambda: sql(f"SELECT count(*) FROM transactions WHERE buyer_request_id = '{rid_d}'") == "1", T), "transaction created")
ok(operator_actions(rid_d) == 0, "zero operator actions")

# ---------------------------------------------------------------- E
print("\n== E: vendor ambiguity and unclear replies ==")
BE1, BE2 = f"23490{RUN}05", f"23490{RUN}06"
ok(say(BE1, "I need a PS5 in Benin City") == 200, "buyer E1: PS5")
ok(say(BE2, "I need a Samsung A54 in Benin City") == 200, "buyer E2: Samsung A54 (model stated: not asked again)")
re1 = wait(lambda: req_of(BE1)[0], T)
re2 = wait(lambda: req_of(BE2)[0], T)
ok(wait(lambda: vendor_reqs(re1) != "0" and vendor_reqs(re2) != "0", T), "both routed automatically (V1 now has two open requests)")
ok(sql(f"SELECT variant FROM buyer_requests WHERE id = '{re2}'") == "A54", "model taken from the message, not asked")
ok(not wait_text(BE2, "Which Samsung model", 1), "no needless question")
ok(vendor_says(V1, "Yes I have it, 500k") == 200, "V1: 'Yes I have it, 500k' (names neither request)")
ok(wait_text(V1, "Which request is that for?"), "Majo asks the vendor which request", last_text(V1).replace("\n", " | "))
ok(vendor_says(V1, "the ps5") == 200, "V1 answers 'the ps5'")
ok(wait_text(BE1, "500,000"), "PS5 buyer receives the offer (reply associated by the vendor's answer)")
ok(not wait_text(BE2, "500,000", 1), "Samsung buyer received nothing from that reply")
sam_vr = sql(f"SELECT public_reference FROM vendor_requests r JOIN vendors v ON v.id = r.vendor_id WHERE r.buyer_request_id = '{re2}' AND v.whatsapp_external_id = '{V1}'")
sam_q = sql(f"SELECT external_message_id FROM messages WHERE direction = 'OUTBOUND' AND metadata->>'reference' = '{sam_vr}' AND metadata->>'kind' = 'vendor_request'")
ok(vendor_says(V1, "Which one?", quoted=sam_q) == 200, "V1 replies 'Which one?' using WhatsApp Reply on the Samsung request (V1 still has 2 open requests)")
ok(wait_text(V1, "The buyer is asking for samsung A54"), "Majo asks a targeted question built from the request", last_text(V1))
sig0 = sql(f"SELECT count(*) FROM supply_signals s JOIN vendors v ON v.id = s.vendor_id WHERE v.whatsapp_external_id = '{V1}'")
ok(vendor_says(V1, "PS5 slim available 450k") == 200, "while Majo waits for the Samsung answer, V1 advertises other stock (unquoted)")
ok(wait(lambda: sql(f"SELECT count(*) FROM supply_signals s JOIN vendors v ON v.id = s.vendor_id WHERE v.whatsapp_external_id = '{V1}'") != sig0, T),
   "it becomes supply (a new SupplySignal) …")
ok(sql("SELECT count(*) FROM vendor_responses WHERE raw_message = 'PS5 slim available 450k'") == "0",
   "… and is never attached as a reply to the Samsung (or TV) request")
ok(vendor_says(V1, "yes 380k") == 200, "V1: 'yes 380k'")
ok(wait_text(BE2, "380,000"), "Samsung buyer receives the offer")
for r_ in (re1, re2):
    ok(operator_actions(r_) == 0, "zero operator actions")
BE3, BE4 = f"23490{RUN}12", f"23490{RUN}13"
ok(say(BE3, "I need a PS4 in Benin City") == 200 and say(BE4, "I need an Xbox in Benin City") == 200, "two more requests reach V1")
re3, re4 = wait(lambda: req_of(BE3)[0], T), wait(lambda: req_of(BE4)[0], T)
ok(wait(lambda: vendor_reqs(re3) != "0" and vendor_reqs(re4) != "0", T), "both routed")
ok(vendor_says(V1, "Available, 300k") == 200, "V1: 'Available, 300k' (ambiguous)")
ok(wait_text(V1, "Which request is that for?"), "Majo asks which")
xbox_ref = sql(f"SELECT r.public_reference FROM vendor_requests r JOIN vendors v ON v.id = r.vendor_id WHERE r.buyer_request_id = '{re4}' AND v.whatsapp_external_id = '{V1}'")
ok(vendor_says(V1, xbox_ref) == 200, f"V1 answers with the reference {xbox_ref}")
ok(wait_text(BE4, "300,000"), "Xbox buyer receives the offer (MM reference answers the open question)")
ok(not wait_text(BE3, "300,000", 1), "PS4 buyer received nothing")
ok(sql("SELECT count(*) FROM vendor_responses WHERE vendor_request_id IS NULL AND review_state = 'NEEDS_REVIEW'") == "0",
   "no reply left in the operator's unmatched queue")

# ---------------------------------------------------------------- F
print("\n== F: no match ==")
BF = f"23490{RUN}07"
ok(say(BF, "I need a MacBook Air M1 in Port Harcourt, 700k") == 200, "buyer in Port Harcourt (only a Lagos seller has one)")
ok(wait_text(BF, "I can check sellers in other areas too"), "Majo offers a broader search", last_text(BF))
rf = req_of(BF)[0]
ok(vendor_reqs(rf) == "0", "nobody contacted before the buyer agrees")
ok(say(BF, "Yes") == 200, "buyer: Yes")
ok(wait(lambda: vendor_reqs(rf) == "1", T), "routed to the Lagos seller (broadened)")
ok(sql(f"SELECT v.whatsapp_external_id FROM vendor_requests r JOIN vendors v ON v.id = r.vendor_id WHERE r.buyer_request_id = '{rf}'") == V2, "the Lagos vendor")
BF2 = f"23490{RUN}08"
ok(say(BF2, "I need a mattress in Benin City") == 200, "buyer: mattress (no furniture seller at all)")
ok(wait_text(BF2, "I couldn't find a seller for mattress in Benin City"), "honest no-match reply", last_text(BF2))
rf2 = req_of(BF2)[0]
ok(sql(f"SELECT count(*) FROM workflow_exceptions WHERE buyer_request_id = '{rf2}'") == "0", "no-match is not an operator event")

# ---------------------------------------------------------------- H
print("\n== H: two requests in one conversation, one question at a time ==")
BH = f"23490{RUN}14"


def open_question(buyer):
    return sql(f"""SELECT COALESCE(br.product, '') || '|' || c.field FROM clarifications c JOIN buyers b ON b.id = c.buyer_id
                   LEFT JOIN buyer_requests br ON br.id = c.buyer_request_id
                   WHERE b.external_user_id = '{buyer}' AND c.status = 'OPEN'""")


ok(say(BH, "I need a TV in Benin City") == 200, "buyer H: TV")
ok(wait(lambda: open_question(BH) == "tv|spec", T), "Majo asks the TV size", open_question(BH))
ok(say(BH, "I also need a laptop in Benin City") == 200, "buyer H, before answering: a laptop too")
ok(wait(lambda: req_of(BH, "laptop")[0], T), "laptop request created (TV question superseded)")
time.sleep(3)  # let both requests settle: exactly one question is open at a time
answers = {"tv|spec": "43 inch", "tv|budget": "200k", "laptop|spec": "Dell XPS 13", "laptop|budget": "any"}
asked = []
for _ in range(6):
    q = wait(lambda: open_question(BH), T)
    if not q:
        break
    asked.append(q)
    n = len(texts(BH))
    ok(say(BH, answers[q]) == 200, f"buyer answers the open question ({q})", answers[q])
    wait(lambda: open_question(BH) != q or len(texts(BH)) > n, T)
    time.sleep(1.5)
tv, lap = req_of(BH, "tv")[0], req_of(BH, "laptop")[0]
ok(wait(lambda: vendor_reqs(tv) != "0" and vendor_reqs(lap) != "0", T), "both requests routed", " -> ".join(asked))
ok(sql(f"SELECT variant || '|' || budget_max FROM buyer_requests WHERE id = '{tv}'") == "43 inch|200000", "TV got the TV answers")
ok(sql(f"SELECT variant || '|' || COALESCE(budget_max::text, '-') FROM buyer_requests WHERE id = '{lap}'") == "Dell XPS 13|-", "laptop got the laptop answers")

# ---------------------------------------------------------------- G
print("\n== G: provider failure -> retry / exception ==")
set_fail_plan({V4: [503, 503], V5: [503] * 20})
BG = f"23490{RUN}09"
ok(say(BG, "I need a laptop in Warri") == 200, "buyer in Warri (Warri vendor's sends fail twice)")
ok(wait_text(BG, "Which laptop model"), "generic laptop -> model question", last_text(BG))
ok(say(BG, "HP Elitebook 840") == 200, "buyer answers model")
ok(wait_text(BG, "And what's your budget?"), "then budget", last_text(BG))
ok(say(BG, "any") == 200, "buyer: any (skip)")
rg = req_of(BG)[0]
ok(wait(lambda: sql(f"SELECT status FROM vendor_requests WHERE buyer_request_id = '{rg}'") == "SENT", 40), "vendor request recovered automatically")
ok(int(sql(f"SELECT count(*) FROM workflow_decisions WHERE decision = 'RETRY_DELIVERY' AND reason = 'recovered' AND buyer_request_id = '{rg}'")) == 1,
   "recovery recorded")
ok(sql(f"SELECT count(*) FROM workflow_exceptions WHERE buyer_request_id = '{rg}'") == "0", "recovered failure is not an operator exception")
BG2 = f"23490{RUN}10"
ok(say(BG2, "I need a PS5 in Uyo") == 200, "buyer in Uyo (vendor send keeps failing)")
rg2 = wait(lambda: req_of(BG2)[0], T)
ok(wait(lambda: sql(f"SELECT reason FROM workflow_exceptions WHERE buyer_request_id = '{rg2}'") == "delivery_retry_exhausted", 60),
   "retries exhausted -> SYSTEM_EXCEPTION for the operator")
BG3 = f"23490{RUN}11"
ok(say(BG3, "I need a PS5 in Calabar") == 200, "buyer in Calabar (vendor outside the 24h window: 131047)")
rg3 = wait(lambda: req_of(BG3)[0], T)
ok(wait(lambda: sql(f"SELECT reason FROM workflow_exceptions WHERE buyer_request_id = '{rg3}'") == "provider_policy_rejection", T),
   "policy rejection -> exception immediately, never retried")
ok(int(sql(f"SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_RESEND' AND payload->>'buyer_request_id' = '{rg3}'")) == 0, "no retry job for a policy rejection")

# ---------------------------------------------------------------- metrics
print("\n== autonomy metrics ==")
code, m = api("GET", "/api/autonomy/metrics")
ok(code == 200, "metrics endpoint")
print(json.dumps({k: m[k] for k in ["total_workflows", "workflows_by_class", "manual_routing", "manual_confirmation", "operator_intervention",
                                     "clarification_resolution", "automatic_recovery", "open_exceptions", "exceptions_by_reason"]}, indent=1))
ok(m["manual_routing"]["n"] == 0 and m["manual_routing"]["of"] >= 8, "manual routing 0", json.dumps(m["manual_routing"]))
ok(m["manual_confirmation"]["n"] == 0 and m["manual_confirmation"]["of"] >= 8, "manual confirmation 0", json.dumps(m["manual_confirmation"]))
ok(m["operator_intervention"]["n"] == 0, "operator interventions 0", json.dumps(m["operator_intervention"]))
ok(m["automatic_recovery"]["n"] >= 1, "automatic recovery measured", json.dumps(m["automatic_recovery"]))
ok(m["workflows_by_class"].get("system_failure", 0) == 2, "the two unrecoverable delivery cases are the only system failures")
ok(m["autonomous_completion"]["n"] == m["autonomous_completion"]["of"] and m["autonomous_completion"]["of"] == 2,
   "every transaction completed autonomously", json.dumps(m["autonomous_completion"]))
ok(int(sql("SELECT count(*) FROM events WHERE actor_type = 'OPERATOR' AND event_type IN ('REQUEST_CONFIRMED', 'VENDORS_ROUTED', 'CLARIFICATION_SENT', 'VENDOR_RESPONSE_ASSOCIATION_CORRECTED', 'VENDOR_RESPONSE_CORRECTED')")) == 0,
   "no operator commerce event in the whole run")
done()
