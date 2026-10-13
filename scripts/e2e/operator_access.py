#!/usr/bin/env python3
"""
Reuse-audit R1 E2E: operator access control and control-plane read views.

Real backend binary + real PostgreSQL (fresh database per run); no WhatsApp
traffic except one simulated buyer message through the signed webhook.

Checks:
  * every GET /api route refuses a missing or wrong token (401) and accepts the
    shared token; /health, /ready and the webhook stay outside the guard;
  * named operators (cmd/operatorctl): a VIEWER reads but cannot change
    anything (403); an OPERATOR acts, and the event timeline names them;
    revocation takes effect on the next call;
  * CORS: only the configured dashboard origin gets an Access-Control grant;
  * GET /api/events, /api/events/types, /api/jobs, /api/jobs/summary and the
    paginated GET /api/requests (limit/offset, X-Total-Count).

Env: as discovery_common, plus OPERATORCTL (path to the operatorctl binary).
"""
import json
import os
import subprocess
import urllib.error
import urllib.request
import uuid

from discovery_common import RUN, ok, done, say, wait, sql

OPCTL = os.environ["OPERATORCTL"]
DBURL = os.environ["DATABASE_URL"]
BASE = os.environ.get("BASE", "http://localhost:8080")
DASH_ORIGIN = os.environ.get("DASH", "http://localhost:3000")


def raw(method, path, headers=None, body=None):
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None,
                                 method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def opctl(*args):
    r = subprocess.run([OPCTL, *args], env=dict(os.environ, DATABASE_URL=DBURL), capture_output=True, text=True)
    return r.returncode, r.stdout + r.stderr


Z = str(uuid.uuid4())
GETS = ["/api/requests", f"/api/requests/{Z}", f"/api/requests/{Z}/events", "/api/vendors", f"/api/vendors/{Z}",
        f"/api/vendors/{Z}/supply", f"/api/requests/{Z}/matches", f"/api/requests/{Z}/vendor-requests",
        "/api/vendor-responses/unmatched", f"/api/vendor-responses/{Z}", f"/api/vendors/{Z}/vendor-requests",
        "/api/supply-listings", f"/api/supply-listings/{Z}", f"/api/supply-listings/{Z}/media/{Z}",
        f"/api/vendors/{Z}/supply-listings", f"/api/requests/{Z}/offers", f"/api/requests/{Z}/offers/preview",
        f"/api/requests/{Z}/selection", "/api/metrics/funnel", "/api/discovery/settings", "/api/discovery/candidates",
        "/api/discovery/preferences", "/api/discovery/batches", f"/api/discovery/batches/{Z}", f"/api/discovery/deliveries/{Z}",
        "/api/discovery/funnel", "/api/autonomy/settings", "/api/autonomy/metrics", "/api/autonomy/exceptions",
        f"/api/requests/{Z}/autonomy", f"/api/transactions/{Z}", "/api/events", "/api/events/types", "/api/jobs",
        "/api/jobs/summary"]

print("1. Every API read is behind the guard")
no_tok = [p for p in GETS if raw("GET", p)[0] != 401]
ok(not no_tok, f"all {len(GETS)} GET /api routes answer 401 without a token", ", ".join(no_tok))
bad_tok = [p for p in GETS if raw("GET", p, {"Authorization": "Bearer wrong-token-" + RUN})[0] != 401]
ok(not bad_tok, "a wrong token gets 401 everywhere", ", ".join(bad_tok))
shared = [p for p in GETS if raw("GET", p, {"Authorization": "Bearer " + os.environ["OPERATOR_TOKEN"]})[0] in (401, 403)]
ok(not shared, "the shared token reads every route", ", ".join(shared))
ok(raw("GET", "/health")[0] == 200 and raw("GET", "/ready")[0] == 200, "/health and /ready stay public")
code, hdr, _ = raw("GET", "/api/requests")
ok(code == 401 and "Bearer" in hdr.get("WWW-Authenticate", ""), "401 carries WWW-Authenticate: Bearer")

print("2. Named operators")
rc, out = opctl("create", "-name", f"Viewer {RUN}", "-role", "VIEWER")
ok(rc == 0 and "majo_op_" in out, "operatorctl creates a VIEWER and prints its token once", out.strip()[:80])
viewer = out.split("token (shown once; give it to the operator, then discard): ")[1].strip()
rc, out = opctl("create", "-name", f"Ada {RUN}", "-role", "OPERATOR")
ok(rc == 0, "operatorctl creates an OPERATOR")
ada = out.split("token (shown once; give it to the operator, then discard): ")[1].strip()
rc, out = opctl("create", "-name", f"Ada {RUN}", "-role", "VIEWER")
ok(rc != 0, "duplicate operator names are refused")
ok(sql(f"SELECT count(*) FROM operators WHERE token_hash = sha256(convert_to('{ada}', 'UTF8'))") == "1",
   "only the token's SHA-256 is stored")

V = {"Authorization": "Bearer " + viewer}
A = {"Authorization": "Bearer " + ada, "Content-Type": "application/json"}
ok(raw("GET", "/api/requests", V)[0] == 200 and raw("GET", "/api/autonomy/metrics", V)[0] == 200, "VIEWER reads")
code, _, body = raw("POST", "/api/vendors", dict(V, **{"Content-Type": "application/json"}),
                    {"business_name": "x", "whatsapp_external_id": "2348000000001", "phone": "2348000000001", "categories": ["phones"]})
