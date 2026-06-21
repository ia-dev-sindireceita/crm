# tests/e2e — browser smoke

Playwright suite that drives the production renderers
(`internal/http/handler/aipanel`) + rate-limit middleware
(`internal/http/middleware/ratelimit`) end-to-end through a real browser.
Adds the AI panel cooldown UI smoke for SIN-62318.

## What runs here

- `cmd/aipanel-e2e-fixture` — a Go HTTP fixture binary (NOT a production
  surface) that mounts the live regenerate button, the cooldown swap, and
  an in-memory token-bucket limiter so the spam-click flow is
  deterministic in a few seconds.
- `tests/e2e/specs/aipanel-cooldown.spec.ts` — covers the four
  acceptance criteria from SIN-62318:
  1. spam-click → 429 → outerHTML swap → bar shrinks → recovery
  2. `prefers-reduced-motion: reduce` collapses the bar without animation
  3. keyboard focus + native disabled semantics survive the swap
  4. CLS during the swap stays under the 0.05 "Good" threshold
- `cmd/inbox-autoscroll-e2e-fixture` — a Go HTTP fixture binary (NOT a
  production surface) that serves the real `web/static/js/inbox.js`
  against the production inbox DOM contract (the `#conversation-thread`
  scroll container inside `#inbox-conversation-pane`) and reproduces the
  three htmx swap shapes that move the thread (open/innerHTML,
  send/beforeend, inbound/OOB-beforeend). Listens on `127.0.0.1:8089`.
- `tests/e2e/specs/inbox-autoscroll.spec.ts` — covers the four
  acceptance behaviours from SIN-65455, asserting real `scrollTop`:
  1. opening a conversation lands scrolled to the latest message
  2. sending pins to the just-sent message (even after scrolling up)
  3. an inbound message pins to bottom when already at the bottom
  4. an inbound message does NOT yank the view when scrolled up reading
     history (pin-to-bottom only when near the bottom)

The Go fixtures are unit-tested in `cmd/*-e2e-fixture/main_test.go`
and run in the standard `go test ./...` suite.

## One-time setup

The repo does not vendor Playwright or its browser bundle (~300 MB).
Each developer / CI runner installs them locally:

```bash
cd tests/e2e
npm install
npx playwright install chromium --with-deps
```

`--with-deps` pulls system libraries on Linux. Skip it on macOS/WSL.

## Running the suite

```bash
# from repo root — build the Go fixture, then run Playwright
cd tests/e2e
npm run fixture:build
npm test
```

`npm test` starts the fixture as Playwright's `webServer` on
`127.0.0.1:8088`, runs the spec, and shuts the fixture down. Set
`CI=1` to enable retries + the GitHub reporter.

### Useful overrides

- `AIPANEL_FIXTURE_PORT` — port the fixture binds to (default `8088`).
- `AIPANEL_COOLDOWN_MS` — token-bucket refill interval the test waits on
  (default `1500` ms). Lower for faster runs; raise if a busy laptop
  flakes the bar-width assertion.

### Headed / debugging

```bash
npm run test:headed                 # watch the browser drive the swap
npx playwright test --ui            # interactive UI mode
npx playwright test --debug         # step through with Playwright Inspector
npm run test:report                 # open HTML report after a run
```

## CI

Not yet wired into `.github/workflows/ci.yml`. The Go fixture's unit
tests (`./cmd/aipanel-e2e-fixture/...`) run as part of the standard
`go test ./...` job and gate the swap-target / wiring invariants. The
browser layer is opt-in until the runner image picks up Playwright;
follow-up issue tracks that.

## Why a fixture and not the real `cmd/server`

The production server isn't yet wired with the AI panel route — that
landed at the renderer level in SIN-62317 and is mounted by whichever
tenant route owns the panel. The fixture wires the renderers + the
production middleware in the smallest possible harness so the browser
flow can be tested independently of the larger panel-host route. When
the production route lands, this suite can be repointed at it (or kept
as the smoke harness for the renderer in isolation).
