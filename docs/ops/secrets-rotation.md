# Runbook — Secrets rotation

Owning task: [SIN-63189](/SIN/issues/SIN-63189) (Fase 6 PR7).
Cadence calendar: [`docs/ops/secret-rotation-schedule.md`](./secret-rotation-schedule.md).
Scripts: [`scripts/rotate-secret.sh`](../../scripts/rotate-secret.sh),
[`scripts/update-config-and-redeploy.sh`](../../scripts/update-config-and-redeploy.sh).

This page tells the on-call engineer how to rotate every secret the CRM
depends on without leaking the new value, without losing the previous
window of validity, and with every rotation event captured in the
`audit_log_security` ledger.

## Hard rules (apply to every rotation below)

1. **Never log a secret value.** The audit ledger stores `actor_user_id`,
   `secret_name`, and `phase` only — never the key material itself.
   `scripts/rotate-secret.sh` enforces this: every helper that touches
   plaintext key material is wrapped to write `***REDACTED***` into any
   log line or audit row payload.
2. **Two rotations are recorded per cycle:** `started` and `completed`.
   The validation step gates the `completed` row — if validation fails,
   the cycle remains `started` and a `failed` row is appended.
3. **Operator identity is required.** Set `CRM_OPS_ACTOR_USER_ID` to the
   UUID of the human running the rotation (look up in `users` via
   email). The script refuses to run with an empty actor.
4. **Old value lives until validation passes.** No "delete first" path.
   Every rotation provides a documented rollback that returns the
   previous value to active use within the validation window.
5. **Default cadence: 90 days** unless the secret-specific row below
   overrides it. The cadence calendar is the single source of truth for
   "next rotation date".
6. **Run rotations in a maintenance window** unless the secret-specific
   row promises zero-downtime. Hard-time windows let the on-call team
   roll back without paging users.

## Per-secret matrix

| Secret                                      | Default cadence | Owner            | Automated?            | Downtime           |
| ------------------------------------------- | --------------- | ---------------- | --------------------- | ------------------ |
| DB password — `app_runtime`                 | 90 days         | CTO              | yes (`rotate-secret`) | zero (dual role)   |
| DB password — `app_admin`                   | 90 days         | CTO              | yes (`rotate-secret`) | zero (offline use) |
| DB password — `app_master_ops`              | 90 days         | CTO              | yes (`rotate-secret`) | zero (dual role)   |
| OpenRouter API key                          | 90 days         | CTO              | semi (provider UI)    | ≤ 60 s (restart)   |
| PSP API key (Pagar.me)                      | 90 days         | CTO              | semi (provider UI)    | ≤ 60 s (restart)   |
| Slack webhook URL — alerts                  | 180 days        | CTO              | semi (provider UI)    | zero (worker hot-reads `.env`) |
| Campaign marker signing key (HMAC)          | 180 days        | CTO              | yes (`rotate-secret`) | dual-key 72 h      |
| Backup encryption key — see [SIN-62261](/SIN/issues/SIN-62261) | 365 days | CEO  | **manual only**       | offline vault swap |

Status icons:

- **yes**  — `scripts/rotate-secret.sh` runs the whole cycle.
- **semi** — script handles config update + redeploy + audit emission;
  the provider-side rotation step lives in the provider's UI.
- **manual** — the secret never lives in CI/CD; the runbook step
  references the offline vault procedure.

## Prerequisites

### Environment variables (operator workstation)

```sh
# Operator identity (UUID from users table — required for every rotation).
export CRM_OPS_ACTOR_USER_ID="<uuid>"

# Audit writer DSN. Connect as app_audit (INSERT-only on audit_log_security).
# Password is read from PGPASSWORD or pulled from the secret manager.
export CRM_AUDIT_DSN="postgres://app_audit@<host>:5432/crm?sslmode=require"
export PGPASSWORD="<app_audit password>"   # NEVER echoed by the script

# Path to the deploy config the rotation will update.
export CRM_DEPLOY_ENV_FILE="deploy/compose/.env"

# Optional override — defaults to ./scripts/rotate-secret.sh's built-in.
export CRM_REDEPLOY_CMD="docker compose -f deploy/compose/compose.yml up -d --no-deps app"
```

### Tools

| Tool         | Min version | Notes                                         |
| ------------ | ----------- | --------------------------------------------- |
| `bash`       | 4.0         | uses `set -euo pipefail`                      |
| `psql`       | 14          | for `ALTER ROLE` + audit INSERT               |
| `openssl`    | 3.0         | CSPRNG via `openssl rand`                     |
| `docker`     | 24.x        | only when the redeploy step runs locally       |
| `jq`         | 1.6         | audit payload composition                     |

