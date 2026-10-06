#!/usr/bin/env python3
"""
Phase 4B E2E 1/4 — staff Discovery -> commerce loop (INTERNAL mode only).

A vendor posts a TV; an allowlisted STAFF test number opts in, gets a dry run,
a batch, an operator-triggered send; taps "Ask about it"; the tap becomes a
BuyerRequest (origin=discovery, listing_id, discovery_delivery_id) that waits
for operator confirmation like any other request, is routed to the listing's
vendor, becomes an Offer carrying the listing, is selected, and creates an
UNPAID Transaction. Also proves the safety rails: a non-allowlisted number is
never sent to, duplicate sends/taps are idempotent, unknown / foreign / stale
ids mutate nothing but are persisted, and STOP is honoured.
"""
import json
import time

from discovery_common import *  # noqa: F401,F403

STAFF = "2347300000001"
OUTSIDER = "2349100000" + RUN[:4]  # a real-customer-like number: NOT on the allowlist
print(f"Majo Phase 4B staff-commerce E2E · run {RUN} · backend {BASE}")

print("\n0. Feature gate")
_, s = api("GET", "/api/discovery/settings")
ok(s["mode"] == "INTERNAL" and s["send_allowed"] and s["internal_recipients"] == 3, "mode INTERNAL, 3 staff numbers allowlisted")
code, _ = api("GET", "/api/discovery/settings", token="")
ok(code == 401, "discovery endpoints are operator-only (no token -> 401)")

print("\n1. Supply: a vendor publishes a TV (canonical lifecycle, 60 days)")
VP = "23473" + RUN + "55"
vid = vendor("TV Palace " + RUN, VP, "GRA, Benin City", ["electronics"])
L = published_listing(vid, VP, "a", "Samsung TV 55 inch ₦450,000")
ref = L["public_reference"]

print("\n2. Consent: separate from Buyer and BuyerRequest, auditable")
code, p = opt_in(OUTSIDER, "GRA, Benin City", ["electronics"])
ok(code == 403, "a non-staff number cannot be opted in by an operator in INTERNAL mode", str(code))
code, _ = opt_in(STAFF, "GRA, Benin City", ["electronics"])
ok(code == 404, "a number that never messaged Majo is not a buyer: no consent record is invented", str(code))
ok(say(STAFF, "hello majo") == 200, "staff member messages Majo (becomes a buyer; opens the 24h window)")
ok(wait(lambda: sql(f"SELECT count(*) FROM buyers WHERE external_user_id = '{STAFF}'") == "1"), "buyer exists")
time.sleep(1.5)
reqs0 = sql("SELECT count(*) FROM buyer_requests")
code, p = opt_in(STAFF, "GRA, Benin City", ["electronics"])
ok(code == 200 and p["status"] == "OPTED_IN" and "staff" in p["source"], "staff number OPTED_IN (explicit staff record)", p.get("source", ""))
ok(sql(f"SELECT count(*) FROM discovery_preference_history WHERE buyer_id = '{p['buyer_id']}'") == "1"
   and int(sql(f"SELECT count(*) FROM events WHERE event_type = 'DISCOVERY_OPTED_IN'")) >= 1,
   "consent change recorded in append-only history + event")
ok(sql("SELECT count(*) FROM buyer_requests") == reqs0, "recording consent created no BuyerRequest (separate concept)")

print("\n3. Discovery is pure: candidates come with reasons, no side effects")
before = len(outbound())
code, c = api("GET", "/api/discovery/candidates?location=" + "GRA%2C%20Benin%20City" + "&category=electronics")
ok(code == 200 and any(x["public_reference"] == ref for x in c["candidates"]), "Discovery returns the listing", f"{len(c['candidates'])} candidate(s)")
cand = [x for x in c["candidates"] if x["public_reference"] == ref][0]
ok(cand["reasons"] and cand["score"] == sum(r["points"] for r in cand["reasons"]), "score = sum of explainable reasons",
   "; ".join(f"{r['points']:+d} {r['dimension']}" for r in cand["reasons"]))
ok(len(outbound()) == before and sql("SELECT count(*) FROM discovery_batches") == "0", "Discovery sent nothing and created no batch")

