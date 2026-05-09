// Playwright config — SIN-62285 PR-B3.
//
// Minimal Chromium-only config wired against ${BASE_URL}, defaulting to the
// staging smoke URL secret. The CSP regression suite (specs/csp.spec.ts) is
// the only spec today; new specs land alongside it under specs/ and are
// picked up automatically by the testDir glob.

import { defineConfig, devices } from "@playwright/test";

const BASE_URL = process.env.BASE_URL ?? "http://localhost:8080";

export default defineConfig({
  testDir: "specs",
  timeout: 30_000,
  fullyParallel: false,
  reporter: process.env.CI ? [["github"], ["list"]] : [["list"]],
  use: {
    baseURL: BASE_URL,
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
