# wallet-alerter-worker

[SIN-62912](https://github.com/pericles-luz/crm) (Fase 3 W3D) — standalone process that subscribes to the
`wallet.balance.depleted` JetStream subject (published by the wallet
debit path, see [ADR 0043](../adr/0043-token-debito-atomico.md)) and
POSTs a single Slack message to the operator alerts channel for every
depletion event.

Worker library: [`internal/worker/wallet_alerter`](../../internal/worker/wallet_alerter).
The cmd entrypoint at [`cmd/wallet-alerter-worker`](../../cmd/wallet-alerter-worker)
is the only place that imports a NATS / Slack SDK — the domain package
stays vendor-free.

## Environment

| Variable                      | Required | Default                       | Notes |
|-------------------------------|----------|-------------------------------|-------|
| `NATS_URL`                    | yes      | —                             | e.g. `tls://nats.example:4222`. Plaintext `nats://` / `ws://` requires `NATS_INSECURE=1`. |
| `NATS_NAME`                   | no       | `crm-wallet-alerter-worker`   | Surfaced by NATS monitoring. |
| `NATS_CONNECT_TIMEOUT`        | no       | `10s`                         | Initial-dial cap. Go duration. |
| `SLACK_ALERTS_WEBHOOK_URL`    | no       | (empty → degraded)            | Operator `#alerts` incoming-webhook. Empty = degraded mode (worker boots + acks events without POSTing). |
| `WALLET_ALERTER_DEDUP_TTL`    | no       | `1h`                          | In-memory dedup window keyed on `(tenant_id, occurred_at)`. |
| `WALLET_ALERTER_ACK_WAIT`     | no       | `15s`                         | JetStream redelivery timeout. |

### NATS auth (pick exactly one)

| Variable           | Notes |
|--------------------|-------|
| `NATS_CREDS_FILE`  | Chained `.creds` JWT — preferred for production. Rotatable on disk. |
| `NATS_NKEY_FILE`   | NKey seed file (no JWT). |
| `NATS_TOKEN`       | Shared-secret bearer token. Legacy / dev only. |

### NATS transport security

| Variable            | Notes |
|---------------------|-------|
| `NATS_TLS_CA`       | PEM bundle. Required when `NATS_URL` is `tls://` or `wss://` (unless `NATS_INSECURE=1`). |
| `NATS_TLS_CERT`     | Optional client cert for mTLS. Paired with `NATS_TLS_KEY`. |
| `NATS_TLS_KEY`      | Optional client key for mTLS. Paired with `NATS_TLS_CERT`. |
| `NATS_INSECURE=1`   | Explicit opt-out from the secure-by-default posture. Dev / in-cluster only — pre-deploy review blocks for prod. |

## Posture: configured vs degraded

The Slack webhook is **optional**. Two postures are supported and both
keep the alerter package the same — only the boot path branches on
`SLACK_ALERTS_WEBHOOK_URL`.

### Configured (production)

- `SLACK_ALERTS_WEBHOOK_URL=https://hooks.slack.com/services/T.../B.../X...`
- Boot log: `wallet-alerter-worker starting ... slack_configured=true`
- Per event: one POST to the webhook with the formatted body. Duplicate
  events within `WALLET_ALERTER_DEDUP_TTL` collapse to a single POST.

### Degraded (webhook unset)

- `SLACK_ALERTS_WEBHOOK_URL=""`
- Boot log: `wallet-alerter-worker starting ... slack_configured=false`
  followed by `wallet_alerter: SLACK_ALERTS_WEBHOOK_URL not configured;
  alerts will be silently dropped`.
- Per event: the worker still consumes, dedups, and acks the JetStream
  delivery, but the Slack adapter's `Notify` returns nil immediately
  without dialling out. This is AC #3 of [SIN-62905](https://github.com/pericles-luz/crm).
- Use when the operator wants the worker scaffolding in place (so the
  durable consumer exists and depleted events do not accumulate forever
  in the stream) but is not yet ready to wire the webhook.

## Signals

| Signal    | Behaviour |
|-----------|-----------|
| `SIGINT`  | Cancel root ctx → drain subscription → drain NATS conn → exit 0. |
| `SIGTERM` | Same as SIGINT. |

In-flight deliveries get up to `WALLET_ALERTER_ACK_WAIT` to ack before
the broker considers them lost and redelivers to another replica.

## Smoke checks

### Local — configured

```bash
export NATS_URL=nats://nats:4222
export NATS_INSECURE=1
export SLACK_ALERTS_WEBHOOK_URL=https://hooks.slack.com/services/T.../B.../X...
go run ./cmd/wallet-alerter-worker
```

Then publish a manual event from another shell:

```bash
docker exec -i crm-nats-1 nats pub wallet.balance.depleted '{
  "tenant_id":"tenant-smoke",
  "policy_scope":"tenant:default",
  "last_charge_tokens":7777,
  "occurred_at":"2026-05-17T00:00:00Z"
}'
```

Expected: one `:warning: Wallet zerada em tenant ...` Slack message
plus an `wallet_alerter: alert dispatched` log line on the worker.

### Local — degraded

```bash
export NATS_URL=nats://nats:4222
export NATS_INSECURE=1
unset SLACK_ALERTS_WEBHOOK_URL
go run ./cmd/wallet-alerter-worker
```

Expected: boot log includes `slack_configured=false` and the warning
line; the same `nats pub` produces a worker log
`wallet_alerter: alert dispatched` without any Slack delivery
(nothing leaves the host).

## Docker / deploy

The worker is wired into `deploy/compose/compose.yml` as a sibling of
`mediascan-worker` (dev path uses `golang:1.22-alpine` + `go run`).
Operators set `SLACK_ALERTS_WEBHOOK_URL` in `deploy/compose/.env`; it
flows in via `env_file`, never inline `environment:`.

> ⚠️ Never set `SLACK_ALERTS_WEBHOOK_URL=<url>` on a `docker run --env`
> argv positional pair. The CTO `docker --env` review pattern requires
> `--env NAME` (name-only) so the URL never appears in `ps`.

A dedicated production image (multi-stage build) is a follow-up to
this PR — same gap exists today for `cmd/mediascan-worker`, which also
runs via `go run` in compose and is not built into the
`cmd/server`-only `Dockerfile`. See SIN follow-up issue once filed.

## Stream contract

The worker calls `EnsureStream("WALLET", []string{"wallet.balance.depleted"})`
at boot. Stream defaults come from the SDK adapter:

- Storage: `FileStorage`
- Retention: `WorkQueuePolicy`
- Duplicates window: 1h (matches the dedup TTL default)

The producer (wallet debit path) MUST publish to the
`wallet.balance.depleted` subject inside the `WALLET` stream. If the
producer eventually adopts the `.v1`-suffixed subject documented in
[ADR 0043 §Mechanism step 5](../adr/0043-token-debito-atomico.md), the
consumer constant in
[`internal/worker/wallet_alerter/alerter.go`](../../internal/worker/wallet_alerter/alerter.go)
must move in the same PR — the consumer and producer subject names are
a hard coupling.

## Troubleshooting

| Symptom                                                            | Likely cause                                                                                                                         |
|--------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------|
| Worker logs `slack notify failed ... status 4xx`                   | Webhook URL is stale (regenerated by Slack) or the channel was archived. Rotate the URL in `.env` and restart.                       |
| `wallet_alerter: subscribe: ...` at boot                           | Durable consumer name conflict — another deploy with `wallet-alerter-v1` is bound. Drain the stale subscription via `nats consumer rm`. |
| Boot log shows `slack_configured=true` but no Slack POST           | The webhook URL is set but the worker is acking duplicate `(tenant_id, occurred_at)` pairs. Check `wallet_alerter: duplicate event suppressed`. |
| `nats.Connect: ... refusing plaintext URL`                         | `NATS_URL` is `nats://` without `NATS_INSECURE=1`. Either switch to `tls://` + `NATS_TLS_CA`, or set `NATS_INSECURE=1` if the broker is on a private network. |
