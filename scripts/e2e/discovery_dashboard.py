#!/usr/bin/env python3
"""
Phase 4B dashboard check (headless Chromium): the operator console drives
the whole internal flow — staff consent record, dry run, batch, send, and the
delivery attribution view — with no consumer surface.
Env: as discovery_common + DASH (dashboard URL), SHOT (screenshot path).
"""
import os
import time

from discovery_common import *  # noqa: F401,F403
from playwright.sync_api import sync_playwright

DASH = os.environ.get("DASH", "http://localhost:3000")
STAFF = "2347300000001"
print(f"Majo Phase 4B dashboard E2E · run {RUN} · dashboard {DASH}")

VP = "23473" + RUN + "55"
vid = vendor("Dash Stores " + RUN, VP, "GRA, Benin City", ["electronics"])
L = published_listing(vid, VP, "z", "Samsung TV 55 inch ₦450,000")
ref = L["public_reference"]
ok(say(STAFF, "hello majo") == 200, "staff messages Majo")
ok(wait(lambda: sql(f"SELECT count(*) FROM buyers WHERE external_user_id = '{STAFF}'") == "1"), "buyer exists")
time.sleep(1)

with sync_playwright() as pw:
    b = pw.chromium.launch()
    pg = b.new_page(viewport={"width": 1300, "height": 1500})
    pg.goto(f"{DASH}/discovery")
    pg.get_by_label("Operator token").fill(TOKEN)
    pg.get_by_role("button", name="Use token").click()
    pg.wait_for_selector("[data-testid=discovery-settings]")
    ok("INTERNAL" in pg.locator("[data-testid=discovery-mode]").inner_text(), "dashboard: mode INTERNAL shown")
    pg.get_by_label("Staff number").fill(STAFF)
    pg.get_by_label("Viewer location").fill("GRA, Benin City")
    pg.get_by_label("Interests").fill("electronics")
    pg.get_by_role("button", name="Save staff test record").click()
    pg.wait_for_selector(f"[data-testid=preferences] >> text={STAFF}")
    ok(True, "dashboard: staff consent record saved and listed")
    pg.get_by_label("Scope category").fill("electronics")
    pg.get_by_label("Audience numbers").fill(STAFF)
    pg.click("[data-testid=dry-run]")
    pg.wait_for_selector("[data-testid=plan-report]")
    ok(pg.locator("[data-testid=plan-deliveries]").inner_text() == "1", "dashboard: dry run plans 1 delivery")
    ok(ref in pg.locator("[data-testid=plan-report]").inner_text(), "dashboard: dry run shows the listing with policy result and reasons")
    ok(sql("SELECT count(*) FROM discovery_batches") == "0", "dashboard: dry run persisted nothing")
    pg.click("[data-testid=create-batch]")
    pg.wait_for_selector("[data-testid=batch-status]")
    ok(pg.locator("[data-testid=batch-status]").inner_text() == "PLANNED", "dashboard: batch PLANNED (nothing sent)")
    ok(sql("SELECT count(*) FROM discovery_deliveries WHERE status = 'SENT'") == "0", "dashboard: still nothing sent")
    n0 = len(outbound(STAFF))
    pg.once("dialog", lambda d: d.accept())  # the operator confirms the send
    pg.click("[data-testid=send-batch]")
    pg.wait_for_selector("[data-testid=batch-notice]")
    ok(len(outbound(STAFF)) > n0, "dashboard: operator-triggered send reached the staff phone (fake Graph)")
    ok(pg.locator("[data-testid=batch-status]").inner_text() == "SENT", "dashboard: batch SENT")
    pg.locator(f"[data-testid=delivery-{STAFF}]").get_by_role("button", name="Show").click()
    pg.wait_for_selector("[data-testid=attribution]")
    ok(True, "dashboard: delivery attribution view opens")
    pg.screenshot(path=os.environ.get("SHOT", "/tmp/discovery-batch.png"), full_page=True)
    b.close()
done()
