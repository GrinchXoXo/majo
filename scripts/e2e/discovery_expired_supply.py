#!/usr/bin/env python3
"""
Phase 4B E2E 4/4 — expired supply.

Three listings reach a staff buyer. One then passes the 60-day ceiling.
Expired supply is not discoverable, not matchable, not deliverable (also when
it was already PLANNED, or already shown and the buyer taps late), but is
RETAINED: listing row, media, source messages, audit trail. One listing's
expiry never blocks the others (partial-failure isolation).
"""
import json
import time
import urllib.request

from discovery_common import *  # noqa: F401,F403

STAFF = "2347300000003"
print(f"Majo Phase 4B expired-supply E2E · run {RUN} · backend {BASE}")

specs = [("a", "TV Palace", "Samsung TV 55 inch ₦450,000", 40), ("b", "Laptop Hub", "HP Laptop 15 inch ₦300,000", 90), ("c", "Phone Zone", "iPhone 13 128gb ₦620,000", 150)]
L, V, P = {}, {}, {}
print("\n1. Three live listings")
for i, (tag, name, cap, shade) in enumerate(specs):
    P[tag] = "23473" + RUN + f"5{i}"
    V[tag] = vendor(f"{name} {RUN}", P[tag], "GRA, Benin City", ["electronics"])
    L[tag] = published_listing(V[tag], P[tag], tag, cap, shade)
ok(say(STAFF, "hello majo") == 200, "staff messages Majo")
ok(wait(lambda: sql(f"SELECT count(*) FROM buyers WHERE external_user_id = '{STAFF}'") == "1"), "buyer exists")
time.sleep(1)
code, _ = opt_in(STAFF, "GRA, Benin City", ["electronics"])
ok(code == 200, "staff opted in")

print("\n2. Batch 1 shows all three")
code, b = batch("exp-1-" + RUN, [STAFF], "electronics")
bid = b["batch"]["id"]
ok(code == 201, "batch PLANNED")
_, bd = api("GET", f"/api/discovery/batches/{bid}")
did = bd["deliveries"][0]["id"]
ok(len(bd["deliveries"][0]["listing_ids"]) == 3, "delivery plans 3 listings")
code, r = api("POST", f"/api/discovery/batches/{bid}/send")
ok(r["sent"] == 1, "sent", json.dumps(r))
msg = [m for m in outbound(STAFF) if m["body"].get("type") == "interactive"][-1]
ids = interactive_ids(msg)
ok(msg["body"]["interactive"]["type"] == "list" and all(any(L[t]["public_reference"] in i for i in ids) for t in "abc"), "a list with one row per listing", f"{len(ids)} ids")
rowA = [i for i in ids if L["a"]["public_reference"] in i][0]
ok(tap_list(STAFF, rowA, "Samsung TV") == 200, "staff opens listing A")
ok(wait(lambda: any(L["a"]["public_reference"] in m["body"].get("interactive", {}).get("body", {}).get("text", "") for m in outbound(STAFF))), "A's card shown while it is alive")

print("\n3. A passes the 60-day ceiling")
media_before = sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{V['a']}'")
msgs_before = sql(f"SELECT count(*) FROM supply_signals WHERE vendor_id = '{V['a']}'")
travel(L["a"]["id"], V["a"], 61)
_, d = api("GET", f"/api/supply-listings/{L['a']['id']}")
ok("lifecycle_expired" in d["eligibility"]["reasons"] and not d["eligibility"]["eligible"], "canonical policy: EXPIRED", ", ".join(d["eligibility"]["reasons"]))
ok(sql(f"SELECT status FROM supply_listings WHERE id = '{L['a']['id']}'") == "ACTIVE", "status column still ACTIVE (the policy decides, no sweep)")

print("\n4. Not deliverable: a late 'Ask about it' on the expired listing")
reqs0 = len(api("GET", "/api/requests")[1] or [])
askA = [i for i in interactive_ids([m for m in outbound(STAFF) if L["a"]["public_reference"] in m["body"].get("interactive", {}).get("body", {}).get("text", "")][-1]) if i.endswith(":ask")][0]
n0 = len(outbound(STAFF))
ok(tap_button(STAFF, askA, "Ask about it") == 200, "buyer taps Ask on A after it expired")
ok(wait(lambda: len(outbound(STAFF)) > n0), "buyer gets an honest answer")
last = [m for m in outbound(STAFF)][-1]["body"]["text"]["body"]
ok("no longer available" in last and L["a"]["public_reference"] in last, "told it is no longer available", last)
time.sleep(1)
ok(len(api("GET", "/api/requests")[1] or []) == reqs0, "no BuyerRequest was created")
ok(sql("SELECT string_agg(reason, ',') FROM discovery_interactions WHERE outcome = 'UNRESOLVED'") == "listing_no_longer_available", "interaction persisted as UNRESOLVED / listing_no_longer_available")

