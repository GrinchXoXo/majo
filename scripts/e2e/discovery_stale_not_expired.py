#!/usr/bin/env python3
"""
Phase 4B E2E 3/4 — stale is NOT expired.

A listing published 35 days ago and last verified 5 days ago is STALE
(freshness window 72h) but alive (60-day ceiling). Discovery keeps it with
the note stale_needs_reconfirmation; the card never claims availability; an
"Ask about it" creates a discovery-origin request whose vendor message asks
to reconfirm; the vendor's AVAILABLE answer moves freshness ONLY — never the
ceiling. Verified by SQL time travel.
"""
import json
import time

from discovery_common import *  # noqa: F401,F403

STAFF = "2347300000002"
print(f"Majo Phase 4B stale≠expired E2E · run {RUN} · backend {BASE}")

VP = "23473" + RUN + "55"
vid = vendor("Stale Stores " + RUN, VP, "GRA, Benin City", ["electronics"])
L = published_listing(vid, VP, "s", "Samsung TV 55 inch ₦450,000")
ref = L["public_reference"]
travel(L["id"], vid, 35)
sql(f"UPDATE supply_listings SET last_verified_at = now() - interval '5 days' WHERE id = '{L['id']}'")
exp0 = sql(f"SELECT expires_at FROM supply_listings WHERE id = '{L['id']}'")

print("\n1. Canonical policy: day 35, STALE, still eligible")
_, d = api("GET", f"/api/supply-listings/{L['id']}")
el = d["eligibility"]
ok(el["eligible"] and el["lifecycle"]["day"] == 35 and el["lifecycle"]["state"] != "EXPIRED", "day 35/60: alive and eligible", f"day {el['lifecycle']['day']}")
ok(el["freshness"]["state"] == "STALE" and "stale_needs_reconfirmation" in el["notes"], "freshness STALE (verified 5 days ago > 72h)", el["freshness"]["state"])
ok(el["geography"]["stage"] == "STATE", "geographic stage STATE at day 35", el["geography"]["stage"])

print("\n2. Discovery keeps stale supply, flagged, with an honest reason")
_, c = api("GET", "/api/discovery/candidates?location=GRA%2C%20Benin%20City&category=electronics")
cand = [x for x in c["candidates"] if x["public_reference"] == ref]
ok(cand and "stale_needs_reconfirmation" in cand[0]["notes"], "candidate present with note stale_needs_reconfirmation")
ok(not any(r["dimension"] == "freshness" and r["points"] > 0 for r in cand[0]["reasons"]), "no freshness points awarded to a stale listing", "; ".join(str(r) for r in cand[0]["reasons"] if r["dimension"] == "freshness") or "none")

print("\n3. Distribution: the card does not claim availability")
ok(say(STAFF, "hello majo") == 200, "staff messages Majo")
ok(wait(lambda: sql(f"SELECT count(*) FROM buyers WHERE external_user_id = '{STAFF}'") == "1"), "buyer exists")
time.sleep(1)
code, p = opt_in(STAFF, "GRA, Benin City", ["electronics"])
code, pv = preview([STAFF], "electronics")
ok(pv["deliveries"] == 1, "dry run plans the stale listing", json.dumps(pv["excluded_buyers"]))
code, b = batch("stale-" + RUN, [STAFF], "electronics")
bid = b["batch"]["id"]
code, r = api("POST", f"/api/discovery/batches/{bid}/send")
ok(code == 200 and r["sent"] == 1, "sent", json.dumps(r))
msg = [m for m in outbound(STAFF) if m["body"].get("type") == "interactive"][-1]
text = msg["body"]["interactive"]["body"]["text"]
ok("Availability to be confirmed with the seller" in text and ref in text, "caption says availability is to be confirmed", text.replace("\n", " | "))
ok("in stock" not in text.lower() and "available now" not in text.lower(), "no availability promise in the card")
_, bd = api("GET", f"/api/discovery/batches/{bid}")
did = bd["deliveries"][0]["id"]
ok(bd["deliveries"][0]["candidates"][0]["needs_reconfirmation"] and bd["deliveries"][0]["candidates"][0]["eligibility"]["freshness"] == "STALE",
   "the delivery records the freshness the buyer was shown (STALE, needs reconfirmation)")

print("\n4. Ask -> request -> vendor reconfirmation")
ask = [i for i in interactive_ids(msg) if i.endswith(":ask")][0]
ok(tap_button(STAFF, ask, "Ask about it") == 200, "staff taps Ask about it")
req = wait(lambda: [r_ for r_ in (api("GET", "/api/requests")[1] or []) if r_.get("discovery_delivery_id") == did])
ok(req and req[0]["origin"] == "discovery" and req[0]["status"] == "NEEDS_REVIEW", "discovery-origin request, NEEDS_REVIEW")
rid = req[0]["id"]
api("POST", f"/api/requests/{rid}/confirm")
code, m = api("GET", f"/api/requests/{rid}/matches")
mm = [x for x in m if x["vendor_id"] == vid]
ok(mm and mm[0].get("relevant_listing") and mm[0]["relevant_listing"]["needs_reconfirmation"], "matching also flags it for reconfirmation")
code, vrs = api("POST", f"/api/requests/{rid}/route", {"vendor_ids": [vid]})
vmsg = [m_ for m_ in outbound(VP) if "text" in m_["body"]][-1]["body"]["text"]["body"]
ok(ref in vmsg and "listing" in vmsg.lower(), "vendor is asked about this exact listing", vmsg.replace("\n", " | ")[:100])
ver0 = sql(f"SELECT last_verified_at FROM supply_listings WHERE id = '{L['id']}'")
ok(say(VP, "Yes still available, 450k") == 200, "vendor reconfirms")
ok(wait(lambda: sql(f"SELECT (last_verified_at > '{ver0}')::text FROM supply_listings WHERE id = '{L['id']}'") == "true"), "last_verified_at moved (freshness refreshed by the vendor)")

print("\n5. Freshness moved; the ceiling did not")
_, d = api("GET", f"/api/supply-listings/{L['id']}")
el = d["eligibility"]
ok(el["freshness"]["state"] == "FRESH" and el["lifecycle"]["day"] == 35, "FRESH again, still day 35")
ok(sql(f"SELECT expires_at FROM supply_listings WHERE id = '{L['id']}'") == exp0, "expires_at (the 60-day ceiling) is unchanged by reconfirmation")
ok(sql(f"SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_VERIFIED' AND entity_id = '{L['id']}'") == "1"
   and sql(f"SELECT count(*) FROM audit_logs WHERE action = 'SUPPLY_LISTING_VERIFIED' AND entity_id = '{L['id']}'") == "1", "the vendor reconfirmation is audited and evented once (as a verification)")
_, c = api("GET", "/api/discovery/candidates?location=GRA%2C%20Benin%20City&category=electronics")
cand = [x for x in c["candidates"] if x["public_reference"] == ref][0]
ok("stale_needs_reconfirmation" not in cand["notes"], "Discovery no longer flags it")
done()
