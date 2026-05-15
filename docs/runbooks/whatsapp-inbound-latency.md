# Runbook — WhatsApp inbound handler latency vs. Meta's 5s response budget

Owning task: [SIN-62762](/SIN/issues/SIN-62762).
Source: [SIN-62731](/SIN/issues/SIN-62731) — Fase 1 webhook receiver.
Code: `internal/adapter/channels/whatsapp/handler.go`, `internal/adapter/channels/whatsapp/handler_metrics.go`.
Wiring: `cmd/server/whatsapp_wire.go`.

## What this monitors

The `POST /webhooks/whatsapp` handler delivers every `entry[].changes[].messages[]`
synchronously through `inbox.HandleInbound` before it acknowledges Meta with
200 OK. The per-message `DeliverTimeout` is 4 seconds; a single envelope packing
5+ messages while Postgres latency degrades can exceed Meta's documented 5-second
response budget for webhook delivery. Meta retries on timeout, the dedup ledger
absorbs the retries, but the operational signal is a retry storm during a slow-DB
incident.

The signals to watch:

| Signal                                       | Source                                                              | Threshold |
| -------------------------------------------- | ------------------------------------------------------------------- | --------- |
| `whatsapp_handler_elapsed_seconds` (p99)     | Prometheus histogram, labelled by `result`                          | **>3s for 10 min → escalate to etapa 2 (queue-and-ACK)** |
| `whatsapp_handler_elapsed_seconds` (p99.9)   | Prometheus histogram                                                | >5s for any 1-min window → incident, page on-call           |
| `whatsapp.handler_complete` slog field       | Per-request `handler_elapsed_ms` + `result`                         | informational; used to reconstruct a single request         |
| `whatsapp.delivered` slog field              | Per-message `deliver_elapsed_ms`                                    | helps split handler latency between Postgres and overhead    |
| `whatsapp_status_lag_seconds`                | Existing status reconciler instrument (`status_reconciler.go`)      | unrelated to this runbook — covers carrier→receiver lag      |

## The 3-second escalation rule

If `histogram_quantile(0.99, sum by (le) (rate(whatsapp_handler_elapsed_seconds_bucket[5m]))) > 3s`
holds for 10 minutes:

1. **Pull a sample of slow envelopes from Loki**:

   ```logql
   {app="crm"} |= "whatsapp.handler_complete" | json | handler_elapsed_ms > 3000
   | line_format "{{.result}} {{.handler_elapsed_ms}}ms"
   ```

   Cross-reference `wamid` for the offending message(s) against the per-message
   `whatsapp.delivered` / `whatsapp.deliver_failed` lines. The
   `deliver_elapsed_ms` field localises whether the latency is in
   `inbox.HandleInbound` (Postgres) or in the surrounding handler.

2. **Check Postgres**. A spike on `whatsapp_handler_elapsed_seconds` with the
   `delivered` result label almost always tracks back to Postgres write latency
   on the `messages` / `contacts` / `inbound_message_dedup` tables. Look at the
   pool wait time and the slow-query log before touching the handler.

3. **If the signal persists past one Postgres incident**, file the etapa 2
   redesign as a child issue of [SIN-62762](/SIN/issues/SIN-62762):

   - Queue inbound deliveries in an in-process worker (buffered channel sized
     to envelope arrival rate, dropped-on-overflow with explicit drop counter).
   - 200-ACK Meta before processing.
   - Worker drains via `inbox.ReceiveInbound.HandleInbound` so the dedup ledger
     still guarantees at-most-once persistence.
   - Add a backpressure counter (`whatsapp_handler_queue_overflow_total`) so the
     "we dropped on overflow" path is observable distinct from the
     "we processed within budget" path.

## Result label semantics

`whatsapp_handler_elapsed_seconds{result=...}` partitions latency by terminal
outcome so a slow `delivered` (operational concern) is distinguishable from a
slow `dropped_signature` (almost certainly an attack — body parsing of a huge
forged payload). Bounded label set:

| `result`                    | When it fires                                                 |
| --------------------------- | ------------------------------------------------------------- |
| `delivered`                 | At least one message or status was routed through the inbox port. |
| `duplicate`                 | All routed messages returned `ErrInboundAlreadyProcessed` (retry). |
| `dropped_body_read`         | `io.ReadAll` failed or `MaxBytesReader` capped the body.       |
| `dropped_signature`         | `X-Hub-Signature-256` did not match HMAC(body, AppSecret).     |
| `dropped_parse`             | The envelope did not parse as JSON.                            |
| `dropped_timestamp_window`  | `entry.time` outside `[now-PastWindow, now+FutureSkew]`.       |
| `dropped_empty`             | Envelope parsed but contained no messages or statuses.         |
| `dropped_tenant`            | `phone_number_id` did not resolve to a tenant (anti-enumeration drop). |
| `dropped_rate_limited`      | Redis rate-limit denied the change block.                      |
| `dropped_feature_off`       | Tenant flag is off (`FEATURE_WHATSAPP_ENABLED`).               |
| `dropped_deliver_error`     | `inbox.HandleInbound` returned a non-dedup error (DB down etc.). |
| `dropped_other`             | A pre-validate fall-through (missing wamid/from, infra error). |

Cardinality is fixed at 12. Adding a new label requires updating
`handlerResultLabels` in `handler_metrics.go` and this table.

## What does NOT page

- A spike in `dropped_signature`. That is either an attacker probing or an
  app-secret rotation in flight. Cross-reference the security audit log and
  ADR 0073 (secret rotation) before responding.
- A spike in `dropped_rate_limited`. The Redis sliding-window limit is doing
  its job. Confirm via the per-tenant counter that the source is not a single
  tenant before tuning `WHATSAPP_RATE_MAX_PER_MIN`.
- A spike in `dropped_tenant`. Almost always a phone-number-association
  misconfiguration; check the tenant_channel_associations table.

## How to verify the metric is being scraped

After a deploy, hit `/metrics` on the public listener and grep:

```bash
curl -sS http://<host>/metrics | grep '^whatsapp_handler_elapsed_seconds'
```

You should see one `_bucket` line per (result × bucket) pair plus `_sum`
and `_count`. Empty output → the registry option was not passed in
`cmd/server/whatsapp_wire.go`.

## Related

- [SIN-62731](/SIN/issues/SIN-62731) — webhook receiver (where the latency
  amplification was flagged in PR [#86](https://github.com/ia-dev-sindireceita/crm/pull/86) review).
- ADR 0075 — webhook conventions.
- ADR 0087 — inbox idempotency / dedup ledger.
- [SIN-62193](/SIN/issues/SIN-62193) — Fase 1 MVP inbox WhatsApp + wallet básico.
