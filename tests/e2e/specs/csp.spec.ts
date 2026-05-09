// CSP regression — SIN-62285 PR-B3.
//
// Loads /dashboard and counts SecurityPolicyViolation events the browser
// raises while the page settles. The Go middleware (SIN-62245) emits a CSP
// with a per-request nonce; every inline script/style on the page must carry
// the matching nonce or be loaded from `'self'`. A non-zero violation count
// means production HTML is leaking inline assets that bypass the nonce —
// either rip them out or migrate them to vendored files (ADR 0082 §4).
//
// We also assert that the response actually carries a Content-Security-Policy
// header so a regression that *removes* CSP entirely (Caddy fallback +
// middleware both off) fails loudly instead of silently passing zero
// violations.

import { test, expect, type Page } from "@playwright/test";

type Violation = {
  blockedURI: string;
  violatedDirective: string;
  sourceFile?: string;
  lineNumber?: number;
};

async function collectCspViolations(page: Page): Promise<Violation[]> {
  return await page.evaluate(
    () =>
      new Promise<Violation[]>((resolve) => {
        const events: Violation[] = [];
        document.addEventListener(
          "securitypolicyviolation",
          (e: SecurityPolicyViolationEvent) => {
            events.push({
              blockedURI: e.blockedURI,
              violatedDirective: e.violatedDirective,
              sourceFile: e.sourceFile,
              lineNumber: e.lineNumber,
            });
          },
          { capture: true }
        );
        // Give async script/style application a tick to settle before resolving.
        setTimeout(() => resolve(events), 1500);
      })
  );
}

test("/dashboard emits zero CSP violations", async ({ page }) => {
  const cspHeaders: string[] = [];
  page.on("response", (resp) => {
    if (resp.url().endsWith("/dashboard") || resp.url().endsWith("/dashboard/")) {
      const v = resp.headers()["content-security-policy"];
      if (v) cspHeaders.push(v);
    }
  });

  await page.goto("/dashboard", { waitUntil: "networkidle" });

  expect(cspHeaders, "Content-Security-Policy header missing on /dashboard").not.toHaveLength(0);

  const violations = await collectCspViolations(page);
  expect(
    violations,
    `unexpected CSP violations on /dashboard:\n${JSON.stringify(violations, null, 2)}`
  ).toHaveLength(0);
});
