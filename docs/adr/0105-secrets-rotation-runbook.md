# ADR 0105 — Secrets rotation runbook: cadence, key inventory, dual-recipient gating, automated drill

- Status: Accepted
- Date: 2026-05-21
- Deciders: CTO, SecurityEngineer
- Drives: [SIN-63188](/SIN/issues/SIN-63188) (this ADR — Fase 6 PR6 doc gate), [SIN-63189](/SIN/issues/SIN-63189) (Fase 6 PR7 — runbook + helper script implementation)
- Builds on: [ADR 0084](./0084-supply-chain.md) §secrets (recipient files, ownership), [ADR 0104](./0104-backup-restore-rpo-rto-drill.md) §D3 (two-recipient age rotation), [ADR 0073](./0073-csrf-and-session.md) §D1 (CSRF cookie does not require a rotation — token rotation is per-session, separate from this ADR)
- Lenses: **Defense in depth**, **Reversibility & blast radius**, **Operational excellence**

> **Numbering note.** The original Fase 6 PR6 task ([SIN-63188](/SIN/issues/SIN-63188))
> referenced `ADR-0073`. That slot was already taken by [ADR 0073](./0073-csrf-and-session.md)
> (Fase 0 CSRF + session cookies). This ADR takes the next free index
> above [ADR 0104](./0104-backup-restore-rpo-rto-drill.md).

## Context

The platform holds at least seven distinct secret classes, each with
a different blast radius, lifetime, and rotation owner. Without a
fixed runbook the rotations skew: a key meant for monthly rollover
gets rotated yearly because "we don't want to break the backup",
and an emergency rotation (suspected compromise) takes a day
because the operator has to assemble the procedure from scratch.

