# ADR 0096 — Transactional email: Mailgun adapter on net/http

- Status: Accepted
- Date: 2026-05-16
- Drives: [SIN-62321](/SIN/issues/SIN-62321) (this ADR), [SIN-62194](/SIN/issues/SIN-62194) (Fase 2)
- Confirms: plan rev 3 decision #9, [SIN-62206](/SIN/issues/SIN-62206) D3 resolution

## Context

Decision #9 of the [plan](/SIN/issues/SIN-62190#document-plan) names
**Mailgun (Sinch)** as the transactional email provider, confirmed by
the board in [SIN-62206](/SIN/issues/SIN-62206). [SIN-62321](/SIN/issues/SIN-62321)
is the implementation child for that decision: replace the absent
provider wiring with a real adapter at the start of Fase 2.

The acceptance criteria leave the HTTP transport choice to the
implementer with a short ADR: either the official SDK
`github.com/mailgun/mailgun-go/v4` or `net/http` directly against the
REST API.

## Decision

**The Mailgun adapter is implemented with `net/http` directly, not the
official SDK.**

The CRM `EmailSender` port lives at `internal/notify/email`. Concrete
adapters live under `internal/adapter/notify/email/{mailgun,noop,recorder}`
and the DI factory at `internal/notify/email/factory` selects between
them on `EMAIL_PROVIDER`.

## Rationale

### Why net/http, not the SDK

1. **Existing precedent.** `internal/adapter/notify/slack` already
   speaks to a third-party HTTP API via `net/http` with an injectable
   `Doer` interface for tests. A second notify adapter following the
   same shape is easier to review and to operate.
2. **Boring-tech budget.** The CRM project rule explicitly prefers the
   stdlib over fashionable libraries when the call surface is small.
   The Mailgun `POST /v3/{domain}/messages` endpoint is a single
   multipart form post; a 50-file SDK is overkill.
3. **Supply chain.** `mailgun-go/v4` pulls a transitive graph
   (`go-resty`, `gopkg.in/h2non/gentleman.v2`, …). Every new
   dependency widens the `govulncheck` surface and the SBOM that
   security review and the gitleaks/golangci pipeline must trust.
   Avoiding it keeps the supply chain narrow.
4. **Testability.** A bare `net/http` adapter is trivially exercised
   with `httptest.Server` (real-network behaviour, real status codes,
   real multipart parsing). The SDK abstracts over the wire format, so
   tests would assert on SDK-internal types instead of the actual
   payload Mailgun receives.
5. **Reversibility.** The adapter sits behind `email.EmailSender`, so
   swapping to the SDK later — or to Resend/Postmark — is a sibling
   package, not a refactor. The "boring choice now" does not foreclose
   the SDK choice later.

### Why net/http is sufficient

The Mailgun /messages contract used here is:

- `POST {region}/v3/{domain}/messages`
- `Authorization: Basic api:{key}`
- `Content-Type: multipart/form-data`
- Fields: `from`, `to[]`, `cc[]`, `bcc[]`, `subject`, `text`, `html`,
  `h:{custom-header}` (incl. `h:Reply-To`), and one `attachment` part
  per attachment.
- Response: `200 OK` with `{"id": "<message-id>", "message": "queued"}`.

All of that is one Go function (`buildMultipart`) plus a status
classifier. No retries, no template engine, no IP-pool routing, no
domain-management calls. If we ever need those, that is the moment to
revisit the SDK.

## Consequences

### Positive

- Zero new direct or transitive dependencies (`go.mod` unchanged).
- Coverage above 85% per package without touching test infra.
- Sender → port → adapter wiring is the same shape as `slack`, so a
  reviewer who knows `slack.go` can read `mailgun.go` in five minutes.
- Region selection (`us` / `eu`) and base URL override (`BaseURL`)
  make `httptest.Server`-driven tests trivial and explicit.

### Negative

- Future Mailgun feature use (suppression lists, mailing-list API,
  templates) must be coded by hand; the SDK already wraps those. If
  that workload grows, this ADR should be superseded by an "adopt
  mailgun-go" ADR.
- The adapter knows only `multipart/form-data`. Mailgun also accepts
  MIME messages via `/messages.mime`; if a producer needs raw MIME
  passthrough (e.g. signed S/MIME), we either add a sibling adapter or
  extend this one.

### Operational

Three env vars are required when `EMAIL_PROVIDER=mailgun`:

- `MAILGUN_API_KEY` — region-private API key.
- `MAILGUN_DOMAIN` — sending domain (e.g. `mg.sindireceita.com.br`).
- `MAILGUN_REGION` — `us` or `eu`.

The factory fails fast when `APP_ENV=production` and `EMAIL_PROVIDER`
is unset, so a misconfigured prod deploy refuses to start instead of
silently dropping email. Dev defaults to `noop`; CI uses `recorder`.

### Out of scope (deliberate)

- Templates (a producer concern, owned by the IAM / wallet / billing
  flows when they wire the first real email).
- Inbound email parsing.
- DNS / SPF / DKIM provisioning (operations work).
- Retry policy (callers wrap `Send` and switch on `email.ErrTransient`
  / `email.ErrPermanent`).

## Rollback

Disable email by setting `EMAIL_PROVIDER=noop` and restarting — the
adapter selection is per-process and contains no shared state. Reverting
the PR removes the adapter and factory packages; the port is the only
new public symbol and is unused outside `factory.New`, so revert is
free of caller-side fanout.
