# Webchat Widget (SIN-62800 / F2-14)

Vanilla JavaScript, CSP-safe widget that lets a visitor on a tenant
site chat with that tenant's CRM inbox over the backend endpoints
shipped in F2-11 (SIN-62798).

## Layout

```
widget/v1/
  widget.js        # source (~270 LoC)
  widget.css       # minimal styles for the floating button + panel
  widget.test.js   # node --test units (pure helpers + mount() against a stub DOM)
  README.md        # this file
static/widget/     # build output (git-ignored, rebuilt by `make widget`)
  widget.js        # minified bundle served from <tenant>.crm.<host>/widget.js
  widget.css
```

## Build

```
make widget       # produces static/widget/widget.{js,css}
make widget-test  # runs node --test
```

The build uses `npx --yes esbuild@<pinned>` so no `npm install` step and
no committed `node_modules` are needed. The pin lives in the Makefile
(`ESBUILD_VERSION`).

## Embed snippet (tenant site)

```html
<script src="https://<tenant>.crm.<host>/widget.js" defer></script>
<noscript>
  <form action="https://<tenant>.crm.<host>/widget/v1/message" method="POST">
    <label>Mensagem
      <textarea name="body" required></textarea>
    </label>
    <button type="submit">Enviar</button>
  </form>
</noscript>
```

The widget itself does not need any inline configuration: it derives
its API base from its own `currentScript.src`, so the embed snippet is
the same for every tenant.

## Security posture

- **No `eval`, no `Function()`, no `innerHTML`.** All dynamic text is
  inserted via `textContent`, which the browser HTML-escapes
  automatically. The widget unit tests assert this by failing the test
  if any `innerHTML` write is attempted (the DOM stub throws on
  assignment).
- **CSP-safe.** The widget can run under a `script-src` policy that
  forbids `unsafe-inline` and `unsafe-eval`. Inline styles are not used
  either — visual styling lives in `widget.css`.
- **Global namespace.** Only `window.__sindCRMWidget` is exposed.
- **Storage scope.** Session credentials live in `sessionStorage` (tab
  scope), never `localStorage`.
- **Transport.** `fetch(..., { credentials: 'omit' })` for the JSON
  POSTs; the SSE stream is authenticated via the session id alone, in
  the `?session_id=` query parameter, because the browser's
  `EventSource` API cannot send custom headers. The CSRF token still
  gates writes (`POST /widget/v1/message`).

## Reconnection

`EventSource` auto-reconnects with `Last-Event-ID` on transient
network drops. When the server returns 401/403 (expired session) the
widget closes the stream, clears `sessionStorage`, and the next send
re-establishes a fresh session.

## Manual browser verification (QA / CTO)

1. `make widget`
2. Boot the local stack (`make up`) with `FEATURE_WEBCHAT_ENABLED=true`
   for at least one tenant.
3. Serve `static/widget/` at `/widget.js` on the tenant host.
4. Open a page that embeds the snippet, click the floating button.
   Expected: panel opens, `POST /widget/v1/session` returns 200, an
   `EventSource` connection appears in DevTools → Network, and a
   message published from the agent UI is rendered in the panel.
5. Confirm in DevTools that `window.__sindCRMWidget` is the only new
   global and that `Content-Security-Policy: script-src 'self'` does
   not produce any inline-script errors.
