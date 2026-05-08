# Environment configuration

This page lists the environment variables `cmd/server` reads at
startup. Defaults assume single-replica production wired to one
Postgres + one Redis. Local development can omit anything except
`HTTP_ADDR`; `cmd/server` falls back to a health-only mode when
`DATABASE_URL` is unset (see `cmd/server/main.go::execute`).

## Quick reference

| Variable             | Required (prod) | Default      | Where read                                                  |
|----------------------|-----------------|--------------|-------------------------------------------------------------|
| `HTTP_ADDR`          | no              | `:8080`      | `cmd/server/main.go`                                        |
| `DATABASE_URL`       | yes             | unset → health-only mode | `internal/adapter/db/postgres/pool.go::NewFromEnv` |
| `REDIS_URL`          | yes             | unset → boot fails | `cmd/server/wire.go::openRedis`                       |
| `SESSION_TTL`        | no              | 24h          | `internal/iam/session.go::ParseSessionTTL`                  |
| `SLACK_WEBHOOK_URL`  | no              | empty (no-op)| `cmd/server/wire.go::assembleDeps`                          |

## SLACK_WEBHOOK_URL

The Slack incoming-webhook the **master** account-lockout flow posts
to when a master operator's account is locked
([SIN-62341](../../README.md), ADR 0073 §D4, acceptance criterion #3).

* **Empty value** — `slack.New("")` returns a Notifier whose `Notify`
  is a silent no-op. The master `iam.Service` is wired with this
  Notifier unconditionally, so missing the webhook does not change
  the lockout behaviour: the row is still written to
  `account_lockout`, the user still sees `429 + Retry-After`. Only
  the side-channel notification disappears.
* **Set value** — must be a valid Slack incoming-webhook URL
  (`https://hooks.slack.com/services/...`). The adapter posts a JSON
  payload `{"text": "account locked: policy=m_login user=… until=…"}`
  with a 5-second per-call deadline. Non-2xx responses are logged
  but do **not** roll back the lockout (the `account_lockout` row is
  the authoritative penalty).
* **Scope** — only the master `iam.Service` carries the Notifier.
  The tenant Service leaves `Alerter` nil so the tenant-policy
  threshold trips never page anyone (`AlertOnLock=false` on the
  tenant `login` policy in `internal/iam/ratelimit/policy.go`).
* **Reload** — the value is captured at process start. Rotating
  the webhook requires a process restart (graceful: `kill -TERM`
  then re-launch). SIGHUP-based reload is not implemented; if you
  need it, file a follow-up ticket. The adapter is per-process state,
  not per-request, so a restart is safe between deploys.
* **Secret handling** — never commit a real webhook URL. Inject
  via the deploy environment (`docker-compose.stg.yml`, Caddy env
  block, or Kubernetes secret). gitleaks (`.gitleaks.toml`) blocks
  accidental commits.

## Other variables

* **`DATABASE_URL`** — Postgres DSN. MUST point at the `app_runtime`
  role in production (`NOBYPASSRLS`). Example:
  `postgres://app_runtime:***@db.internal:5432/crm?sslmode=verify-full`.
  See `docs/adr/0071-postgres-roles.md` for the full role design.

* **`REDIS_URL`** — Redis DSN. Example: `redis://redis.internal:6379/0`.
  Used by the rate-limit sliding-window adapter and (separately) the
  custom-domain enrollment quota. The two share one client.

* **`SESSION_TTL`** — Go duration (`24h`, `1h30m`, etc.). Default 24h.
  Sets the session expiry cmd/server stamps onto new sessions; the
  validator rejects requests carrying an expired session.

* **`HTTP_ADDR`** — listen address. Default `:8080`. Tests free a
  port via `freePort(t)` and pass it through.