print("\n4. Dry run: nothing persisted, nothing sent")
code, pv = preview([STAFF, OUTSIDER], "electronics")
ok(code == 200 and pv["deliveries"] == 1 and pv["mode"] == "INTERNAL", "dry run plans exactly 1 delivery (the staff number)", json.dumps(pv["excluded_buyers"]))
ok(sql("SELECT count(*) FROM discovery_batches") == "0" and sql("SELECT count(*) FROM discovery_deliveries") == "0" and len(outbound()) == before,
   "dry run persisted nothing and sent nothing")

print("\n5. Batch is PLANNED; sending is a separate operator action")
code, b = batch("e2e-" + RUN, [STAFF, OUTSIDER], "electronics")
ok(code == 201 and b["created"] and b["batch"]["status"] == "PLANNED" and b["batch"]["ranking_version"].startswith("discovery-det-v1+"),
   "batch PLANNED with ranking version", b["batch"]["ranking_version"])
bid = b["batch"]["id"]
code, b2 = batch("e2e-" + RUN, [STAFF, OUTSIDER], "electronics")
ok(code == 200 and not b2["created"] and b2["batch"]["id"] == bid, "same client_key -> same batch (idempotent)")
ok(len(outbound()) == before, "creating a batch sends nothing")
_, bd = api("GET", f"/api/discovery/batches/{bid}")
ok(len(bd["deliveries"]) == 1 and bd["deliveries"][0]["recipient"] == STAFF and bd["deliveries"][0]["dedupe_key"] == f"batch:{bid}:buyer:{bd['deliveries'][0]['buyer_id']}",
   "one delivery, deterministic dedupe key")
did = bd["deliveries"][0]["id"]

print("\n6. Send: the 24h session window decides the form of the first message")
sql("UPDATE messages SET created_at = created_at - interval '3 days' WHERE direction = 'INBOUND'")
sent0 = len(outbound(STAFF))
code, r = api("POST", f"/api/discovery/batches/{bid}/send")
ok(code == 200 and r["sent"] == 0 and r["skipped"].get("outside_session_no_template") == 1, "no inbound in 24h -> template required, none configured -> skipped, fails closed", json.dumps(r))
_, d = api("GET", f"/api/discovery/deliveries/{did}")
ok(d["delivery"]["status"] == "SKIPPED" and d["delivery"]["skip_reason"] == "outside_session_no_template", "delivery SKIPPED with a clear reason", d["delivery"]["status"])
ok(len(outbound(STAFF)) == sent0, "nothing reached the phone")
# the staff member's inbound is older than... make it so: a fresh inbound opens the 24h window
ok(say(STAFF, "hello again") == 200, "after 3 days of silence the staff member messages Majo again (opens the 24h window)")
time.sleep(1)
code, b = batch("e2e-" + RUN + "-2", [STAFF, OUTSIDER], "electronics")
bid = b["batch"]["id"]
_, bd = api("GET", f"/api/discovery/batches/{bid}")
ok(len(bd["deliveries"]) == 1, "second batch planned (the skipped delivery did not consume the cap)")
did = bd["deliveries"][0]["id"]
arm = bd["deliveries"][0]["arm"]
code, r = api("POST", f"/api/discovery/batches/{bid}/send")
ok(code == 200 and r["sent"] == 1 and r["failed"] == 0, "sent to the staff number only", json.dumps(r))
ok(len(outbound(OUTSIDER)) == 0, "the non-allowlisted number received NOTHING")
msgs = [m for m in outbound(STAFF) if m["body"].get("type") == "interactive"]
ok(len(msgs) == 1 and msgs[0]["body"]["interactive"]["type"] == "button", f"staff received a rich card ({arm})")
body = msgs[0]["body"]["interactive"]
ok(ref in body["body"]["text"] and "450,000" in body["body"]["text"] and "TV Palace" not in json.dumps(msgs[0]["body"]),
   "card shows the public L-reference and price, never the vendor identity", body["body"]["text"].replace("\n", " | "))
ok(body.get("header", {}).get("type") == "image" and body["header"]["image"]["id"].startswith("up."), "image header uses an uploaded provider media id (media reuse)",
   body["header"]["image"]["id"])