### Vault references

Credentials are pulled from the 1Password vault `CRM — Secrets rotation`
(read-only role for operators; write-back on completion). Vault items
are named exactly after the `<name>` argument of `rotate-secret.sh` so
the operator never has to guess the mapping.

## 1. DB password — `app_runtime`

| Field           | Value                                                                    |
| --------------- | ------------------------------------------------------------------------ |
| `<name>` arg    | `db:app_runtime`                                                         |
| Config key      | `POSTGRES_PASSWORD` (compose) → `CRM_DB_PASSWORD` (per [ADR 0071](../../docs/adr/0071-postgres-roles.md)) |
| Cadence         | 90 days                                                                  |
| Owner           | CTO                                                                      |
| Downtime budget | zero (dual-role swap)                                                    |
| Rollback budget | 5 minutes back to previous role                                          |

### Procedure (zero-downtime dual-role)

```sh
scripts/rotate-secret.sh db:app_runtime
```

The script performs, in order:

1. **`started` audit row** — `event_type='key_rotation'`,
   `target={"secret":"db:app_runtime","phase":"started"}`.
2. **Generate** a 32-byte CSPRNG password via `openssl rand -base64 32`.
   The value is written to a `mode=0600` tempfile under `/dev/shm` (or
   `$TMPDIR` if `/dev/shm` is absent) and **never** echoed.
3. **`CREATE ROLE app_runtime_next LOGIN NOSUPERUSER NOCREATEDB
   NOCREATEROLE NOBYPASSRLS PASSWORD '…'`** via `psql` as
   `app_admin`. Privileges are reproduced from `app_runtime` via
   `pg_dump -s --no-owner --no-acl --section=pre-data` + the grants
   listed in [ADR 0071](../../docs/adr/0071-postgres-roles.md).
4. **Update** `${CRM_DEPLOY_ENV_FILE}` in place: `POSTGRES_USER` →
   `app_runtime_next`, `POSTGRES_PASSWORD` → new value. The previous
   file is preserved at `${CRM_DEPLOY_ENV_FILE}.prev` (`mode=0600`).
5. **Redeploy** the app container — `${CRM_REDEPLOY_CMD}`.
6. **Validate** that `curl -fsS http://app:8080/health` returns 200 AND
   a tenant-scoped `SELECT 1 FROM tenants LIMIT 1` succeeds as the new
   user. Validation timeout: 60 s, polled every 2 s.
7. **`DROP ROLE app_runtime`** and **`ALTER ROLE app_runtime_next
   RENAME TO app_runtime`** in a single transaction.
8. **`completed` audit row** — same `secret_name`, `phase="completed"`,
   `previous_role_dropped=true`.

### Rollback

If validation fails between steps 6 and 7:

```sh
scripts/rotate-secret.sh db:app_runtime --rollback
```

This restores `${CRM_DEPLOY_ENV_FILE}.prev`, redeploys, and drops
`app_runtime_next`. The original `app_runtime` role was never touched
(its grants and password remain valid), so the rollback path has no
data-loss risk. A `failed` audit row is appended.

### Validation step

```sh
# As the new credentials (via psql) — must succeed.
psql "postgres://app_runtime@<host>:5432/crm?sslmode=require" -c "SELECT 1"

# From the app container — must return 200.
curl -fsS http://app:8080/health
```

## 2. DB password — `app_admin`

| Field           | Value                                                                |
| --------------- | -------------------------------------------------------------------- |
| `<name>` arg    | `db:app_admin`                                                       |
| Config key      | `CRM_MIGRATION_PASSWORD` (per [ADR 0071](../../docs/adr/0071-postgres-roles.md)) |
| Cadence         | 90 days                                                              |
| Owner           | CTO                                                                  |
| Downtime budget | zero (offline use — invoked only by the migration runner)            |

### Procedure (single-role, no dual-window needed)

`app_admin` is **not** held by any running app pod. It is read only by
the migration runner at deploy time. Rotation is therefore a plain
`ALTER ROLE` + secret-manager update:

```sh
scripts/rotate-secret.sh db:app_admin
```

The script:

1. **`started` audit row.**
2. Generates a new password (same CSPRNG path as `app_runtime`).
3. `ALTER ROLE app_admin PASSWORD '…'` as the cluster superuser. The
   superuser DSN comes from `CRM_SUPERUSER_DSN` (not `CRM_AUDIT_DSN`);
   the script refuses to run with the audit DSN to enforce role
   separation.
