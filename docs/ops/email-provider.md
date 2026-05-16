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