ids = interactive_ids(msgs[0])
ask_id = [i for i in ids if i.endswith(":ask")][0]
ok(ask_id.startswith(f"dd:{did}:l:{ref}") and any(i.endswith(":not_interested") for i in ids), "structured ids are delivery- and listing-scoped", ask_id)
code, r2 = api("POST", f"/api/discovery/batches/{bid}/send")
ok(r2["already_sent"] and len(outbound(STAFF)) == len(msgs) + 0 + len([m for m in outbound(STAFF) if m["body"].get("type") != "interactive"]), "re-sending the batch is a no-op (idempotent)")
n_uploads = len(uploads())
ok(n_uploads == 1, "exactly one media upload so far", str(n_uploads))

print("\n7. Interaction: structured id > everything else; bad ids mutate nothing but are persisted")
reqs_before = len(api("GET", "/api/requests")[1] or [])
other = sql("SELECT gen_random_uuid()")
for label, rid_ in [("malformed", "dd:not-a-uuid:l:L-1:ask"), ("unknown delivery", f"dd:{other}:l:{ref}:ask"),
                    ("unknown listing", f"dd:{did}:l:L-999999:ask")]:
    ok(tap_button(STAFF, rid_) == 200, f"{label} id accepted by the webhook")
time.sleep(1.5)
ok(len(api("GET", "/api/requests")[1] or []) == reqs_before, "none of them created a BuyerRequest")
ok(int(sql(f"SELECT count(*) FROM discovery_interactions WHERE outcome <> 'RESOLVED'")) >= 3, "all three persisted as unresolved interactions",
   sql("SELECT string_agg(outcome, ',') FROM discovery_interactions"))

print("\n8. 'Ask about it' becomes a BuyerRequest — the existing commerce loop")
ok(tap_button(STAFF, ask_id, "Ask about it") == 200, "staff taps Ask about it")
req = wait(lambda: [r for r in (api("GET", "/api/requests")[1] or []) if r.get("discovery_delivery_id") == did])
ok(req, "BuyerRequest created from the tap")
req = req[0]
rid = req["id"]
ok(req["origin"] == "discovery" and req["listing_id"] == L["id"] and req["status"] == "NEEDS_REVIEW",
   "origin=discovery, listing_id, discovery_delivery_id set; NEEDS_REVIEW (never auto-confirmed)", f"{req['origin']} / {req['status']}")
ok(wait(lambda: any("checking with the seller" in (m["body"].get("text", {}).get("body", "")) for m in outbound(STAFF))), "staff gets an honest acknowledgement (nothing reserved or paid)")
ok(tap_button(STAFF, ask_id, "Ask about it") == 200, "duplicate tap")
time.sleep(1.5)
ok(len([r for r in api("GET", "/api/requests")[1] if r.get("discovery_delivery_id") == did]) == 1, "duplicate tap created NO second request (idempotent)")
code, _ = api("GET", f"/api/requests/{rid}/matches")
ok(code == 409, "confirmation rule preserved: no matching/routing while NEEDS_REVIEW", str(code))

print("\n9. Confirm -> vendor flow (listing provenance)")
code, r = api("POST", f"/api/requests/{rid}/confirm")
ok(code == 200 and r["status"] == "CONFIRMED", "operator confirms (unchanged step)")
code, vrs = api("POST", f"/api/requests/{rid}/route", {"vendor_ids": [vid]})
ok(code == 200 and len(vrs) == 1, "routed to the listing's vendor")
vr = vrs[0]
ok(vr.get("supply_listing_id") == L["id"], "VendorRequest carries the listing id (provenance)")
vmsg = [m for m in outbound(VP) if "text" in m["body"]][-1]["body"]["text"]["body"]
ok(ref in vmsg, "vendor message identifies the exact listing and asks to reconfirm availability", vmsg.replace("\n", " | ")[:120])
ok(say(VP, "Yes still available, 450k") == 200, "vendor confirms availability + price")
off = wait(lambda: [o for o in (api("GET", f"/api/requests/{rid}/offers")[1] or [])])
ok(off, "Offer generated")
ok(off[0].get("supply_listing_id") == L["id"], "Offer inherits the listing (provenance)", str(off[0].get("price")))
ok(wait(lambda: any("1." in (m["body"].get("text", {}).get("body", "")) and "450,000" in m["body"].get("text", {}).get("body", "") for m in outbound(STAFF))),
   "offer delivered to the staff buyer on WhatsApp")
