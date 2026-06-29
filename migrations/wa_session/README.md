# WhatsApp session (whatsmeow) database migrations

These migrations target the **dedicated** WhatsApp-session Postgres pointed at
by `WA_SESSION_DATABASE_URL` (ADR 0107 D3), **not** the app database. They are
kept in their own golang-migrate source directory so the app's
`migrate -path migrations` run never applies them to the app DB and vice-versa.

See `docs/adr/0108-wa-session-credential-at-rest.md` for the decision and
`docs/deploy/staging.md` §5g for the operator runbook.

## Roles (`0001_wa_session_roles`)

Creates two least-privilege roles on the WA session cluster:

| Role                 | Privileges                                              | Used by |
|----------------------|---------------------------------------------------------|---------|
| `wa_session_admin`   | `USAGE`+`CREATE` on schema; owns/`ALTER`/`DROP` `whatsmeow_*`. Runs the whatsmeow `Upgrade` (DDL). | one-shot deploy step |
| `wa_session_runtime` | `USAGE` on schema; `SELECT/INSERT/UPDATE/DELETE` on `whatsmeow_*` only. **No DDL.** | the app boot DSN (`WA_SESSION_DATABASE_URL`) |

`CREATE ROLE` is superuser-only and cluster-scoped, so this migration must be
applied with a **superuser** DSN on the WA session cluster.

## Sequences (`0002_wa_session_runtime_sequences`)

Extends the runtime grant to **sequences** (`USAGE`+`SELECT`, never `UPDATE`,
never DDL). Today's whatsmeow schema uses natural/composite keys and has no
`SERIAL`/`IDENTITY` columns, so this grants nothing yet — it is a
secure-by-default guard so a **future** whatsmeow bump that introduces a
`SERIAL`/`IDENTITY` column does not break the runtime's first `INSERT` (which
would otherwise fail `42501` calling `nextval()` on a sequence it has no
`USAGE` on). Scoped exactly like the `0001` table grant: default privileges on
sequences `wa_session_admin` creates, plus a `whatsmeow_`-prefixed grant loop
for any that pre-exist. Apply with the same superuser DSN as `0001`
(`migrate up` runs both, in order).

## Deploy order (per environment / per whatsmeow version bump)

1. **Roles** — apply this migration as a superuser:
   ```bash
   migrate -path migrations/wa_session -database "$WA_SESSION_SUPERUSER_DATABASE_URL" up
   ```
2. **Passwords** — ops sets a login password on each role (never in the
   migration); see `docs/deploy/staging.md` §5g.
3. **Schema `Upgrade` as admin** — run the whatsmeow `sqlstore` `Upgrade` once,
   connected as `wa_session_admin`, to create/upgrade the `whatsmeow_*` tables.
   Because the up migration sets `ALTER DEFAULT PRIVILEGES FOR ROLE
   wa_session_admin`, the newly created tables auto-grant DML to
   `wa_session_runtime` — no second grant pass is needed.

   **Sequences on a schema bump:** if a new whatsmeow version introduces a
   `SERIAL`/`IDENTITY` column (it creates an owned sequence), `0002` already
   default-grants `USAGE`+`SELECT` on sequences `wa_session_admin` creates, so
   the runtime's `INSERT` keeps working with no extra step. The only case that
   needs a manual grant is a sequence created **outside** `wa_session_admin`'s
   default-privilege scope (e.g. restored by another role, or not
   `whatsmeow_`-prefixed) — for that, run as a superuser:
   ```sql
   GRANT USAGE, SELECT ON SEQUENCE <seq> TO wa_session_runtime;
   ```
   Do **not** grant `UPDATE` (setval) or any DDL — the runtime never needs
   them, and sequence DDL stays an admin-only deploy step.
4. **App boot as runtime** — the app connects via `WA_SESSION_DATABASE_URL`
   using `wa_session_runtime`. On an already-current schema the library's boot
   `Upgrade` is a read-only no-op (it issues no DDL); see ADR-0108 "Boot
   behaviour under the runtime role".

> Step 3 MUST precede booting an app build that carries a newer whatsmeow
> schema version: a pending schema upgrade is DDL, which `wa_session_runtime`
> is intentionally not allowed to run. Running the admin `Upgrade` first keeps
> the boot path DDL-free.