The [Phase-0 security review](/SIN/issues/SIN-62220#document-security-review)
F58 (MEDIUM) flagged "no documented rotation cadence". This ADR
records the cadence, the per-secret procedure (helper script), and
the gating rule that prevents a rotation from breaking the
backup/restore chain (ADR 0104 D3 — dual-recipient encrypt).

## Decision

### D1 — Secret inventory and cadence

| Secret class                          | Where it lives                                      | Cadence  | Rotation owner          | Drill?  |
|---------------------------------------|-----------------------------------------------------|----------|-------------------------|---------|
| Postgres app passwords (`app_runtime`, `app_master_ops`, `app_admin`) | k8s `Secret`, mounted as env on the app pods       | 30 days  | CTO                     | Yes     |
| Backup age-recipient public key       | `/etc/agnu/backup-recipients` on the backup host    | 30 days  | CTO (custody)           | Yes (ADR 0104 §D4) |
| Master 2FA seed encryption key (`master_secret`) | k8s `Secret`, mounted on app + worker pods   | 90 days  | CTO                     | Yes     |
| OpenRouter API key                    | k8s `Secret`, mounted on AI worker only             | 90 days  | CTO                     | Yes     |
| PSP (PIX provider) API key            | k8s `Secret`, mounted on billing worker only        | 90 days  | CTO + Billing operator  | Yes     |
| S3 vault credentials                  | k8s `Secret`, mounted on backup unit only           | 180 days | CTO                     | Yes     |
| Webhook signing keys (per-tenant)     | Postgres `tenants.webhook_signing_key`              | 365 days | Tenant operator (self)  | No      |

"Cadence" is the **target** rotation interval. A rotation may
happen **earlier** at any time — suspected compromise, departing
operator with access, vendor breach notification. A rotation
that misses its cadence by more than 50 % (e.g. 45 days for a
30-day key) is a paged event.

### D2 — Helper script: `scripts/rotate-secret.sh` (single entry point)

[SIN-63189](/SIN/issues/SIN-63189) ships one script with a fixed
contract:

```
scripts/rotate-secret.sh <secret-name>
```

Recognised secret names (mirrored from D1):

```
db-app-runtime
db-app-master-ops
db-app-admin
backup-age
master-secret
openrouter
psp
s3-vault
webhook-signing-key --tenant <id>
```

For each name the script enforces the SAME six-step skeleton (the
specifics vary by secret, but the structure is invariant):

1. **Pre-flight assertions.** Confirm the operator has the right
   role (CTO for everything except `webhook-signing-key`); confirm
   the platform is **not** mid-drill (ADR 0104 §D4 — a drill
   running on the secret being rotated invalidates its artifact).
2. **Generate the new secret.** Server-side, never from the
   operator's clipboard. For Postgres: `openssl rand 32 | base32`.
   For age: `age-keygen` on the master's offline machine. For
   API keys: vendor portal, copy-paste behind a constant-time
   one-shot input.
3. **Append-not-replace.** Where the secret is read by multiple
   consumers, the new value is APPENDED to the recipients/
   credential file (or the k8s Secret carries `current` and
   `previous`), never overwriting. ADR 0104 §D3 is the precedent
   — two recipients in flight.
4. **Cutover.** Rolling restart of the consumers (`kubectl rollout
   restart`) so the new value is loaded. The OLD value remains
   accepted for a documented window (varies — 7 days for backup
   recipients, 24 hours for `master_secret`).
5. **Retire the old value.** After the window, remove the
   previous entry. Backup artifacts older than the retire date
   are decrypted with the **archived** key (D3) on a drill.
6. **Audit + post-rotation drill.** Append a row to
   `audit_log_security` `event_type='key_rotation'` with target
   `{secret_name, previous_kid, new_kid, retire_after}`. Trigger
   the smallest drill for the affected secret class (D5).

Every step writes to stdout AND to
`/var/log/agnu/secrets-rotation/<timestamp>.log` (owned
`root:agnu-audit`, mode `0640`); the log is shipped to the
secure ledger by the journald-to-S3 pipeline (ADR 0084).

### D3 — Append-not-replace gating rule

The runbook's hardest invariant: rotation MUST never put the
platform into a state where a recent backup cannot be decrypted.
Concretely:

- **Backup age recipient** (ADR 0104 §D3) — append, run nightly
  backup with two recipients for one week, then retire. The
  retire window MUST include a successful drill (§D5).
- **Master secret** — the `master_secret` k8s Secret carries
  `current` and `previous` keys; the app code decrypts trying
  `current` first, falling back to `previous`. The "retire"
  step waits until no row in the database is still encrypted
  with `previous`; the wallet adapter's audit decorator emits
  a Prometheus counter per (kid) so the operator can verify
  "previous" is no longer in use before retiring.
- **DB passwords** — Postgres supports multiple roles; the
  rotation creates a NEW role with the new password, swings the
  pods to it, then drops the old role. A `psql --role=…` smoke
  is part of step 4.
- **API keys to vendors that only accept one key** (OpenRouter,
  some PSPs) — these are the **hard mode**: the rotation is an
  atomic swap with a known small consumer-visible window
  (< 30s rolling restart). The runbook MUST schedule these in
  business hours and the on-call MUST be paged-into-readiness
  before step 4.

### D4 — Emergency rotation (suspected compromise)

The runbook supports a `--emergency` flag that:

1. **Suspends step 3 (append).** For an emergency, the OLD value
   must stop being trusted immediately. This means a brief
   outage for any consumer that still holds the OLD value;
   acceptable because the alternative is a confirmed-leaked key
   continuing to authenticate.
2. **Forces a fresh drill (§D5) before the runbook exits.** An
   emergency rotation that succeeds at swapping the key but
   leaves the new value un-drilled is worse than no rotation —
   we have no proof the new key works for restore.
3. **Pages SecurityEngineer + CTO** synchronously; the emergency
   log line carries the requesting operator's identity.

Emergency rotations are intentionally rare and intentionally
disruptive. The audit_log_security row carries
`target={mode:'emergency'}` so the post-mortem template can
find every recent emergency rotation.

### D5 — Per-secret drill, lightweight, runs after every rotation

The full backup/restore drill (ADR 0104 §D4) is quarterly. Every
*rotation* triggers a **smaller** drill that exercises just the
rotated secret:

| Secret class      | Drill                                                                                                                   |
|-------------------|-------------------------------------------------------------------------------------------------------------------------|
| Postgres password | `pg_isready -U <new-role>` from inside one pod; `SELECT 1 FROM pg_roles WHERE rolname = '<old-role>'` confirms retirement|
| Backup age key    | Encrypt-decrypt a 1 KB sentinel with the new recipient; assert byte-equality                                            |
| `master_secret`   | `wallet.Pricer.Encrypt(probe)` with new kid; `wallet.Pricer.Decrypt(probe)` succeeds                                    |
| OpenRouter        | `POST /chat/completions` with a one-token probe; 200 OK + non-empty body                                                |
| PSP               | `POST /pix/qr-static` for R$ 0.01 to a known sentinel account; deletes the QR after asserting 201                       |
| S3 vault          | `aws s3 cp /dev/null s3://$VAULT/sentinel-$STAMP` + cleanup                                                             |

Each drill writes its own `restore-drill-{rotation}.json` artifact
to the same S3 prefix as the quarterly drill (ADR 0104 §D4), so
the operator has one place to audit "did the rotation actually
work end-to-end".

## Consequences

**Positive.**

- F58 closed. Rotation cadence is documented per secret, the
  helper script makes the steps idempotent, the drill catches
  mistakes before they reach the next quarterly window.
- The "append-not-replace" rule means a rotation cannot
  silently break the backup chain — the precondition for D3 is
  "two recipients live", and the retire step is gated on a
  drill artifact.
- Emergency rotations are a documented, audited, paged path —
  not an ad-hoc set of `kubectl edit secret` commands.

**Negative / costs.**

- A rotation involves 4–6 wall-clock days for non-emergency
  secrets (3-day in-flight period + drill + retire). Acceptable
  — the platform is not single-tenant SaaS that needs hourly
  rotations.
- Helper script lives in `scripts/rotate-secret.sh` — a code
  path that must be tested in CI but is not run on every PR.
  Mitigation: `scripts/rotate-secret.sh --dry-run <name>`
  exercises every branch without touching real keys and is
  part of the supply-chain CI lane (ADR 0084).
- Per-tenant `webhook-signing-key` rotation is operator
  self-service (D1) — the master cannot rotate these unilaterally
  without breaking the operator's downstream consumers. Operators
  may neglect this. Accepted; the cadence is "yearly" which is
  generous on purpose.

**Residual risks (accepted).**

- A vendor key (OpenRouter, PSP) that the vendor revokes
  out-of-band leaves us with a key that *we think* is current
  but is actually dead. Mitigation: the per-rotation drill (D5)
  is exercised at rotation time but not continuously; a future
  ADR may add a heartbeat probe. Today, the vendor-side breakage
  surfaces as an alert from the AI/billing worker — acceptable
  because those workers fail loud.
- The runbook does not cover keys held by master-impersonating
  helpers (e.g. a bridge that connects to upstream). Those
  helpers live outside this repository; their rotation is the
  bridge owner's job. The audit_log_security row from D2 §6 is
  the cross-domain glue.

## Alternatives considered

- **HashiCorp Vault for everything.** Rejected — adds a single
  point of failure (a Vault outage breaks every secret at once)
  and a new operational surface we cannot afford to staff. k8s
  Secrets + a rotation script is the boring-tech floor.
- **Auto-rotation cron job.** Rejected as the default — auto-
  rotation of vendor keys (OpenRouter, PSP) requires vendor
  API support we cannot rely on; auto-rotation of master_secret
  without a human-in-the-loop drill courts the failure mode
  "we automated the rotation and forgot to automate the drill".
- **Per-secret runbook files.** Considered. Rejected because
  the procedural skeleton is identical across secrets (D2 §1-6);
  separate files would drift. One ADR + one script is the
  smaller surface.

## What this ADR does **not** decide

- The Postgres role grants themselves — covered by
  [ADR 0083](./0083-app-runtime-vs-master-ops.md) and ADR 0071.
- The CSRF cookie rotation (per-session, not per-cadence) —
  covered by [ADR 0073](./0073-csrf-and-session.md) §D1.
- The TOTP seed rotation per user — that is a user-initiated
  re-enrolment flow covered by [ADR 0102](./0102-2fa-totp-recovery-codes.md)
  §D3/D4.
- The webhook-signing-key rotation API for tenants — UI/contract
  detail for a separate Fase 6 follow-up.
- Vendor-specific key lifecycles (OpenRouter token expiry, PSP
  certificate ladders) — referenced from the helper script's
  per-secret branch, not duplicated here.
