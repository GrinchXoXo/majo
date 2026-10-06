"""
Shared helpers for the Phase 4B Discovery & Distribution E2Es.

Real backend binary, real PostgreSQL, real Python extraction service; the
WhatsApp side is scripts/e2e/fake_graph_api.py (outbound log + media upload).
Test-equivalent run, NOT live WhatsApp, and NEVER a real customer: the
backend runs with DISCOVERY_MODE=INTERNAL and a staff-only allowlist.

Env: BASE, APP_SECRET, DATABASE_URL (psql), MEDIA_DIR, OPERATOR_TOKEN,
OUTBOUND_LOG, UPLOAD_LOG.
"""
import hashlib
import hmac
import json
import os
import random
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.request
import zlib

BASE = os.environ.get("BASE", "http://localhost:8080")
SECRET = os.environ.get("APP_SECRET", "")
DB = os.environ["DATABASE_URL"]
MEDIA_DIR = os.environ["MEDIA_DIR"]
TOKEN = os.environ["OPERATOR_TOKEN"]
OUTBOUND_LOG = os.environ["OUTBOUND_LOG"]
UPLOAD_LOG = os.environ.get("UPLOAD_LOG", "")
RUN = f"{random.randint(100000, 999999)}"
checks = 0


def ok(cond, label, detail=""):
    global checks
    checks += 1
    print(f"  [{'PASS' if cond else 'FAIL'}] {label}" + (f" -> {detail}" if detail else ""))
    if not cond:
        sys.exit(1)


def done():
    print(f"\nALL {checks} CHECKS PASSED")


def sql(q):
    return subprocess.run(["psql", DB, "-At", "-c", q], capture_output=True, text=True, check=True).stdout.strip()


def api(method, path, body=None, token=TOKEN):
    h = {"Content-Type": "application/json"}
    if token:
        h["Authorization"] = "Bearer " + token
    req = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None, method=method, headers=h)
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        raw = e.read() or b"null"
        try:
            return e.code, json.loads(raw)
        except ValueError:
            return e.code, raw.decode()


def wait(cond, timeout=30.0):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.25)
    return cond()


def png(rgb):
    w = h = 16
    raw = b"".join(b"\x00" + bytes(rgb) * w for _ in range(h))

    def chunk(t, d):
        return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xFFFFFFFF)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def _post_webhook(value):
    payload = json.dumps({"object": "whatsapp_business_account", "entry": [{"changes": [{"field": "messages", "value": value}]}]}).encode()
    sig = "sha256=" + hmac.new(SECRET.encode(), payload, hashlib.sha256).hexdigest()
    req = urllib.request.Request(BASE + "/webhooks/whatsapp", data=payload, method="POST",
                                 headers={"Content-Type": "application/json", "X-Hub-Signature-256": sig})
    try:
        with urllib.request.urlopen(req) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def webhook(frm, wamid, kind, body="", media_id="", interactive=None, button=None):
    """A signed Meta-format inbound: text, image, interactive reply or quick-reply button."""
    msg = {"from": frm, "id": wamid, "timestamp": str(int(time.time())), "type": kind}
    if kind == "text":
        msg["text"] = {"body": body}
    elif kind == "image":
        msg["image"] = {"id": media_id, "mime_type": "image/png"}
    elif kind == "interactive":
        msg["interactive"] = interactive
    elif kind == "button":
        msg["button"] = button
    return _post_webhook({"messaging_product": "whatsapp", "contacts": [{"wa_id": frm, "profile": {"name": "U"}}], "messages": [msg]})


_n = [0]


def _wamid(tag):
    _n[0] += 1
    return f"wamid.{RUN}.{tag}{_n[0]}"


def say(frm, text):
    return webhook(frm, _wamid("t"), "text", text)


def tap_button(frm, rid, title="tap"):
    return webhook(frm, _wamid("b"), "interactive", interactive={"type": "button_reply", "button_reply": {"id": rid, "title": title}})


def tap_list(frm, rid, title="row"):
    return webhook(frm, _wamid("l"), "interactive", interactive={"type": "list_reply", "list_reply": {"id": rid, "title": title}})


