#!/usr/bin/env python3
"""
Phase 4A.1 end-to-end check: durable inbound media ingestion, driven through
the real backend binary: Meta-format webhooks signed with
X-Hub-Signature-256 -> MediaRef -> MEDIA_DOWNLOAD job (Postgres) ->
in-process worker -> Graph media download (scripts/e2e/fake_graph_api.py
with MEDIA_DIR) -> validation -> SHA-256 -> local storage provider ->
media_assets -> MEDIA_ASSET_STORED.

This is a test-equivalent run, NOT a live WhatsApp run: Meta is faked.

Two phases, so the backend can be killed (kill -9) and restarted between
them while a job is waiting for its retry:
  media_ingestion.py send     # posts webhooks, checks durable jobs exist
  (kill -9 the backend, restart it)
  media_ingestion.py verify   # waits for the worker, checks every outcome

Env: BASE (backend, default http://localhost:8080), APP_SECRET (backend's
WHATSAPP_APP_SECRET), DATABASE_URL (psql is used for read-only checks),
MEDIA_DIR (shared with fake_graph_api.py), STORAGE_DIR (backend's
MEDIA_STORAGE_DIR), STATE (state file, default /tmp/majo_media_e2e.json).
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
STORAGE_DIR = os.environ["STORAGE_DIR"]
STATE = os.environ.get("STATE", "/tmp/majo_media_e2e.json")

checks = 0


def ok(cond, label, detail=""):
    global checks
    checks += 1
    print(f"  [{'PASS' if cond else 'FAIL'}] {label}" + (f" -> {detail}" if detail else ""))
    if not cond:
        sys.exit(1)


def sql(q):
    out = subprocess.run(["psql", DB, "-At", "-c", q], capture_output=True, text=True, check=True).stdout.strip()
    return out


def api(method, path, body=None):
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None,
                                 method=method, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req) as r:
        return json.loads(r.read() or b"null")


def media_webhook(frm, wamid, kind, media_id, mime, caption="", sign=True):
    body = {"id": media_id, "mime_type": mime}
    if caption:
        body["caption"] = caption
    if kind == "audio":
        body["voice"] = True
    payload = json.dumps({"object": "whatsapp_business_account", "entry": [{"changes": [{"field": "messages", "value": {
        "messaging_product": "whatsapp",
        "contacts": [{"wa_id": frm, "profile": {"name": "Vendor " + frm[-2:]}}],
        "messages": [{"from": frm, "id": wamid, "timestamp": str(int(time.time())), "type": kind, kind: body}],
    }}]}]}).encode()
    headers = {"Content-Type": "application/json"}
    if sign:
        headers["X-Hub-Signature-256"] = "sha256=" + hmac.new(SECRET.encode(), payload, hashlib.sha256).hexdigest()
    try:
        with urllib.request.urlopen(urllib.request.Request(BASE + "/webhooks/whatsapp", data=payload, method="POST", headers=headers)) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def put_media(media_id, data, mime="image/jpeg", fail=""):
    open(os.path.join(MEDIA_DIR, media_id + ".bin"), "wb").write(data)
    open(os.path.join(MEDIA_DIR, media_id + ".mime"), "w").write(mime)
    if fail:
        open(os.path.join(MEDIA_DIR, media_id + ".fail"), "w").write(fail)


def jpeg(tag):
    return b"\xff\xd8\xff\xe0" + tag.encode() + os.urandom(64)


def job(wamid):
    return sql(f"""SELECT j.status || '|' || j.attempts || '|' || COALESCE(j.last_error, '')
                   FROM jobs j JOIN messages m ON j.idempotency_key = m.id::text
                   WHERE m.external_message_id = '{wamid}'""")


def asset(wamid):
    row = sql(f"""SELECT a.id, a.owner_vendor_id, a.sha256, a.size_bytes, a.storage_key, a.provider_media_id,
                         a.source_message_id = m.id, a.mime_type, COALESCE(a.provider_media_expires_at::text, 'NULL')
                  FROM messages m JOIN media_assets a ON a.id = m.media_asset_id
                  WHERE m.external_message_id = '{wamid}'""")
    if not row:
        return None
    k = ["id", "vendor", "sha256", "size", "key", "provider_media_id", "is_source", "mime", "expires"]
    return dict(zip(k, row.split("|")))


def send():
    run = f"{random.randint(100000, 999999)}"
    A, B = "23470" + run + "31", "23470" + run + "32"
    print(f"Majo Phase 4A.1 media E2E · run {run} · phase SEND · backend {BASE}")
    va = api("POST", "/api/vendors", {"business_name": "Ada Bags " + run, "whatsapp_external_id": A, "primary_location": "Benin City"})
    api("PATCH", f"/api/vendors/{va['id']}", {"status": "ACTIVE"})
    vb = api("POST", "/api/vendors", {"business_name": "Bola Bags " + run, "whatsapp_external_id": B, "primary_location": "Lagos"})
    api("PATCH", f"/api/vendors/{vb['id']}", {"status": "ACTIVE"})

    p = {k: jpeg(k + run) for k in ("p1", "p2", "p3")}
    put_media(f"m{run}1", p["p1"])
    put_media(f"m{run}2", p["p2"])
    put_media(f"m{run}3", p["p3"])
    put_media(f"m{run}dup", p["p1"])                         # same vendor, same bytes, new media ID
    put_media(f"m{run}b", p["p1"])                           # vendor B, identical bytes
    put_media(f"m{run}retry", jpeg("retry" + run), fail="503,503,503")  # outlives a process restart
    put_media(f"m{run}gif", b"GIF89a" + os.urandom(32), mime="image/gif")
    # m{run}gone has no .bin: Meta answers 404 (media expired/unknown)

    print("\nWebhook authenticity")
    ok(media_webhook(A, f"wamid.{run}.forged", "image", f"m{run}1", "image/jpeg", sign=False) == 401, "unsigned media webhook rejected", "401")

    print("\nAcceptance: vendor sends three photos + caption")
    for i, cap in ((1, "Imported ladies bags ₦18,500"), (2, ""), (3, "")):
        ok(media_webhook(A, f"wamid.{run}.p{i}", "image", f"m{run}{i}", "image/jpeg", cap) == 200, f"photo {i} webhook accepted")
    ok(media_webhook(A, f"wamid.{run}.p1", "image", f"m{run}1", "image/jpeg", "Imported ladies bags ₦18,500") == 200, "photo 1 redelivered by Meta")
    ok(media_webhook(A, f"wamid.{run}.dup", "image", f"m{run}dup", "image/jpeg") == 200, "same photo re-sent by the same vendor")
    ok(media_webhook(B, f"wamid.{run}.b", "image", f"m{run}b", "image/jpeg") == 200, "identical photo sent by another vendor")
    ok(media_webhook(A, f"wamid.{run}.retry", "image", f"m{run}retry", "image/jpeg") == 200, "photo whose download will 503 three times")
    ok(media_webhook(A, f"wamid.{run}.gone", "image", f"m{run}gone", "image/jpeg") == 200, "photo whose media ID has expired")
    ok(media_webhook(A, f"wamid.{run}.gif", "image", f"m{run}gif", "image/jpeg") == 200, "unsupported (GIF) content")
    ok(media_webhook(A, f"wamid.{run}.voice", "audio", f"m{run}voice", "audio/ogg") == 200, "voice note (not ingested in 4A.1)")

    msgs = sql(f"SELECT count(*) FROM messages WHERE external_message_id LIKE 'wamid.{run}.%'")
    ok(msgs == "9", "one message per distinct wamid (redelivery deduped)", msgs)
    jobs = sql(f"""SELECT count(*) FROM jobs j JOIN messages m ON j.idempotency_key = m.id::text
                   WHERE m.external_message_id LIKE 'wamid.{run}.%'""")
    ok(jobs == "8", "8 durable MEDIA_DOWNLOAD jobs (no job for the voice note, none duplicated)", jobs)
    refs = sql(f"""SELECT count(*) FROM messages WHERE external_message_id LIKE 'wamid.{run}.%'
                   AND content->'media'->>'provider' = 'whatsapp' AND content->'media'->>'provider_ref' <> ''""")
    ok(refs == "9", "MediaRef persisted on every media message as before", refs)

    json.dump({"run": run, "A": va["id"], "B": vb["id"], "sha": {k: hashlib.sha256(v).hexdigest() for k, v in p.items()}}, open(STATE, "w"))
    print(f"\nSEND phase: {checks} checks passed")


def verify():
    st = json.load(open(STATE))
    run, A, B, sha = st["run"], st["A"], st["B"], st["sha"]
    print(f"Majo Phase 4A.1 media E2E · run {run} · phase VERIFY (after restart) · backend {BASE}")

    end = time.time() + 120
    while time.time() < end:
        pending = sql(f"""SELECT count(*) FROM jobs j JOIN messages m ON j.idempotency_key = m.id::text
                          WHERE m.external_message_id LIKE 'wamid.{run}.%' AND j.status IN ('PENDING','RUNNING')""")
        if pending == "0":
            break
        time.sleep(1)
    ok(pending == "0", "all jobs reached a terminal state", f"{pending} open")

    print("\nThree photos stored with provenance")
    for i in (1, 2, 3):
        a = asset(f"wamid.{run}.p{i}")
        ok(a is not None, f"photo {i}: MediaAsset linked to its message")
        ok(a["vendor"] == A and a["sha256"] == sha[f"p{i}"] and a["is_source"] == "t" and a["provider_media_id"] == f"m{run}{i}"
           and a["mime"] == "image/jpeg" and a["expires"] == "NULL",
           f"photo {i}: owner, SHA-256, source message, provider media ID, MIME; no invented expiry", a["sha256"][:16] + "…")
        data = open(os.path.join(STORAGE_DIR, a["key"]), "rb").read()
        ok(hashlib.sha256(data).hexdigest() == a["sha256"] and len(data) == int(a["size"]), f"photo {i}: stored bytes match", a["key"])
        ok(job(f"wamid.{run}.p{i}").startswith("SUCCEEDED|1|"), f"photo {i}: job SUCCEEDED on first attempt")

    print("\nDedupe and ownership")
    p1, dup, b = asset(f"wamid.{run}.p1"), asset(f"wamid.{run}.dup"), asset(f"wamid.{run}.b")
    ok(dup["id"] == p1["id"] and dup["is_source"] == "f", "same vendor, same bytes -> same asset; first message stays the source")
    ok(b["id"] != p1["id"] and b["vendor"] == B and b["key"] != p1["key"] and b["sha256"] == p1["sha256"],
       "same bytes from another vendor -> separate asset owned by that vendor")

    print("\nDurability across kill -9 + restart")
    ok(job(f"wamid.{run}.retry").startswith("SUCCEEDED|4|"), "job that 503'd 3x survived the restart and succeeded on attempt 4", job(f"wamid.{run}.retry"))
    ok(asset(f"wamid.{run}.retry") is not None, "its asset exists")

    print("\nPermanent failures are visible, never faked")
    g = job(f"wamid.{run}.gone")
    ok(g.startswith("FAILED|1|") and "404" in g, "expired media ID -> FAILED after 1 attempt with reason", g[:90])
    f = job(f"wamid.{run}.gif")
    ok(f.startswith("FAILED|1|") and "unsupported" in f, "GIF -> FAILED, unsupported content type", f[:90])
    ok(asset(f"wamid.{run}.gone") is None and asset(f"wamid.{run}.gif") is None, "no MediaAsset for failed downloads")
    failed_events = sql(f"""SELECT count(*) FROM events e JOIN messages m ON e.entity_id = m.id
                            WHERE e.event_type = 'MEDIA_INGESTION_FAILED' AND m.external_message_id LIKE 'wamid.{run}.%'""")
    ok(failed_events == "2", "MEDIA_INGESTION_FAILED recorded for each terminal failure", failed_events)

    print("\nEvents and scope")
    stored = sql(f"""SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED' AND payload->>'vendor_id' IN ('{A}', '{B}')""")
    ok(stored == "5", "one MEDIA_ASSET_STORED per stored asset (3 photos + vendor B + retried photo)", stored)
    assets = sql(f"SELECT count(*) FROM media_assets WHERE owner_vendor_id IN ('{A}', '{B}')")
    ok(assets == "5", "5 logical assets in total", assets)
    files = sum(len(fs) for _, _, fs in os.walk(os.path.join(STORAGE_DIR, "vendors", A))) + \
        sum(len(fs) for _, _, fs in os.walk(os.path.join(STORAGE_DIR, "vendors", B)))
    ok(files == 5, "5 stored objects, no orphans or temp files", str(files))
    voice = sql(f"SELECT count(*) FROM messages WHERE external_message_id = 'wamid.{run}.voice' AND media_asset_id IS NULL")
    ok(voice == "1", "voice note kept as MediaRef only")
    listings = sql(f"SELECT count(*) FROM supply_listings WHERE vendor_id IN ('{A}', '{B}') AND status <> 'DRAFT'")
    ok(listings == "0", "no listing beyond DRAFT (nothing published)", listings)
    print(f"\nVERIFY phase: {checks} checks passed")


if __name__ == "__main__":
    {"send": send, "verify": verify}[sys.argv[1]]()
