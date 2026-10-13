"""Reuse-audit R1 pilot: AI-service error reports reach a Sentry-protocol
endpoint (what GlitchTip exposes) and leave no buyer data behind."""
import gzip
import http.server
import logging
import threading

import sentry_sdk
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import errortrack


def test_scrub():
    assert errortrack.scrub("buyer 2348012345678 wrote") == "buyer [number] wrote"
    assert errortrack.scrub("call +234 801 234 5678") == "call [number]"
    assert errortrack.scrub("Bearer abc.def-1") == "Bearer [redacted]"
    assert errortrack.scrub("ada@example.com") == "[email]"
    assert errortrack.scrub("code 131047") == "code 131047"
    uid = "9f1c2a3b-1111-2222-3333-444455556666"
    assert errortrack.scrub(f"request {uid}") == f"request {uid}"


def test_disabled_without_dsn(monkeypatch):
    monkeypatch.delenv("SENTRY_DSN", raising=False)
    assert errortrack.init_error_tracking() is False


class _Ingest(http.server.BaseHTTPRequestHandler):
    bodies = []

    def do_POST(self):  # noqa: N802
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.headers.get("Content-Encoding") == "gzip":
            raw = gzip.decompress(raw)
        _Ingest.bodies.append(self.path + "\n" + raw.decode("utf-8", "replace"))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *a):
        pass


def test_reports_sent_and_scrubbed(monkeypatch):
    srv = http.server.HTTPServer(("127.0.0.1", 0), _Ingest)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        monkeypatch.setenv("SENTRY_DSN", f"http://publickey@127.0.0.1:{srv.server_port}/7")
        monkeypatch.setenv("SENTRY_ENVIRONMENT", "test")
        assert errortrack.init_error_tracking() is True

        app = FastAPI()

        @app.post("/extract")
        def extract(body: dict):
            logging.getLogger("majo-ai").error("extraction failed for 2348012345678")
            raise RuntimeError("boom while reading 'I need a fridge, call 08012345678'")

        client = TestClient(app, raise_server_exceptions=False)
        r = client.post("/extract", json={"text": "I need a fridge in Lagos, my number is 08012345678"})
        assert r.status_code == 500
        sentry_sdk.flush(5)
        got = "\n---\n".join(_Ingest.bodies)
        assert got.count("/api/7/envelope/") >= 2, got[:500]
        assert "extraction failed for [number]" in got
        assert "RuntimeError" in got and "call [number]" in got
        assert '"environment":"test"' in got.replace(" ", "")
        for leak in ("2348012345678", "08012345678", "I need a fridge in Lagos"):
            assert leak not in got, leak
    finally:
        sentry_sdk.get_client().close()
        srv.shutdown()