ok(code == 403 and json.loads(body).get("code") == "FORBIDDEN", "VIEWER cannot change anything (403 FORBIDDEN)", str(code))
ok(raw("POST", f"/api/requests/{Z}/pause", dict(V, **{"Content-Type": "application/json"}), {"reason": "x"})[0] == 403,
   "VIEWER cannot pause a request")

phone = "23481" + RUN + "77"
code, _, body = raw("POST", "/api/vendors", A, {"business_name": f"Shop {RUN}", "whatsapp_external_id": phone, "phone": phone, "primary_location": "Lagos", "categories": ["phones"]})
ok(code == 201, "OPERATOR creates a vendor", str(code))
vid = json.loads(body)["id"]
row = sql(f"SELECT e.actor_type || '|' || coalesce(o.name, '') FROM events e LEFT JOIN operators o ON o.id = e.actor_id "
          f"WHERE e.entity_id = '{vid}' AND e.event_type = 'VENDOR_CREATED'")
ok(row == f"OPERATOR|Ada {RUN}", "the event names the operator who acted", row)

code, _, body = raw("GET", f"/api/events?entity_id={vid}&limit=5", V)
ev = json.loads(body)["events"] if code == 200 else []
ok(code == 200 and ev and ev[0]["actor_name"] == f"Ada {RUN}", "the timeline API shows the operator's name")

rc, _ = opctl("revoke", "-name", f"Viewer {RUN}")
ok(rc == 0 and raw("GET", "/api/requests", V)[0] == 401, "a revoked token stops working on the next call")
rc, out = opctl("list")
ok(rc == 0 and "revoked" in out and f"Ada {RUN}" in out, "operatorctl list shows active and revoked operators")

print("3. CORS allow-list")
code, hdr, _ = raw("GET", "/health", {"Origin": DASH_ORIGIN})
ok(hdr.get("Access-Control-Allow-Origin") == DASH_ORIGIN, "the dashboard origin is granted", str(hdr.get("Access-Control-Allow-Origin")))
code, hdr, _ = raw("GET", "/health", {"Origin": "https://evil.example"})
ok("Access-Control-Allow-Origin" not in hdr, "another website gets no CORS grant")
code, hdr, _ = raw("OPTIONS", "/api/requests", {"Origin": "https://evil.example", "Access-Control-Request-Method": "GET"})
ok(code == 403, "a preflight from another website is refused", str(code))
code, hdr, _ = raw("OPTIONS", "/api/requests", {"Origin": DASH_ORIGIN, "Access-Control-Request-Method": "GET",
                                                 "Access-Control-Request-Headers": "authorization"})
ok(code == 204 and "Authorization" in hdr.get("Access-Control-Allow-Headers", ""), "the dashboard preflight is allowed")

print("4. Timeline, jobs and paginated requests")
for i in range(3):
    say("23490" + RUN + str(i), f"I need a fridge in Lagos, budget 300k ({i})")
wait(lambda: int(sql("SELECT count(*) FROM buyer_requests")) >= 3, 60)
code, hdr, body = raw("GET", "/api/requests?limit=2&offset=0", A)
page1 = json.loads(body)
ok(code == 200 and len(page1) == 2 and int(hdr.get("X-Total-Count", "0")) >= 3, "requests page 1 (limit=2) with X-Total-Count",
   f"{len(page1)} / {hdr.get('X-Total-Count')}")
code, hdr, body = raw("GET", "/api/requests?limit=2&offset=2", A)
page2 = json.loads(body)
ok(code == 200 and page2 and not {r["id"] for r in page1} & {r["id"] for r in page2}, "page 2 continues without overlap")
code, hdr, body = raw("GET", "/api/requests", A)
ok(code == 200 and isinstance(json.loads(body), list) and len(json.loads(body)) >= 3, "unpaged GET /api/requests keeps its array shape")
ok(raw("GET", "/api/requests?limit=0", A)[0] == 400 and raw("GET", "/api/events?limit=500", A)[0] == 400, "bad paging is a 400")

code, hdr, body = raw("GET", "/api/events?limit=3", A)
b = json.loads(body)
ok(code == 200 and len(b["events"]) == 3 and b["total"] >= 3 and hdr.get("X-Total-Count") == str(b["total"]), "event timeline pages")
times = [e["created_at"] for e in b["events"]]
ok(times == sorted(times, reverse=True), "events are newest first")
code, _, body = raw("GET", "/api/events/types", A)
types = json.loads(body)
ok(code == 200 and "VENDOR_CREATED" in types, "event types list", ",".join(types[:6]))
code, _, body = raw("GET", "/api/events?event_type=VENDOR_CREATED&limit=50", A)
ok(code == 200 and all(e["event_type"] == "VENDOR_CREATED" for e in json.loads(body)["events"]), "event_type filter")
ok(raw("GET", "/api/events?entity_id=not-a-uuid", A)[0] == 400, "a malformed entity_id is a 400")

code, _, body = raw("GET", "/api/jobs/summary", A)
summary = json.loads(body)
ok(code == 200 and isinstance(summary, list), "job summary", json.dumps(summary)[:160])
code, hdr, body = raw("GET", "/api/jobs?limit=5", A)
jb = json.loads(body)
ok(code == 200 and "jobs" in jb and hdr.get("X-Total-Count") == str(jb["total"]), "job list pages", f"total={jb.get('total')}")
ok(all("payload" not in j for j in jb["jobs"]), "job payloads are not exposed")
ok(raw("GET", "/api/jobs?status=BOGUS", A)[0] == 400, "an unknown job status is a 400")

done()