print("\n5. Not discoverable")
_, c = api("GET", "/api/discovery/candidates?location=GRA%2C%20Benin%20City&category=electronics&limit=10")
refs = [x["public_reference"] for x in c["candidates"]]
ok(L["a"]["public_reference"] not in refs and L["b"]["public_reference"] in refs and L["c"]["public_reference"] in refs, "A is absent; B and C remain", ",".join(refs))

print("\n6. Not matchable")
ok(say(STAFF, "I need a Samsung TV 55 inch, budget 500k") == 200, "buyer asks for the same TV in words")
req = wait(lambda: [r_ for r_ in (api("GET", "/api/requests")[1] or []) if r_.get("raw_message", "").startswith("I need a Samsung TV") and r_["status"] == "NEEDS_REVIEW"])
ok(req, "request extracted")
api("POST", f"/api/requests/{req[0]['id']}/confirm")
_, m = api("GET", f"/api/requests/{req[0]['id']}/matches")
mm = [x for x in m if x["vendor_id"] == V["a"]]
ok(not mm or not mm[0].get("relevant_listing"), "the expired listing is not used by matching", "vendor listed without a listing" if mm else "vendor not matched")

print("\n7. Not deliverable: planned while alive, expired before the send")
sql("UPDATE discovery_deliveries SET claimed_at = claimed_at - interval '30 days' WHERE claimed_at IS NOT NULL")
sql("UPDATE discovery_exposures SET exposed_at = exposed_at - interval '30 days'")
sql("UPDATE messages SET created_at = created_at - interval '2 hours' WHERE direction = 'INBOUND'")
code, b2 = batch("exp-2-" + RUN, [STAFF], "electronics")
_, bd2 = api("GET", f"/api/discovery/batches/{b2['batch']['id']}")
ok(len(bd2["deliveries"]) == 1, "batch 2 planned")
planned = bd2["deliveries"][0]["listing_ids"]
ok(L["a"]["id"] not in planned and L["b"]["id"] in planned and L["c"]["id"] in planned, "planned with B and C (A already excluded)")
travel(L["c"]["id"], V["c"], 61)
n0 = len(outbound(STAFF))
code, r = api("POST", f"/api/discovery/batches/{b2['batch']['id']}/send")
ok(code == 200 and r["sent"] == 1, "send still succeeds (one listing's expiry does not block the delivery)", json.dumps(r))
new = outbound(STAFF)[n0:]
blob = json.dumps([m_["body"] for m_ in new])
ok(L["b"]["public_reference"] in blob and L["c"]["public_reference"] not in blob and L["a"]["public_reference"] not in blob,
   "only B was presented: C expired between planning and sending, A long expired")

print("\n8. Expired supply is retained as history")
for t in "ac":
    ok(sql(f"SELECT count(*) FROM supply_listings WHERE id = '{L[t]['id']}'") == "1", f"listing {t.upper()} row kept")
ok(sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id = '{V['a']}'") == media_before, "A's media assets all kept", media_before)
ok(sql(f"SELECT count(*) FROM supply_signals WHERE vendor_id = '{V['a']}'") == msgs_before, "A's source signals kept")
_, d = api("GET", f"/api/supply-listings/{L['a']['id']}")
ok(len(d["media"]) == 2 and len(d["source_messages"]) == 3, "detail still shows media and provenance")
ok(all(urllib.request.urlopen(urllib.request.Request(BASE + m_["preview_path"], headers={"Authorization": "Bearer " + TOKEN})).status == 200 for m_ in d["media"] if m_.get("preview_path")), "media still viewable")
ok(int(sql(f"SELECT count(*) FROM audit_logs WHERE entity_id = '{L['a']['id']}'")) >= 1 and sql(f"SELECT count(*) FROM discovery_exposures WHERE listing_id = '{L['a']['id']}'") == "1",
   "audit trail and the exposure record of A are kept (so attribution stays truthful)")
_, hist = api("GET", "/api/supply-listings?status=ACTIVE&lifecycle=expired")
ok(any(x["id"] == L["a"]["id"] for x in hist) and any(x["id"] == L["c"]["id"] for x in hist), "A and C are in the expired history view")
done()
