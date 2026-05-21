# ADR 0102 — Tenant 2FA: TOTP + single-use recovery codes; admin-mandatory, member-opt-in

- Status: Accepted
- Date: 2026-05-21
- Deciders: CTO
- Drives: [SIN-63188](/SIN/issues/SIN-63188) (this ADR — Fase 6 PR6 doc gate)
- Ratifies: implementation shipped in [SIN-63184](/SIN/issues/SIN-63184) (Fase 6 PR1, merged in PR #224, commit `0f4f716`)
- Builds on: [ADR 0073](./0073-csrf-and-session.md) §D3 (session rotation triggers — 2FA success swaps pre-MFA for post-MFA session-id), [ADR 0074](./0074-master-mfa-phase0.md) (master operator MFA — already shipped in Fase 0)
- Lenses: **Defense in depth**, **Secure-by-default API**, **Reversibility & blast radius**

> **Numbering note.** The original Fase 6 PR6 task ([SIN-63188](/SIN/issues/SIN-63188))
> referenced `ADR-0070`. That slot was already taken by
> [ADR 0070](./0070-password-hashing.md) (Argon2id parameters, Fase 0). Per the
> [README](./README.md) rule "numbers are permanent — supersedes/amendments
> live inside the affected ADR, never via renumbering", this ADR takes the
> next free index above [ADR 0101](./0101-consent-registry-generic-lgpd.md).

## Context

Phase 6 promotes the platform from "operator MFA only" (ADR 0074, master
console) to "tenant 2FA across the admin/atendente plane".  The
[Phase-0 security review](/SIN/issues/SIN-62220#document-security-review)
flagged **F15** (MEDIUM) — tenant operators authenticate with password
only.  A compromised tenant credential exposes every WhatsApp message,
contact PII, and outbound channel cost ledger for that tenant. That
risk does not match the master-console threat model (master accounts
move money and impersonate); it does match the threshold for
"unilateral tenant lockout is a worse customer outcome than a stolen
password", so 2FA on **admins** is mandatory and on **members** is
opt-in.

Two trade-offs forced the shape of the decision:

- **MFA factor.** TOTP (RFC 6238) is the floor: it works on any
  smartphone with a free authenticator app, has no SMS-fraud
  exposure, no SIM-swap risk, no carrier dependency, and crucially no
  per-tenant onboarding (an atendente buys a $0 app, scans a QR, done).
  WebAuthn is strictly better cryptographically but requires
  hardware enrolment that small operators do not have on day one.
  Result: TOTP now, WebAuthn as an *additional* factor when ADR 010X
  ships.
- **Recovery.** TOTP secrets are stored on a single device. Phone-lost
  is a non-trivial fraction of all auth incidents (Apple ID transfer
  failures, factory resets, second-hand phones). Without recovery
  codes the only recourse is master-impersonation, which is exactly
  the audit-noisy event we are trying to avoid for routine
  customer-success. Single-use recovery codes (10 codes, Argon2id at
  rest, individually consumed) move recovery from a master-ops ticket
  to a user-self-service flow.

## Decision

Adopt **TOTP + single-use recovery codes** for tenant 2FA.

### D1 — Factor: TOTP only on the tenant plane (Fase 6)

A 32-byte CSPRNG seed is generated server-side at enrolment time. The
seed and a per-tenant pepper produce a 160-bit OTPAuth URI rendered
into a QR code in the enrolment view.  The URI is **never** logged or
stored in plaintext server-side: the seed is encrypted at rest with
the secrets-rotation key (ADR 0106) and decrypted only inside the
verify code-path. Time skew tolerated: ±1 step (≈ ±30s) per RFC 6238.
Replay window: a verified counter must strictly exceed the
last-accepted counter for the user (the `iam.user_mfa.last_seen_step`
column).

WebAuthn is **deliberately out of scope for Fase 6** — adding it now
would force three branches in every login template (TOTP, WebAuthn,
recovery) and triple the QA surface for the same threat model.
WebAuthn enrolment for master/admin lands in a follow-up ADR once
hardware-key issuance is in place.

### D2 — Mandatory for admins; opt-in for members; bypass attempts audited

Migration 0107_user_mfa enforces the role gate at the DB layer:

```
ALTER TABLE users
  ADD COLUMN totp_required_at timestamptz;
```

`totp_required_at IS NOT NULL` means the user MUST present a verified
TOTP step on every login. The login flow sets this column on first
admin elevation; demoting an admin does NOT clear it (an admin who
was promoted has secrets on the device — leaving the gate up is a
fail-closed default).

For members, enrolment is offered but not mandated. A member with
`totp_required_at IS NULL` and no enrolled secret completes a normal
password login; a member with an enrolment lands on the verify step
identically to an admin.

**Bypass attempts** (an authenticated session that lacks the
post-MFA flag tries to reach a guarded endpoint) are persisted to
`audit_log_security` as `event_type='2fa_required'` with target
columns `{path, session_id, has_session}`. This produces a forensic
trail for the on-call without leaking the absent-second-factor
condition to the caller (the handler returns the standard
redirect-to-verify response).

### D3 — Recovery codes: 10 per user, Argon2id at rest, single-use

On a successful first enrolment the server mints **10 base32**
recovery codes (8 chars each, displayed once in groups of 4 for
read-aloud). Each code is hashed with Argon2id (ADR 0070 parameters)
and stored as one row per code in `user_mfa_recovery_codes`:

```
( user_id uuid, code_hash bytea, consumed_at timestamptz NULL )
```

On verify, the server first checks the candidate against the TOTP
step; if that fails, it scans the not-yet-consumed recovery hashes
for the user and Argon2id-verifies in constant-time. A successful
recovery-code use:

1. Sets `consumed_at = now()` on the matching row (idempotent under
   the `(user_id, code_hash)` UNIQUE constraint).
2. Writes `audit_log_security` `event_type='2fa_recovery_used'` with
   target `{session_id, remaining_codes}`.
3. Sets the user's `reenroll_required = TRUE` flag — the next
   authenticated request is intercepted by the enrolment view, the
   user is forced to regenerate (D4) before any business action
   continues.

The `reenroll_required` gate is intentional: a single recovery code
in the wild indicates "either the user lost the device or someone
else owned the codes". Either way, regenerating the codeset is a
cheap re-establishment of trust.

### D4 — Regeneration invalidates the prior set atomically

`POST /admin/2fa/recovery/regenerate` performs the rotation in one
transaction:

```
BEGIN;
DELETE FROM user_mfa_recovery_codes WHERE user_id = $1;
INSERT INTO user_mfa_recovery_codes (user_id, code_hash, consumed_at)
  VALUES …;   -- 10 fresh hashes
UPDATE users SET reenroll_required = FALSE WHERE id = $1;
INSERT INTO audit_log_security (…, event_type, target)
  VALUES (…, '2fa_recovery_regenerated', '{"old_count": …}');
COMMIT;
```

There is no soft-replace or grace window. A regeneration always
moves the user to "no codes from the previous batch are valid",
matching the user-mental-model of "I just got new codes; the old
slip is paper trash".

### D5 — Reversibility

Every step is independently togglable so a regression can be
contained without a deploy:

- `iam.user_mfa.totp_required_at` is a per-user column — reverting a
  single admin to password-only is one row-update on an audited
  master query (master-ops only, RLS-enforced).
- `auth.mfa.enforced` config key (boolean, default `true`) flips the
  whole gate off in emergency; an `auth.mfa.enforced=false` deploy is
  a paged on-call event and the audit log records the config write.
- The recovery-code table is independent of the seed table; nuking
  the recovery row for a user (master-ops query) does **not**
  invalidate the TOTP seed.

## Consequences

**Positive.**

- F15 closed. Admin compromise now requires both the password and
  the TOTP device (or a recovery code, which itself audits and
  forces re-enrolment).
- Boring tech: stdlib `crypto/rand`, `pquerna/otp` (one external
  dep, well-maintained, no vendoring required), Argon2id (already
  in the build per ADR 0070).
- The master plane keeps its **stricter** MFA gate (ADR 0074) —
  this ADR does NOT relax it. Tenant operators see the friendlier
  TOTP-only flow; master operators continue to see the post-Fase-0
  hardware-key roadmap.

**Negative / costs.**

- Per-user state machine grows from `{authenticated, anonymous}` to
  `{anonymous, pre-mfa, post-mfa, recovery-pending}`. Templ helpers
  in the authenticated layout test the post-mfa flag explicitly;
  the unit tests for the layout enforce the conditional.
- Lost-phone-and-lost-codes is still a master-ops support ticket.
  We accept this because the legitimate-failure rate is much lower
  than the "lost-phone-but-has-codes" rate, and the master-ops flow
  audits exactly what we want it to.
- TOTP secrets-at-rest become a backup correctness criterion (ADR
  0104 §3.2). The drill exercises `user_mfa` table restore.

**Residual risks (accepted).**

- TOTP is replayable inside a 30s window if the attacker has the
  device AND the password AND can race the verify step. The per-step
  monotonic counter (D1) defeats the race on a single second; an
  attacker who replays must skew the device clock, which leaves
  observable jitter in the access log.
- Recovery codes printed by the user are off-platform. We do not
  mandate an on-screen confirmation that they were stored.
  Mitigation: the first-login banner after enrolment nags until the
  user clicks "I saved them" — soft control, accepted.

## Alternatives considered

- **WebAuthn-mandatory.** Strictly safer but requires hardware
  rollout we cannot underwrite in Fase 6. Deferred to a future ADR
  once the master tenant has confirmed it can issue YubiKeys to
  every tenant admin.
- **SMS OTP.** Rejected outright — SIM-swap fraud and carrier-cost
  exposure unacceptable.
- **Email OTP.** Rejected — email is the password-reset channel; an
  attacker with email access already has the account. Email OTP
  would offer false assurance.
- **No recovery codes; master-ops resets only.** Rejected as the
  default flow because every routine lost-phone becomes a master-
  ops ticket. Master-ops escalation remains the fall-back when both
  device and codes are lost.

## What this ADR does **not** decide

- WebAuthn enrolment and verify mechanics (separate ADR, post-Fase-6).
- Master operator 2FA (already decided in [ADR 0074](./0074-master-mfa-phase0.md)).
- The TOTP seed encryption key rotation cadence — covered by
  [ADR 0106](./0106-secrets-rotation-runbook.md) §3 (`master_secret`
  rotation drill).
