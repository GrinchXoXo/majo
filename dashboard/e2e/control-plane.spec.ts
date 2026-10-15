import { expect, test, type Page } from "@playwright/test";

// Smoke tests for the operator control plane: the token gate, the paginated
// requests list, the event timeline, the jobs view, and pause/resume on a
// request. They guard the dashboard through framework upgrades.

const TOKEN = process.env.E2E_OPERATOR_TOKEN ?? "";

async function signIn(page: Page) {
  await page.goto("/requests");
  await page.getByLabel("Operator token").fill(TOKEN);
  await Promise.all([page.waitForEvent("load"), page.getByRole("button", { name: "Use token" }).click()]);
  await expect(page.getByText("Operator token set")).toBeVisible();
}

test.beforeAll(() => {
  if (!TOKEN) throw new Error("set E2E_OPERATOR_TOKEN");
});

test("without a token the API refuses and the page says how to fix it", async ({ page }) => {
  await page.goto("/requests");
  await expect(page.getByText(/Set your operator token/)).toBeVisible();
  await expect(page.getByRole("table", { name: "Buyer requests" })).toHaveCount(0);
});

test("requests list is paginated and links to the request", async ({ page }) => {
  await signIn(page);
  const table = page.getByRole("table", { name: "Buyer requests" });
  await expect(table).toBeVisible();
  await expect(page.getByText(/^1–\d+ of \d+$/)).toBeVisible();
  expect(await table.locator("tbody tr").count()).toBeGreaterThan(0);
  // Sorting is per page and does not lose rows.
  const before = await table.locator("tbody tr").count();
  await table.getByRole("button", { name: /Created/ }).click();
  await expect(table.locator("tbody tr")).toHaveCount(before);
});

test("event timeline shows events and filters by actor", async ({ page }) => {
  await signIn(page);
  await page.goto("/events");
  const table = page.getByRole("table", { name: "Event timeline" });
  await expect(table).toBeVisible();
  await page.getByLabel("Actor type").selectOption("SYSTEM");
  await expect(table.locator("tbody tr").first()).toBeVisible();
  const actors = await table.locator("tbody tr td:nth-child(4)").allTextContents();
  expect(actors.every((a) => a.startsWith("SYSTEM"))).toBe(true);
});

test("jobs page shows the per-type summary", async ({ page }) => {
  await signIn(page);
  await page.goto("/jobs");
  await expect(page.getByRole("table", { name: "Jobs by type" })).toBeVisible();
});

test("an operator can pause and resume a request", async ({ page }) => {
  await signIn(page);
  await page.getByRole("table", { name: "Buyer requests" }).locator("tbody tr a").first().click();
  await page.waitForURL(/\/requests\/[0-9a-f-]{36}$/);
  const reason = page.getByLabel("Pause or resume reason");
  await reason.fill("playwright smoke test");
  await page.getByRole("button", { name: "Pause automation" }).click();
  await expect(page.getByTestId("hold-status")).toContainText("PAUSED");
  await reason.fill("playwright smoke test done");
  await page.getByRole("button", { name: "Resume automation" }).click();
  await expect(page.getByTestId("hold-status")).toHaveCount(0);
});

// Pilot batch item 4: the global pause, its banner on every page, and resume.
test("an operator can pause and resume ALL automation", async ({ page }) => {
  await signIn(page);
  await page.goto("/autonomy");
  const panel = page.getByRole("region", { name: "Global pause" });
  await expect(panel.getByTestId("pause-state")).toContainText("Running");
  await panel.getByLabel("Reason").fill("playwright: incident drill");
  await panel.getByRole("button", { name: "Pause all automation" }).click();
  await panel.getByRole("button", { name: "Confirm pause" }).click();
  await expect(panel.getByTestId("pause-state")).toContainText("playwright: incident drill");
  await page.goto("/requests");
  await expect(page.getByTestId("pause-banner")).toContainText("ALL AUTOMATION IS PAUSED");
  await page.goto("/autonomy");
  await panel.getByLabel("Reason").fill("playwright: drill over");
  await panel.getByRole("button", { name: "Resume all automation" }).click();
  await panel.getByRole("button", { name: "Confirm resume" }).click();
  await expect(panel.getByTestId("pause-state")).toContainText("Running");
  await page.goto("/requests");
  await expect(page.getByTestId("pause-banner")).toHaveCount(0);
});
