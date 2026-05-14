# ADR 0004 — Logging and Audit

**Status:** Accepted  
**Date:** 2026-05-14  
**Authors:** Coder (SIN-62255), ratified by CTO  

---

## Context

The CRM application processes sensitive personal data (CPF, email, phone, passwords, TOTP codes) and financial information. Logs written to stdout are shipped to a centralised log sink visible to operators. A naive `slog.Info("login", "user", u)` call that prints a struct can inadvertently expose PII or secrets.

Prior to this ADR the `internal/obs` package provided a raw JSON logger with no scrubbing. Penetration test finding F51 (HIGH) identified password echo in login traces; F54 (MEDIUM) identified CPF leakage via webhook debug logs.

---

## Decisions

### Decision 1 — Single redacting handler in `internal/log`

A new `internal/log` package owns one `slog.Handler` implementation (`redactHandler`) that wraps the JSON handler from `internal/obs`. Every component that must log PII-adjacent data imports `internal/log.New(...)` instead of building its own handler.

The allowlist of case-insensitive keys whose values are always replaced with `[REDACTED]`:

```
password  passwd    pwd
token     jwt       refresh_token   access_token
api_key   authorization
cookie    set_cookie
secret    recovery_code  totp_secret  otp_seed
cpf       cnpj
phone     telefone
email     e_mail    mail
```

### Decision 2 — Struct tag `log:"redact"`

When a struct is logged as an `slog.Any` attribute, the handler inspects exported fields for the tag `log:"redact"`. Tagged fields are zeroed before the value is serialised. A per-type reflection cache amortises the cost.

Example:

```go
type loginAttempt struct {
    Username string
    Password string `log:"redact"`
    IP       string
}
```

### Decision 5 — Raw payloads prohibited below Error

Raw request/response bodies and webhook payloads MUST NOT be logged at `Info` or `Debug`. At `Error` level (incident post-mortem only), callers use `log.NewRawEvent(payload)` and `log.LogRawEvent(ctx, logger, msg, ev)`, which log only:

- `raw_event_id` — stable UUID for cross-system correlation
- `payload_sha256` — hex SHA-256 digest for tamper evidence

The raw bytes are never written to a log line.

### Decision 7 — CI scrubbing gate

`scripts/ci/log_scrubbing_test.go` runs the `internal/log` and `internal/obs` packages under `-v`, captures combined stdout+stderr, and asserts that no line matches:

- Common plaintext passwords (`correct horse battery staple`, `password123`, `p@ssw0rd`)
- JWT pattern (three base64url segments)
- CPF pattern
- Email or phone at `INFO`/`DEBUG` level

A match fails CI with the offending line number and content.

---

## Consequences

- **Positive:** PII and secrets cannot accidentally flow into logs via key-name collision or struct printing; F51 and F54 are closed.
- **Positive:** The reflection cache means repeated struct-type scrubs are O(1) after the first call.
- **Negative:** Any new sensitive field not in the allowlist requires either a tag or an allowlist addition — this is intentional, but requires developer awareness.
- **Negative:** Struct fields tagged `log:"redact"` are silently dropped with no visible marker in the JSON; callers should log a placeholder field if the presence/absence of the value matters for triage.

---

## Alternatives considered

- **Output-side grep scrubbing:** rejected — brittle against format changes and catches leaks too late.
- **Compile-time linter:** useful complement but insufficient alone; runtime values cannot be statically checked.
- **Full structured PII store with field-level access control:** out of scope for Phase 0; revisit when audit export is needed.
