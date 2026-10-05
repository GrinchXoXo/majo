#!/usr/bin/env python3
"""
Stand-in for the Meta Graph API messages endpoint, for local end-to-end runs
without a real WhatsApp number.

Point the backend at it with WHATSAPP_API_URL=http://127.0.0.1:9090 . Every
outbound message the backend sends is appended to OUTBOUND_LOG (JSON lines).

Recipients listed in FAIL_RECIPIENTS (comma-separated wa_ids) get the same
HTTP 400 / error 131047 ("Re-engagement message") that Meta returns when the
24-hour customer-service window has closed and a template is required.
"""
import json
import os
import http.server

LOG = os.environ.get("OUTBOUND_LOG", "outbound.jsonl")
FAIL = {r.strip() for r in os.environ.get("FAIL_RECIPIENTS", "").split(",") if r.strip()}
PORT = int(os.environ.get("PORT", "9090"))


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("content-length", 0))))
        to = body.get("to", "")
        failed = to in FAIL
        with open(LOG, "a") as f:
            f.write(json.dumps({"path": self.path, "to": to, "failed": failed, "body": body}) + "\n")
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
        self.wfile.write(b'{"messaging_product":"whatsapp","messages":[{"id":"wamid.fake"}]}')

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    http.server.HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