4. Writes the new password into the secret manager under
   `crm/db/app_admin/password` and stamps `rotated_at=<now>`.
5. **`completed` audit row.**

### Rollback

`ALTER ROLE app_admin PASSWORD '<previous>'` from the previous secret
manager version (kept for 30 days per the vault's retention policy).
Append a `failed` audit row.

### Validation step

```sh
# Migration smoke — must succeed.
PGPASSWORD="<new>" psql "postgres://app_admin@<host>:5432/crm?sslmode=require" \
  -c "SELECT current_user, has_table_privilege('audit_log_security','INSERT')"
```

## 3. DB password — `app_master_ops`

| Field           | Value                                                                  |
| --------------- | ---------------------------------------------------------------------- |
| `<name>` arg    | `db:app_master_ops`                                                    |
| Config key      | `MASTER_OPS_DATABASE_URL` password segment, `CRM_MASTER_OPS_PASSWORD`  |
| Cadence         | 90 days                                                                |
| Owner           | CTO                                                                    |
| Downtime budget | zero (dual-role swap, same shape as `app_runtime`)                     |

Same procedure as `app_runtime` (dual-role swap), with these
differences:

- The transition role is `app_master_ops_next`, with BYPASSRLS=true
  (the master ops posture, per ADR 0071).
- The redeploy command targets the master-ops console pod, not the app
  pod: `CRM_REDEPLOY_CMD` should be overridden accordingly.
- Validation also checks that the `master_ops_audit_trigger` still
  RAISES when `app.master_ops_actor_user_id` is unset (one fixture
  INSERT into any audit-bearing table; expect the failure).

## 4. OpenRouter API key

| Field           | Value                                                                 |
| --------------- | --------------------------------------------------------------------- |
| `<name>` arg    | `openrouter`                                                          |
| Config key      | `OPENROUTER_API_KEY`                                                  |
| Cadence         | 90 days                                                               |
| Owner           | CTO                                                                   |
| Downtime budget | ≤ 60 s (app restart picks up new env)                                 |

### Procedure (manual provider step + scripted redeploy)

OpenRouter does not expose an API for key rotation, so the operator
generates the new key in the OpenRouter dashboard first:

1. Sign in to <https://openrouter.ai/account> with the **shared ops**
   account (1Password → `CRM — OpenRouter ops`).
2. Under **API keys**, click **Create key**, name it `crm-prod-<YYYY-MM-DD>`,
   copy the value into the clipboard.

Then run the scripted half:

```sh
scripts/update-config-and-redeploy.sh openrouter
```

The script prompts for the new key on stdin (no echo), then:

1. **`started` audit row.**
2. Writes the new value into `${CRM_DEPLOY_ENV_FILE}` (preserving the
   previous file at `.prev`).
3. **Redeploys** the app via `${CRM_REDEPLOY_CMD}`.
4. **Validates** with `curl -fsS http://app:8080/health` AND a smoke
   OpenRouter completion against the configured model.
5. **`completed` audit row.**

The operator then revokes the OLD key in the OpenRouter dashboard.
This is a **manual** step the script reminds the operator to perform
on completion — it cannot be automated without an OpenRouter management
API.

### Rollback

`scripts/update-config-and-redeploy.sh openrouter --rollback` restores
`${CRM_DEPLOY_ENV_FILE}.prev` and redeploys. The OpenRouter dashboard
must also be reverted (re-enable the old key, delete the new one).

### Validation step

```sh
# Quick model call — must return 200 with a non-empty `choices` array.
curl -fsS -X POST https://openrouter.ai/api/v1/chat/completions \
  -H "Authorization: Bearer ${OPENROUTER_API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"openrouter/auto","messages":[{"role":"user","content":"ping"}]}' \
  | jq -e '.choices | length > 0'
```

## 5. PSP API key (Pagar.me)

| Field           | Value                                       |
| --------------- | ------------------------------------------- |
| `<name>` arg    | `pagarme`                                   |
| Config key      | `PAGARME_API_KEY`                           |
| Cadence         | 90 days                                     |
| Owner           | CTO                                         |
| Downtime budget | ≤ 60 s (app restart)                        |

Identical shape to the OpenRouter procedure: the operator generates a
new key in the Pagar.me dashboard, then runs:

```sh
scripts/update-config-and-redeploy.sh pagarme
```

The script's validation step issues a `GET /core/v5/recipients/me`
call (read-only, idempotent) to confirm the new key is accepted by the
provider before the audit `completed` row is written.

### Rollback

Same as OpenRouter: `--rollback` restores the previous env file and
redeploys. Revert the dashboard manually.

### Pagar.me-specific note

Pagar.me supports a "secondary key" slot per merchant account. The
operator SHOULD use the secondary slot for the rotation so the primary
key is never invalidated mid-rotation:

1. Generate the new key into the secondary slot.
2. Run the script — it writes the secondary into `PAGARME_API_KEY` and
   redeploys.
3. After 24 h with no PSP errors in the alerter dashboard, promote
   secondary → primary in the dashboard and re-run the script (so the
   new primary value lands in `.env`).

## 6. Slack webhook URL — alerts

| Field           | Value                                       |
| --------------- | ------------------------------------------- |
| `<name>` arg    | `slack-alerts`                              |
| Config key      | `SLACK_ALERTS_WEBHOOK_URL`                  |
| Cadence         | 180 days                                    |
| Owner           | CTO                                         |
| Downtime budget | zero (worker hot-reads `.env` on next event) |

### Procedure

1. In Slack, open the `#crm-alerts` channel → **Integrations →
   Incoming Webhooks → Add New Webhook**. Copy the resulting URL.
2. Run:

   ```sh
   scripts/update-config-and-redeploy.sh slack-alerts
   ```

3. The script writes the new URL into `${CRM_DEPLOY_ENV_FILE}` and
   redeploys ONLY the alerter worker (`docker compose up -d --no-deps
   wallet-alerter-worker`), so the rest of the stack is untouched.
4. Validation: fire a synthetic alert via
   `scripts/check-security-headers.sh --slack-canary` and confirm it
   lands in `#crm-alerts` within 60 s.
5. Delete the old webhook in the Slack admin UI.

### Rollback

Restore the previous webhook from `${CRM_DEPLOY_ENV_FILE}.prev` and
redeploy the alerter. The old URL must also be re-enabled in Slack
admin if it was already deleted (Slack keeps a 30-day grace window).

## 7. Campaign marker signing key (HMAC)

| Field           | Value                                                                                                    |
| --------------- | -------------------------------------------------------------------------------------------------------- |
| `<name>` arg    | `marker:campaigns`                                                                                       |
| Config keys     | `CAMPAIGNS_MARKER_SIGNING_KEY` (current) + `CAMPAIGNS_MARKER_SIGNING_KEY_PREVIOUS` (verify-only, 72 h)   |
| Cadence         | 180 days                                                                                                 |
| Owner           | CTO                                                                                                      |
| Downtime budget | zero (dual-key 72 h window)                                                                              |

> **Roadmap note.** The current production wire
> (`cmd/server/campaigns_public_wire.go`) reads only
> `CAMPAIGNS_MARKER_SIGNING_KEY`. Multi-key verification reads
> (`…_PREVIOUS`) are tracked under [SIN-62982](/SIN/issues/SIN-62982)
> follow-up. Until that lands, the dual-key window is **at most
> 60 s** (the redeploy interval) and the rotation MUST run during a
> maintenance window so the small window of in-flight signed markers
> can be tolerated as "verification miss → fall back to unsigned".

### Procedure (dual-key window, target shape)

```sh
scripts/rotate-secret.sh marker:campaigns
```

The script performs:

1. **`started` audit row** with `secret_name="marker:campaigns"`.
2. **Generate** a 32-byte CSPRNG key, base64-encode (RawStdEncoding).
3. **Write** the new key as `CAMPAIGNS_MARKER_SIGNING_KEY` and move the
   previous value to `CAMPAIGNS_MARKER_SIGNING_KEY_PREVIOUS` in
   `${CRM_DEPLOY_ENV_FILE}`.
4. **Redeploy** the app — signer now uses the NEW key; verifier accepts
   either.
5. **Schedule** a follow-up Paperclip ticket (assignee=CTO) for **72 h
   later** to drop the `_PREVIOUS` variable. The script prints the
   `gh issue create` command rather than calling Paperclip directly —
   the ticket is the dual-window's only liveness signal.
6. **`completed` audit row** with `dual_window_expires_at=<now>+72h`.

### Rollback (within the 72 h window)

Both keys are still valid for verification, so rollback is a single
swap: restore `${CRM_DEPLOY_ENV_FILE}.prev` (which has the old key as
primary) and redeploy. Append a `failed` audit row.

### Validation step

```sh
# Round-trip: sign a synthetic marker with the new key, verify with both.
go run ./cmd/marker-roundtrip-smoke \
  --primary "${CAMPAIGNS_MARKER_SIGNING_KEY}" \
  --previous "${CAMPAIGNS_MARKER_SIGNING_KEY_PREVIOUS:-}"
```

## 8. Backup encryption key

| Field           | Value                                                       |
| --------------- | ----------------------------------------------------------- |
| `<name>` arg    | (manual — script refuses)                                   |
| Config key      | n/a (key lives in offline vault per [SIN-62261](/SIN/issues/SIN-62261)) |
| Cadence         | 365 days                                                    |
| Owner           | CEO                                                         |
| Downtime budget | offline (vault swap; no live key in CI/CD)                  |

### Why this one is manual

[SIN-62261](/SIN/issues/SIN-62261) requires the backup encryption key
to live in a **cold, offline vault** that no agent and no CI runner
ever touches. Rotation therefore cannot be scripted — by design.

### Procedure (CEO-led ceremony)

1. CEO retrieves the offline vault hardware (Yubikey + sealed paper
   shard) from the safe.
2. Generates a new `age` recipient key offline (`age-keygen -o
   crm-backup-<YYYY-MM-DD>.key`).
3. Re-encrypts the next backup using the NEW key while keeping the
   PREVIOUS key in the recipient list for **30 days**, so the on-call
   engineer can still decrypt the last month of dumps if a restore
   drill catches a gap.
4. After 30 days with at least one successful restore drill against a
   dump encrypted with the new key, removes the previous key from the
   recipient list and shreds it.
5. CEO files the rotation by hand into `audit_log_security` via the
   master ops console — the only path that does not require a live
   agent to see the key:

   ```sql
   INSERT INTO audit_log_security
     (actor_user_id, event_type, target)
   VALUES
     ('<CEO user uuid>', 'key_rotation',
      '{"secret":"backup-encryption","phase":"completed",
        "ceremony_id":"<safe-receipt-number>"}');
   ```

### Validation step

The next quarterly restore drill ([SIN-63187](/SIN/issues/SIN-63187),
[`docs/ops/restore-drill-runbook.md`](./restore-drill-runbook.md))
exercises the new key end-to-end. A drill that succeeds with the new
key is the rotation's validation.

## Audit ledger queries (for the on-call)

All rotations land in `audit_log_security` with `event_type='key_rotation'`.
The `target` column carries a structured payload. Useful queries:

```sql
-- All rotations in the last 90 days, newest first.
SELECT occurred_at, actor_user_id,
       target->>'secret' AS secret,
       target->>'phase'  AS phase
FROM audit_log_security
WHERE event_type='key_rotation'
  AND occurred_at >= now() - interval '90 days'
ORDER BY occurred_at DESC;

-- Rotations started but never completed (in-flight or failed > 24h).
SELECT a.target->>'secret' AS secret, a.occurred_at AS started_at, a.actor_user_id
FROM audit_log_security a
WHERE a.event_type='key_rotation'
  AND a.target->>'phase'='started'
  AND a.occurred_at <= now() - interval '24 hours'
  AND NOT EXISTS (
    SELECT 1 FROM audit_log_security b
    WHERE b.event_type='key_rotation'
      AND b.target->>'secret'=a.target->>'secret'
      AND b.target->>'phase' IN ('completed','failed')
      AND b.occurred_at > a.occurred_at
  );
```

The second query is the source of truth for the "rotation stuck" alert
suggested in [`docs/ops/secret-rotation-schedule.md`](./secret-rotation-schedule.md).

## What goes wrong (and how to spot it)

| Symptom                                              | Likely cause                                                         | First action                                                  |
| ---------------------------------------------------- | -------------------------------------------------------------------- | ------------------------------------------------------------- |
| App container restart loops after step 5             | New password not committed in DB before redeploy (race)              | `--rollback` to restore old env; investigate DB connectivity. |
| `audit_log_security` INSERT denied                   | `app_audit` password rotated but `PGPASSWORD` is stale               | Re-fetch from secret manager; retry just step 1.              |
| OpenRouter/PSP/Slack call rejected after validation  | Provider key already disabled before the dashboard swap completed    | Generate one more new key, swap again, append a `failed` row. |
| `app_runtime_next` rename collision                  | Previous rotation aborted mid-flight, left the role behind           | `DROP ROLE app_runtime_next` manually, then re-run.           |
| Marker signing key validation fails                  | Multi-key reader not deployed yet (see roadmap note in section 7)    | Roll back; raise the multi-key follow-up before re-trying.    |