ok(say(STAFF, "1") == 200, "staff selects option 1")
sel = wait(lambda: (api("GET", f"/api/requests/{rid}/selection")[1] or {}).get("transaction"))
ok(sel and sel["payment_status"] == "UNPAID" and sel["gross_amount"] == 450000, "Transaction created, UNPAID", f"gross={sel['gross_amount']}")

print("\n10. Attribution: delivery -> request -> offer -> selection -> transaction")
_, d = api("GET", f"/api/discovery/deliveries/{did}")
ok(d["attribution"]["requests"] and d["attribution"]["requests"][0]["origin"] == "discovery", "request attributed")
ok(d["attribution"]["offers"] and d["attribution"]["offers"][0]["supply_listing_id"] == L["id"], "offer attributed to the listing")
ok(d["attribution"]["selections"] and d["attribution"]["transactions"], "selection and transaction attributed")
_, f = api("GET", f"/api/discovery/funnel?batch_id={bid}")
ok(f["deliveries_sent"] == 1 and f["interacted"] >= 1 and f["asked"] == 1 and f["requests_created"] == 1 and f["offers"] == 1
   and f["selections"] == 1 and f["transactions"] == 1, "funnel counts line up (asked counts distinct delivery+listing, not taps)", json.dumps({k: f[k] for k in ("deliveries_sent", "asked", "offers", "selections", "transactions")}))
_, fall = api("GET", "/api/discovery/funnel")
ok(f["unresolved_interactions"] == 1 and fall["unresolved_interactions"] >= 3, "unresolved interactions visible to the operator (1 attributable to this batch, 3 overall)",
   f"{f['unresolved_interactions']} / {fall['unresolved_interactions']}")

print("\n11. Caps, dedupe and STOP")
code, b3 = batch("e2e-" + RUN + "-3", [STAFF], "electronics")
_, bd3 = api("GET", f"/api/discovery/batches/{b3['batch']['id']}")
ok(len(bd3["deliveries"]) == 0, "frequency cap (1 per 72h) excludes the buyer from a new batch")
code, pv = preview([STAFF], "electronics")
ok(pv["deliveries"] == 0 and pv["excluded_buyers"], "dry run says why", json.dumps(pv["excluded_buyers"]))
sql("UPDATE discovery_deliveries SET claimed_at = claimed_at - interval '30 days' WHERE claimed_at IS NOT NULL")
sql("UPDATE discovery_exposures SET exposed_at = exposed_at - interval '30 days'")
code, b4 = batch("e2e-" + RUN + "-4", [STAFF], "electronics")
_, bd4 = api("GET", f"/api/discovery/batches/{b4['batch']['id']}")
ok(len(bd4["deliveries"]) == 1, "30 days later the cap and exposure windows have passed: a new delivery is planned (PLANNED, not sent)")
ok(say(STAFF, "STOP") == 200, "staff replies STOP before the operator sends it")
ok(wait(lambda: sql(f"SELECT status FROM discovery_preferences WHERE buyer_id = (SELECT id FROM buyers WHERE external_user_id = '{STAFF}')") == "OPTED_OUT"), "consent OPTED_OUT")
ok(wait(lambda: any("won't get Majo finds" in (m["body"].get("text", {}).get("body", "")) for m in outbound(STAFF))), "STOP acknowledged")
ok(int(sql(f"SELECT count(*) FROM discovery_preference_history WHERE buyer_id = (SELECT id FROM buyers WHERE external_user_id = '{STAFF}')")) >= 2, "opt-out is auditable (history rows kept)")
before = len(outbound(STAFF))
code, r = api("POST", f"/api/discovery/batches/{b4['batch']['id']}/send")
ok(code == 200 and r["sent"] == 0 and r["skipped"].get("opted_out") == 1 and len(outbound(STAFF)) == before,
   "the already-planned delivery is re-checked at send time: skipped (opted_out), nothing sent", json.dumps(r["skipped"]))
code, pv = preview([STAFF], "electronics")
ok(pv["deliveries"] == 0 and "opted_out" in json.dumps(pv["excluded_buyers"]), "opted-out buyer is never planned", json.dumps(pv["excluded_buyers"]))
ok(say(STAFF, "I need a PS5 slim in Lagos, 550k") == 200, "STOP does not block normal commerce: staff can still make a request")
ok(wait(lambda: any(r_.get("raw_message", "").startswith("I need a PS5") for r_ in (api("GET", "/api/requests")[1] or []))), "free-text request still handled")
done()
