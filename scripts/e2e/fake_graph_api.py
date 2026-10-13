#!/usr/bin/env python3
"""
Stand-in for the Meta Graph API messages endpoint, for local end-to-end runs
without a real WhatsApp number.

Point the backend at it with WHATSAPP_API_URL=http://127.0.0.1:9090 . Every
outbound message the backend sends is appended to OUTBOUND_LOG (JSON lines),
and each accepted message gets a unique wamid (like Meta's), so delivery
status callbacks can be simulated against it.

Recipients listed in FAIL_RECIPIENTS (comma-separated wa_ids) get the same
HTTP 400 / error 131047 ("Re-engagement message") that Meta returns when the
24-hour customer-service window has closed and a template is required.

Phase 4A.1 media endpoints (inbound media download), when MEDIA_DIR is set:
  GET /{media-id}           -> {"url": ".../download/{media-id}", "mime_type", "file_size", ...}
  GET /download/{media-id}  -> bytes of MEDIA_DIR/{media-id}.bin
A missing .bin is a 404 (expired/unknown media ID). MEDIA_DIR/{media-id}.fail
holds comma-separated HTTP statuses returned (one per call) before success,
e.g. "503,503". MEDIA_DIR/{media-id}.mime overrides the claimed MIME type.
If ACCESS_TOKEN is set, a matching Bearer token is required (401 otherwise).
"""
import json
import os
import uuid
import http.server

LOG = os.environ.get("OUTBOUND_LOG", "outbound.jsonl")
FAIL = {r.strip() for r in os.environ.get("FAIL_RECIPIENTS", "").split(",") if r.strip()}
PORT = int(os.environ.get("PORT", "9090"))
MEDIA_DIR = os.environ.get("MEDIA_DIR", "")
TOKEN = os.environ.get("ACCESS_TOKEN", "")


UPLOAD_LOG = os.environ.get("UPLOAD_LOG", "uploads.jsonl")
# Phase 4C correction: scripted transient failures. FAIL_PLAN is a JSON file
# {"<recipient>": [503, 503]}: each send to that recipient pops one status
# and fails with it (an empty list = succeed). Used to prove automatic retry.
FAIL_PLAN = os.environ.get("FAIL_PLAN", "")


def planned_failure(to):
    if not FAIL_PLAN or not os.path.exists(FAIL_PLAN):
        return None
    try:
        plan = json.load(open(FAIL_PLAN))
    except ValueError:
        return None
    codes = plan.get(to) or []
    if not codes:
        return None
    code = codes.pop(0)
    plan[to] = codes
    json.dump(plan, open(FAIL_PLAN, "w"))
    return int(code)
FAIL_UPLOADS = os.environ.get("FAIL_UPLOADS", "") == "1"


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length", 0)))
        # Phase 4B: POST /{phone}/media (multipart) uploads outbound media and
        # returns a provider media id, which Majo caches and reuses.
        if self.path.split("?")[0].rstrip("/").endswith("/media"):
            if TOKEN and self.headers.get("authorization") != "Bearer " + TOKEN:
                return self._json(401, {"error": {"message": "Invalid OAuth access token", "code": 190}})
            if FAIL_UPLOADS:
                return self._json(500, {"error": {"message": "scripted upload failure", "code": 1}})
            media_id = "up." + uuid.uuid4().hex[:16]
            with open(UPLOAD_LOG, "a") as f:
                f.write(json.dumps({"path": self.path, "media_id": media_id, "bytes": len(raw),
                                    "content_type": self.headers.get("content-type", "")}) + "\n")
            return self._json(200, {"id": media_id})
        body = json.loads(raw)
        to = body.get("to", "")
        planned = planned_failure(to)
        if planned:
            with open(LOG, "a") as f:
                f.write(json.dumps({"path": self.path, "to": to, "failed": True, "planned": planned, "wamid": "", "body": body}) + "\n")
            return self._json(planned, {"error": {"message": "scripted transient failure", "code": 1}})
        failed = to in FAIL
        wamid = "" if failed else "wamid.fake." + uuid.uuid4().hex
        with open(LOG, "a") as f:
            f.write(json.dumps({"path": self.path, "to": to, "failed": failed, "wamid": wamid, "body": body}) + "\n")
        if failed:
            self.send_response(400)
            self.send_header("content-type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"error": {
                "message": "(#131047) Re-engagement message", "type": "OAuthException", "code": 131047,
                "error_data": {"details": "Message failed to send because more than 24 hours have passed since the customer last replied to this number."},
            }}).encode())
            return
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"messaging_product": "whatsapp", "messages": [{"id": wamid}]}).encode())

    def _json(self, status, obj):
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(obj).encode())

    def do_GET(self):
        if not MEDIA_DIR:
            return self._json(404, {"error": {"message": "media endpoints disabled", "code": 100}})
        if TOKEN and self.headers.get("authorization") != "Bearer " + TOKEN:
            return self._json(401, {"error": {"message": "Invalid OAuth access token", "code": 190}})
        path = self.path.split("?")[0].strip("/")
        download = path.startswith("download/")
        media_id = path[len("download/"):] if download else path
        if "/" in media_id or media_id in ("", ".", ".."):
            return self._json(400, {"error": {"message": "bad id", "code": 100}})
        base = os.path.join(MEDIA_DIR, media_id)
        fail = base + ".fail"
        if os.path.exists(fail):
            codes = [c for c in open(fail).read().split(",") if c.strip()]
            if codes:
                with open(fail, "w") as f:
                    f.write(",".join(codes[1:]))
                return self._json(int(codes[0]), {"error": {"message": "scripted failure", "code": 0}})
        if not os.path.exists(base + ".bin"):
            return self._json(404, {"error": {"message": f"Object with ID '{media_id}' does not exist", "code": 100}})
        data = open(base + ".bin", "rb").read()
        mime = open(base + ".mime").read().strip() if os.path.exists(base + ".mime") else "image/jpeg"
        if download:
            self.send_response(200)
            self.send_header("content-type", mime)
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        host = self.headers.get("host", f"127.0.0.1:{PORT}")
        return self._json(200, {"messaging_product": "whatsapp", "id": media_id, "mime_type": mime,
                                "file_size": len(data), "url": f"http://{host}/download/{media_id}?sig=short-lived"})

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    http.server.HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
