// Playwright config for the CRM browser-smoke suite (SIN-62318).
//
// The Go fixture binary is built once via the `fixture:build` npm script
// and started by Playwright as a webServer pinned to 127.0.0.1:8088.
// Tests run against that loopback origin only — the fixture is not a
// public surface.

import { defineConfig, devices } from "@playwright/test";

const FIXTURE_PORT = Number(process.env.AIPANEL_FIXTURE_PORT || 8088);
const FIXTURE_HOST = "127.0.0.1";
const COOLDOWN_MS = Number(process.env.AIPANEL_COOLDOWN_MS || 1500);

// SIN-65455: a second fixture serves the inbox thread auto-scroll smoke on
// its own loopback port. inbox-autoscroll.spec.ts targets it by absolute
// URL so the aipanel project's baseURL is untouched.
const INBOX_FIXTURE_PORT = Number(process.env.INBOX_FIXTURE_PORT || 8089);

export default defineConfig({
  testDir: "./specs",
  fullyParallel: false,
  workers: 1,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 30_000,
  expect: { timeout: 10_000 },

  use: {
    baseURL: `http://${FIXTURE_HOST}:${FIXTURE_PORT}`,
    trace: process.env.CI ? "retain-on-failure" : "on-first-retry",
    video: "retain-on-failure",
    screenshot: "only-on-failure",
  },

  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        // Allow tests to opt into prefers-reduced-motion explicitly.
        reducedMotion: "no-preference",
      },
    },
  ],

  webServer: [
    {
      command: `./.bin/aipanel-e2e-fixture -addr ${FIXTURE_HOST}:${FIXTURE_PORT} -cooldown ${COOLDOWN_MS}ms -static ../../web/static`,
      url: `http://${FIXTURE_HOST}:${FIXTURE_PORT}/`,
      reuseExistingServer: !process.env.CI,
      timeout: 15_000,
      stdout: "pipe",
      stderr: "pipe",
    },
    {
      command: `./.bin/inbox-autoscroll-e2e-fixture -addr ${FIXTURE_HOST}:${INBOX_FIXTURE_PORT} -static ../../web/static`,
      url: `http://${FIXTURE_HOST}:${INBOX_FIXTURE_PORT}/`,
      reuseExistingServer: !process.env.CI,
      timeout: 15_000,
      stdout: "pipe",
      stderr: "pipe",
    },
  ],
});
