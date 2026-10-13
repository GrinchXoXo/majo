import { defineConfig, devices } from "@playwright/test";

// Dashboard smoke tests (reuse-audit R1 pilot). They need a running backend
// with some data and a running dashboard; nothing is started here.
//   DASH_URL=http://localhost:3000 E2E_OPERATOR_TOKEN=... npx playwright test
// Browsers: `npx playwright install chromium`, or point PW_CHROMIUM_PATH at
// an existing Chromium binary.
export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.DASH_URL ?? "http://localhost:3000",
    trace: "retain-on-failure",
    ...devices["Desktop Chrome"],
    launchOptions: process.env.PW_CHROMIUM_PATH ? { executablePath: process.env.PW_CHROMIUM_PATH } : {},
  },
});
