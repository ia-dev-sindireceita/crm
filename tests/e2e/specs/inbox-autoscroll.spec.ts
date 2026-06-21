// Inbox message-thread auto-scroll browser smoke (SIN-65455).
//
// Drives the production web/static/js/inbox.js against the real inbox DOM
// contract served by cmd/inbox-autoscroll-e2e-fixture, asserting the four
// acceptance behaviours through real htmx swaps + real scrollTop:
//
//   1. open conversation        → thread lands at the bottom
//   2. send (POST .../messages) → pins to the bottom (even when the
//                                 operator had scrolled up)
//   3. inbound while at bottom  → pins to the bottom
//   4. inbound while scrolled up→ does NOT jump (history-reading is safe)
//
// The Go fixture is unit-tested in
// cmd/inbox-autoscroll-e2e-fixture/main_test.go (wiring + swap-shape
// contracts); this spec is the behavioural truth the static harness can't
// show. Scroll metrics are logged so the run output is numeric evidence.

import { test, expect, Page } from "@playwright/test";

const FIXTURE_PORT = Number(process.env.INBOX_FIXTURE_PORT || 8089);
const BASE = `http://127.0.0.1:${FIXTURE_PORT}`;

// "At the bottom" tolerance, in px. inbox.js sets scrollTop = scrollHeight
// (the browser clamps to scrollHeight - clientHeight), so a settled pin
// leaves distanceFromBottom at ~0; allow a couple px for sub-pixel layout.
const BOTTOM_SLACK = 2;

interface Metrics {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
  distanceFromBottom: number;
  bubbles: number;
}

async function threadMetrics(page: Page): Promise<Metrics> {
  return page.$eval("#conversation-thread", (el) => ({
    scrollTop: Math.round(el.scrollTop),
    scrollHeight: el.scrollHeight,
    clientHeight: el.clientHeight,
    distanceFromBottom: Math.round(el.scrollHeight - el.scrollTop - el.clientHeight),
    bubbles: el.querySelectorAll("li.message-bubble").length,
  }));
}

async function openConversation(page: Page): Promise<void> {
  await page.goto(`${BASE}/`);
  await page.click("#open-conversation");
  await page.waitForSelector("#conversation-thread li.message-bubble");
}

// fireInbound triggers the inbound moment exactly as production does: the
// #thread-live-poll sentinel itself fires the request, so the shipped
// inbox.js sees evt.detail.elt.id === "thread-live-poll" and runs its
// inbound (pin-when-near-bottom) branch. The sentinel is display:none +
// aria-hidden, so dispatch a raw click event (htmx listens for it) rather
// than a user-actionable click, which Playwright would reject as not visible.
async function fireInbound(page: Page): Promise<void> {
  await page.locator("#thread-live-poll").dispatchEvent("click");
}

// waitForBottom polls until the thread has settled at the bottom (inbox.js
// runs the scroll on htmx:afterSettle, one tick after the swap).
async function expectAtBottom(page: Page, label: string): Promise<Metrics> {
  await expect
    .poll(async () => (await threadMetrics(page)).distanceFromBottom, {
      message: `${label}: thread should settle at the bottom`,
      timeout: 5000,
    })
    .toBeLessThanOrEqual(BOTTOM_SLACK);
  const m = await threadMetrics(page);
  console.log(`[${label}] ${JSON.stringify(m)}`);
  return m;
}

test.describe("inbox thread auto-scroll (SIN-65455)", () => {
  test("1. opening a conversation lands scrolled to the latest message", async ({ page }) => {
    await openConversation(page);
    const m = await expectAtBottom(page, "open");
    // Precondition sanity: the seed thread really does overflow, otherwise
    // the scroll assertion would be vacuous.
    expect(m.scrollHeight).toBeGreaterThan(m.clientHeight + 50);
    expect(m.scrollTop).toBeGreaterThan(0);
  });

  test("2. sending pins to the just-sent message even after scrolling up", async ({ page }) => {
    await openConversation(page);
    await expectAtBottom(page, "open");

    // Scroll up to read history, then send — inbox.js must still pin
    // because the send swap targets #conversation-thread (not a poll
    // trigger), so it always pins regardless of scroll position.
    await page.$eval("#conversation-thread", (el) => {
      el.scrollTop = 0;
    });
    const before = await threadMetrics(page);
    console.log(`[send:scrolled-up] ${JSON.stringify(before)}`);
    expect(before.distanceFromBottom).toBeGreaterThan(BOTTOM_SLACK);

    await page.fill("#compose-body", "minha resposta");
    await page.click("#compose-submit");
    await page.waitForFunction(
      (n) => document.querySelectorAll("#conversation-thread li.message-bubble").length > n,
      before.bubbles,
    );
    const after = await expectAtBottom(page, "send");
    expect(after.bubbles).toBe(before.bubbles + 1);
    // The newest bubble is the outbound one we just sent.
    const lastDir = await page.$eval(
      "#conversation-thread li.message-bubble:last-child",
      (el) => el.getAttribute("data-direction"),
    );
    expect(lastDir).toBe("out");
  });

  test("3. an inbound message pins to bottom when already at the bottom", async ({ page }) => {
    await openConversation(page);
    const before = await expectAtBottom(page, "open");

    await fireInbound(page);
    await page.waitForFunction(
      (n) => document.querySelectorAll("#conversation-thread li.message-bubble").length > n,
      before.bubbles,
    );
    const after = await expectAtBottom(page, "inbound:at-bottom");
    expect(after.bubbles).toBe(before.bubbles + 1);
    const lastDir = await page.$eval(
      "#conversation-thread li.message-bubble:last-child",
      (el) => el.getAttribute("data-direction"),
    );
    expect(lastDir).toBe("in");
  });

  test("4. an inbound message does NOT yank the view when scrolled up reading history", async ({ page }) => {
    await openConversation(page);
    await expectAtBottom(page, "open");

    // Operator scrolls up to read history.
    await page.$eval("#conversation-thread", (el) => {
      el.scrollTop = 0;
    });
    const before = await threadMetrics(page);
    console.log(`[inbound:scrolled-up before] ${JSON.stringify(before)}`);
    expect(before.scrollTop).toBeLessThanOrEqual(BOTTOM_SLACK);

    await fireInbound(page);
    // Wait for the inbound bubble to actually append (so we know the swap
    // happened) before asserting the view did not move.
    await page.waitForFunction(
      (n) => document.querySelectorAll("#conversation-thread li.message-bubble").length > n,
      before.bubbles,
    );
    const after = await threadMetrics(page);
    console.log(`[inbound:scrolled-up after] ${JSON.stringify(after)}`);

    // The bubble was appended (thread grew) but the scroll position held —
    // inbox.js pins to bottom only when the operator was near the bottom.
    expect(after.bubbles).toBe(before.bubbles + 1);
    expect(after.scrollHeight).toBeGreaterThan(before.scrollHeight);
    expect(after.scrollTop).toBeLessThanOrEqual(BOTTOM_SLACK);
    expect(after.distanceFromBottom).toBeGreaterThan(BOTTOM_SLACK);
  });
});
