#!/usr/bin/env python3
"""
Phase 4B E2E 2/4 — geographic expansion consumed by Discovery.

One listing (origin GRA, Benin City, Edo, South-South), five hypothetical
viewers, five points in its life. Discovery owns no geography: every answer
comes from the canonical supply.EligibilityPolicy (LOCAL 0d, NEARBY 20d,
STATE 30d, REGION 45d, ceiling 60d), reached through ViewerReach. Time is
simulated by moving the listing's timestamps into the past in SQL.
Also checks the Distribution planner uses the same answer for a staff viewer.
"""
import json
import time
import urllib.parse

from discovery_common import *  # noqa: F401,F403

print(f"Majo Phase 4B geographic-expansion E2E · run {RUN} · backend {BASE}")
VIEWERS = {"local": "GRA, Benin City", "city": "Benin City", "state": "Edo", "region": "Warri", "country": "Lagos"}
STAFF = "2347300000001"

VP = "23473" + RUN + "55"
vid = vendor("Edo Gadgets " + RUN, VP, "GRA, Benin City", ["electronics"])
L = published_listing(vid, VP, "g", "Samsung TV 55 inch ₦450,000")
ref = L["public_reference"]


def reach(location):
    q = urllib.parse.urlencode({"location": location, "category": "electronics", "limit": 10})
    code, c = api("GET", "/api/discovery/candidates?" + q)
    assert code == 200, (code, c)
    cand = [x for x in c["candidates"] if x["public_reference"] == ref]
    exc = [x for x in c["exclusions"] if x["public_reference"] == ref]
    return (cand[0] if cand else None), (exc[0] if exc else None)


def row(day, expect):
    """expect: viewer -> True (discoverable) / False."""
    got = {}
    for name, loc in VIEWERS.items():
        cand, exc = reach(loc)
        got[name] = bool(cand)
        if cand:
            ok(cand["reach"]["reachable"], f"day {day}: {name:7s} ({loc}) discoverable", cand["reach"]["reason"][:90])
        else:
            if day >= 60:  # the storage pre-filter drops past-ceiling supply before the pipeline: no row at all
                ok(exc is None and not expect[name], f"day {day}: {name:7s} ({loc}) NOT discoverable (past the 60-day ceiling, never retrieved)")
            else:
                ok(exc and exc["stage"] in ("eligibility", "retrieval", "filter") and not expect[name],
                   f"day {day}: {name:7s} ({loc}) NOT discoverable", f"{exc['code']}: {exc.get('detail', '')[:80]}" if exc else "no exclusion row")
        ok(got[name] == expect[name], f"day {day}: {name} expectation holds")


print("\nDay 0 — LOCAL: only the origin area")
row(0, {"local": True, "city": False, "state": False, "region": False, "country": False})
_, c = api("GET", "/api/discovery/candidates?location=" + urllib.parse.quote("GRA, Benin City") + "&category=electronics")
ok(c["candidates"][0]["eligibility"]["geography"]["stage"] == "LOCAL", "canonical stage reported: LOCAL")

print("\nDay 21 — NEARBY: the rest of the city")
travel(L["id"], vid, 21)
row(21, {"local": True, "city": True, "state": False, "region": False, "country": False})

print("\nDay 31 — STATE: all of Edo")
travel(L["id"], vid, 10)
row(31, {"local": True, "city": True, "state": True, "region": False, "country": False})

print("\nDay 46 — REGION: South-South")
travel(L["id"], vid, 15)
row(46, {"local": True, "city": True, "state": True, "region": True, "country": False})
cand, _ = reach("Warri")
ok(cand["reach"]["level"] == "REGION", "Warri is related at the REGION level", cand["reach"]["level"])
loc_pts = {n: sum(r["points"] for r in reach(l)[0]["reasons"] if r["dimension"].startswith("location")) for n, l in VIEWERS.items() if reach(l)[0]}
ok(loc_pts["local"] > loc_pts["city"] > loc_pts["state"] > loc_pts["region"], "location points shrink with distance (explainable ranking)", json.dumps(loc_pts))

print("\nDay 61 — EXPIRED: the ceiling wins over every geography")
travel(L["id"], vid, 15)
row(61, {"local": False, "city": False, "state": False, "region": False, "country": False})
_, d = api("GET", f"/api/supply-listings/{L['id']}")
ok(d["eligibility"]["geography"]["stage"] == "EXPIRED" and "lifecycle_expired" in d["eligibility"]["reasons"], "the canonical policy says why: lifecycle_expired (not a geography reason)")

print("\nDistribution uses the same answer (planner, staff viewer in Warri)")
sql(f"UPDATE supply_listings SET published_at = published_at + interval '15 days', expires_at = expires_at + interval '15 days', last_verified_at = last_verified_at + interval '15 days' WHERE id = '{L['id']}'")
sql(f"UPDATE supply_signals SET created_at = created_at + interval '15 days', expires_at = expires_at + interval '15 days' WHERE vendor_id = '{vid}'")
ok(say(STAFF, "hello majo") == 200, "staff member messages Majo")
ok(wait(lambda: sql(f"SELECT count(*) FROM buyers WHERE external_user_id = '{STAFF}'") == "1"), "buyer exists")
time.sleep(1)
code, p = opt_in(STAFF, "Warri", ["electronics"])
ok(code == 200, "staff viewer opted in with location Warri")
code, pv = preview([STAFF], "electronics")
ok(pv["deliveries"] == 1, "day 46: the REGION stage reaches Warri -> 1 planned delivery", json.dumps(pv["excluded_buyers"]))
sql(f"UPDATE supply_listings SET published_at = published_at + interval '20 days', expires_at = expires_at + interval '20 days', last_verified_at = last_verified_at + interval '20 days' WHERE id = '{L['id']}'")
code, pv = preview([STAFF], "electronics")
ok(pv["deliveries"] == 0, "day 26: STATE stage does not reach Warri -> nothing planned", json.dumps(pv["listing_exclusions"]))
ok(pv["listing_exclusions"].get("geographic") == 1 and pv["excluded_buyers"].get("no_suitable_candidates") == 1,
   "dry run says why: the listing is excluded for geography, so the buyer has no suitable candidate", json.dumps([pv["listing_exclusions"], pv["excluded_buyers"]]))
ok(sql("SELECT count(*) FROM discovery_batches") == "0" and sql("SELECT count(*) FROM discovery_deliveries") == "0", "dry runs persisted nothing")
done()
