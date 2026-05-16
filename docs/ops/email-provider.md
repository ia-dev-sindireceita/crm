# Email provider — runbook

Decision: [ADR 0096](../adr/0096-email-mailgun-adapter.md). Production
provider is **Mailgun (Sinch)**. Adapters live under
`internal/adapter/notify/email/{mailgun,noop,recorder}`; selection is
driven by `EMAIL_PROVIDER` at boot.

## Provider selection

| Env value           | When                  | Behaviour                                     |
| ------------------- | --------------------- | --------------------------------------------- |
| `mailgun`           | production            | POST to Mailgun REST API (requires keys)      |
| `recorder`          | CI / integration test | in-memory capture for assertions, no network  |
| `noop`              | local dev (default)   | validates message, drops silently             |
| _unset_             | dev only              | falls back to `noop`. **Prod boot refuses.**  |

## Required env vars (when `EMAIL_PROVIDER=mailgun`)

| Variable           | Example                | Notes                                                              |
| ------------------ | ---------------------- | ------------------------------------------------------------------ |
| `MAILGUN_API_KEY`  | `key-xxxxxxxx`         | Region-private API key. Never logged. Never placed in URLs.        |
| `MAILGUN_DOMAIN`   | `mg.sindireceita.com.br` | Authenticated sending domain (SPF/DKIM verified at the provider). |
| `MAILGUN_REGION`   | `us` or `eu`           | Choose the region the API key belongs to; wrong region → 404.     |

`APP_ENV=production` + unset `EMAIL_PROVIDER` returns
`email factory: missing environment variable: EMAIL_PROVIDER` and the
process exits — by design, so a misconfigured deploy never silently
loses outgoing email.

## Switching providers later

1. Add the new adapter under `internal/adapter/notify/email/<name>`
   implementing `internal/notify/email.EmailSender`.
2. Add a case to `internal/notify/email/factory/factory.go`
   (`case "<name>":`).
3. Add an ADR superseding [0096](../adr/0096-email-mailgun-adapter.md).
4. Roll out by changing `EMAIL_PROVIDER` and restarting; no
   application code change is needed at the call sites because every
   producer talks to the port.

## Operational notes

- The adapter classifies failures into `email.ErrTransient`
  (network blip, HTTP 408 / 429 / 5xx) and `email.ErrPermanent`
  (auth failure, banned recipient, malformed domain). Producers
  decide their retry policy with `errors.Is`.
- Logs are structural only: recipient count, payload bytes, status,
  Mailgun message-id. **Subject and body are never logged** (ADR 0004
  PII discipline).
- HTTPS only. TLS verification uses Go defaults (no
  `InsecureSkipVerify`). HTTP Basic auth uses the literal username
  `api` and the API key as password — standard Mailgun convention.

## Rotating `MAILGUN_API_KEY`

Routine secret rotation for the production sending account. Cross-reference
the secret-rotation policy in [ADR 0073](../adr/0073-csrf-and-session.md).
Trigger this runbook on the scheduled cadence, after an incident, or on
operator offboarding.

1. In the Mailgun console (Sending → Domain settings → API keys for the
   active sending domain), create a new region-private API key. **Leave
   the previous key active** so in-flight sends and the rollback window
   keep working.
2. Update `deploy/compose/.env` on the VPS:
   `MAILGUN_API_KEY=<new-key>`. Keep the previous value commented out
   in the same file for the rollback window — do not delete it yet.
3. Restart the app process so the factory re-reads the env:
   `docker compose -f deploy/compose/compose.yml restart app`.
4. Confirm a real send succeeds: trigger an email-emitting flow
   (e.g. password reset for a test account) and verify the
   `mailgun: send ok` log line appears with a fresh `provider_id`
   (Mailgun message-id). If you only see `email send failure`, you are
   on the rollback path — see below.
5. Once step 4 is green, revoke the previous key in the Mailgun console
   and delete the commented-out value from `deploy/compose/.env`.
6. Record the rotation in the secret-rotation log (per ADR 0073):
   date, operator, reason (scheduled / incident / offboarding), and the
   first 4 characters of the new key fingerprint (never the full key).

### Rollback

If step 4 fails (auth error, wrong region, or the new key was minted on
the wrong domain), re-enable the previous key in the Mailgun console
panel from step 1, restore the previous `MAILGUN_API_KEY` value in
`deploy/compose/.env`, and re-run step 3 with the old value. No
application code changes are needed for rollback — the factory re-reads
env at boot and every call site talks to the port, not the adapter.

## Integration tests against a real Mailgun account

Optional. Gated by `MAILGUN_INTEGRATION_TESTS=1`; skipped in CI. To
run locally:

```bash
export MAILGUN_INTEGRATION_TESTS=1
export MAILGUN_API_KEY=key-…
export MAILGUN_DOMAIN=mg.example.com
export MAILGUN_REGION=us
go test ./internal/adapter/notify/email/mailgun -run Integration
```

(Today the package has only unit tests against `httptest.Server`; the
gate is reserved for the first producer that wants a soak test against
a real account.)