def tap_quick_reply(frm, payload, text="Show me"):
    return webhook(frm, _wamid("q"), "button", button={"payload": payload, "text": text})


def outbound(to=None):
    rows = []
    if os.path.exists(OUTBOUND_LOG):
        for line in open(OUTBOUND_LOG):
            r = json.loads(line)
            if to is None or r["to"] == to:
                rows.append(r)
    return rows


def uploads():
    if not UPLOAD_LOG or not os.path.exists(UPLOAD_LOG):
        return []
    return [json.loads(l) for l in open(UPLOAD_LOG)]


def interactive_ids(msg):
    """Every structured id offered by an outbound message (buttons, list rows, template quick replies)."""
    b = msg["body"]
    ids = []
    inter = b.get("interactive")
    if inter:
        if inter["type"] == "button":
            ids += [x["reply"]["id"] for x in inter["action"]["buttons"]]
        elif inter["type"] == "list":
            ids += [r["id"] for s in inter["action"]["sections"] for r in s["rows"]]
    tpl = b.get("template")
    if tpl:
        for c in tpl.get("components", []):
            if c["type"] == "button":
                ids += [p["payload"] for p in c["parameters"]]
    return ids


def vendor(name, phone, location, categories):
    _, v = api("POST", "/api/vendors", {"business_name": name, "whatsapp_external_id": phone,
                                         "primary_location": location, "categories": categories})
    api("PATCH", f"/api/vendors/{v['id']}", {"status": "ACTIVE"})
    return v["id"]


def post_listing(phone, tag, caption, shade=40, photos=2):
    for i in range(1, photos + 1):
        mid = f"d{RUN}{tag}{i}"
        open(os.path.join(MEDIA_DIR, mid + ".bin"), "wb").write(png((shade + i * 30, shade + len(tag), (int(RUN) + i) % 200)))
        open(os.path.join(MEDIA_DIR, mid + ".mime"), "w").write("image/png")
        ok(webhook(phone, f"wamid.{RUN}.{tag}p{i}", "image", media_id=mid) == 200, f"photo {i} accepted ({tag})")
        time.sleep(0.3)
    ok(webhook(phone, f"wamid.{RUN}.{tag}c", "text", caption) == 200, f"caption accepted ({tag})")


def published_listing(vid, phone, tag, caption, shade=40):
    """Vendor posts photos + caption -> DRAFT -> operator publishes -> ACTIVE listing dict."""
    post_listing(phone, tag, caption, shade)
    ls = wait(lambda: (api("GET", f"/api/vendors/{vid}/supply-listings")[1] or []))
    ok(ls and ls[0]["status"] == "DRAFT", f"draft created ({tag})", ls[0]["public_reference"] if ls else "none")
    code, p = api("POST", f"/api/supply-listings/{ls[0]['id']}/publish", {})
    ok(code == 200 and p["status"] == "ACTIVE", f"published ({tag})", p["public_reference"])
    return p


def travel(lid, vid, days):
    """Time travel: move the listing and the vendor's signals `days` into the past."""
    iv = f"{days} days"
    sql(f"""UPDATE supply_listings SET published_at = published_at - interval '{iv}', expires_at = expires_at - interval '{iv}',
            last_verified_at = last_verified_at - interval '{iv}', created_at = created_at - interval '{iv}' WHERE id = '{lid}'""")
    sql(f"""UPDATE supply_signals SET created_at = created_at - interval '{iv}', expires_at = expires_at - interval '{iv}'
            WHERE vendor_id = '{vid}'""")


def opt_in(staff, location, interests):
    code, p = api("POST", "/api/discovery/preferences", {"recipient": staff, "status": "OPTED_IN", "location": location,
                                                          "interests": interests, "note": "e2e staff"})
    return code, p


def preview(recipients, category="", limit=3):
    return api("POST", "/api/discovery/preview", {"scope": {"category": category, "limit": limit}, "recipients": recipients})


def batch(key, recipients, category="", limit=3):
    return api("POST", "/api/discovery/batches", {"client_key": key, "scope": {"category": category, "limit": limit}, "recipients": recipients})
