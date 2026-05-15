# ADR 0095 — Realtime message status: HTMX polling (Option A) vs SSE (accepted)

> **Status:** Accepted — Coder + CTO sign-off per [SIN-62736](/SIN/issues/SIN-62736) AC §2.
> **Antecedents:** [ADR 0073](./0073-htmx-stdlib-templates.md) (HTMX over SPAs), [ADR 0094](./0094-webhook-timestamp-past-window.md) (PR8 status reconciler context).
> **Bundle severity:** LOW (UX freshness, no security/correctness implication; polling cost bounded).
> **Lenses applied:** HTMX over SPAs, Boring technology, Reversibility, Observability.

## 1. Context

Fase 1 inbox UI (SIN-62193) renders message bubbles with a lifecycle status: `pending → sent → delivered → read` for outbound messages, plus the terminal `failed`. The status is persisted on the `message` row and advanced server-side by the WhatsApp status reconciler (PR8, [SIN-62734](/SIN/issues/SIN-62734)). The remaining gap is **propagation to the operator's open inbox tab without a full page reload**.

Two paths are credible for the Fase 1 budget (< 400 LoC delivered, AC #2 says p95 UI transition < 5s):

- **Option A — short polling.** Each non-final bubble carries `hx-trigger="every 3s"` on a `GET /inbox/conversations/:id/messages/:msgID/status?currentStatus=…` endpoint that returns the re-rendered bubble (200) or a `304 Not Modified` when the persisted status matches the caller's `currentStatus` query parameter. HTMX's default behaviour on 304 is to skip the swap, so the DOM is untouched on the no-change path.
- **Option B — server-sent events (SSE).** A long-lived `GET /inbox/conversations/:id/events` keeps a connection per open conversation tab. The status reconciler (or a fan-out bus) publishes a status-change event; the server pushes the new bubble into the open SSE stream. Latency is sub-second; the cost is an internal pub/sub (NATS subject, Redis pub/sub, in-process broadcaster, …), reconnect/backoff handling, throttling, and a per-connection footprint on the listener.

## 2. Decision

**Adopt Option A (short polling) for Fase 1.** Document Option B as the documented scale-out path in §5.

The decision is rooted in three constraints:

1. **Boring-technology budget.** Option A uses only the HTTP request/response cycle, the existing HTMX runtime already loaded in the inbox shell (`web/static/vendor/htmx/2.0.9/htmx.min.js`), and the existing inbox repository port. No new infra, no new component to monitor.
2. **Blast radius and reversibility.** Polling is per-bubble; the operator's open tab is the only consumer. Disabling the feature is a one-line template change (drop the `hx-*` attributes) or, ultimately, a feature flag on the handler that returns the bubble without polling attrs.
3. **Latency budget.** The AC asks for p95 < 5s on Option A. Polling at 3s puts p95 ≤ 6s in the worst case (rendered bubble was emitted just after a status change, next poll fires almost three seconds later). With `Cache-Control: no-store` and the no-swap 304 path, the cost per non-final bubble per operator per 3s is one HTTP request that returns ~200 bytes on the unchanged branch.

## 3. Mechanics

### 3.1 Endpoint contract

```
GET /inbox/conversations/{conversationID}/messages/{msgID}/status[?currentStatus=<status>]
```

- The handler resolves the tenant from the request context, parses the two UUIDs, and calls the `GetMessage` use case (`internal/inbox/usecase/get_message.go`).
- If `?currentStatus=` is omitted (cold call / bootstrap), the handler always returns 200 + the rendered bubble. This makes the endpoint usable as a standalone read for non-polling callers (e.g. tests, future server-side rendering paths).
- If `?currentStatus=` matches the persisted status, the handler returns `304 Not Modified` with `Cache-Control: no-store` and an empty body. HTMX leaves the DOM untouched.
- If `?currentStatus=` differs from the persisted status, the handler returns `200 OK` + the re-rendered bubble. HTMX swaps `outerHTML` on the bubble.
- `inbox.ErrNotFound` from the use case maps to `404`. Tenant absence maps to `500` (a programming error in the middleware). Unparseable IDs map to `400`.

### 3.2 Bubble template contract

The `message_bubble` template (`internal/web/inbox/templates.go`) emits the polling attributes only when the message is **outbound** AND its status is **not final** (∉ `{read, failed}`):

```html
<li id="msg-{ID}" class="message-bubble {direction-class}" data-status="{status}"
    hx-get="/inbox/conversations/{conv}/messages/{id}/status?currentStatus={status}"
    hx-trigger="every 3s"
    hx-target="this"
    hx-swap="outerHTML">
  …
</li>
```

When the status reaches `read` or `failed`, the next bubble swap drops the `hx-*` attributes. HTMX's outerHTML swap replaces the polling node with one that has no trigger; the loop terminates.

Inbound bubbles never emit polling attributes and never render a status glyph — the lifecycle states are an outbound delivery-ack concept.

### 3.3 Glyphs and accessibility

WhatsApp-style indicators with Portuguese aria-labels:

| Status      | Glyph | aria-label              |
|-------------|-------|-------------------------|
| `pending`   | ⏱     | "Aguardando envio"       |
| `sent`      | ✓     | "Enviada"                |
| `delivered` | ✓✓    | "Entregue"               |
| `read`      | ✓✓    | "Lida" (CSS class colors blue) |
| `failed`    | ⚠     | "Falha ao enviar"        |

Color cues live in `web/static/css/inbox.css` under `.message-bubble__status--{status}` classes; the template emits the class but does not inline color, so a future redesign does not require template changes.

### 3.4 Cache headers

Every response sets `Cache-Control: no-store`. The endpoint is operator-private (per-tenant) and the value churns on the order of seconds; we do not want intermediate caches (CDN edge, browser disk) pinning a stale partial. The 304 short-circuit lives at the application layer using the explicit `?currentStatus=` query, not browser `If-None-Match` — which would be unreachable under `no-store`.

## 4. Why not Option B (SSE)

| Concern                       | Polling (A)                                       | SSE (B)                                                                    |
|-------------------------------|---------------------------------------------------|----------------------------------------------------------------------------|
| Median latency                | ~1.5s                                             | < 1s                                                                       |
| p95 latency                   | < 6s                                              | < 1.5s                                                                     |
| Network per non-final bubble  | ~200B per 3s (304) — ~67B/s                       | Persistent TCP + 0B until event, then ~400B                                |
| Infra dependencies            | None new                                          | Pub/sub (NATS / Redis / in-proc), reconnect, fan-out, throttling           |
| Monitoring surface            | Same as any HTMX endpoint                         | New: connection count, dropped events, fan-out lag                          |
| Reversibility cost            | Drop hx-* attributes (1 line)                     | Tear down endpoint + pub/sub + per-connection state machine                |
| Operator concurrency          | Bounded by HTTP request budget                    | Bounded by listener FD budget; load test required before prod                |

Option B is the right answer **once** real-world data shows that operators routinely keep > N tabs open AND status freshness drives a measurable triage SLO. Both gating signals are absent today; we have zero production data on operator concurrency or freshness sensitivity. Picking Option A keeps the change small, observable, and reversible.

## 5. Scale-out path (informational)

If Fase 2 telemetry (PR8's `whatsapp_status_lag_seconds` histogram + a future `inbox_polling_qps` gauge) shows polling exceeding our budget, the SSE path is:

1. Introduce `GET /inbox/conversations/:id/events` — keeps a tenant- and conversation-scoped HTTP/1.1 stream open per tab.
2. The status reconciler publishes `(tenantID, conversationID, messageID, newStatus)` onto an internal channel (start with `internal/event/inproc`, escalate to NATS subject `inbox.status.{tenantID}.{conversationID}` when multi-instance).
3. The SSE handler subscribes to the operator's open conversation, debounces events at 250ms, and writes `event: status\n data: <bubble-html>\n\n`.
4. The bubble template swaps `hx-get/hx-trigger` for `hx-ext="sse"` + `sse-connect=…` on the conversation pane (not per-bubble), with `sse-swap="status-{msgID}"`.

The fallback (browser without `EventSource`, proxy stripping chunked responses, …) is the polling implementation shipped here.

## 6. Observability

- The existing `whatsapp_status_lag_seconds` histogram (PR8) measures **reconciler → DB**. UI freshness is `lag_seconds + (polling period / 2)` on average. We don't add a new histogram in this PR; if a gap appears between reconciler lag and operator-perceived lag, we add an end-to-end histogram in a follow-up.
- The status endpoint inherits the standard request-log line; no per-endpoint metric is required for Fase 1.

## 7. Consequences

- Operators see `pending → sent → delivered → read` transitions within ~3–6 seconds without reloading the conversation pane.
- Each open conversation tab generates ≤ N (number of non-final outbound bubbles) requests per 3s. A typical reply with 1–5 in-flight bubbles is well under any reasonable limit.
- Final-state bubbles do not poll. Closing the conversation tab terminates polling client-side immediately.
- The endpoint can be disabled per-tenant or per-rollout by short-circuiting the handler to always return 404 — useful if a follow-up incident requires a hard kill switch.

## 8. Out of scope

- Inbound message receipts (`read by operator`): Fase 1 does not propagate per-operator read state to the contact.
- Realtime conversation-list freshness (the left-pane "last message" snippet): handled separately in a follow-up; the inbox shell still requires a manual reload for new-conversation visibility.
- Push to non-operator surfaces (mobile, webhooks for partner systems): out of Fase 1 scope.
