# ADR 0092 — CI Postgres credentials & the test-bootstrap race

- Status: accepted
- Date: 2026-05-15
- Deciders: CTO, Coder
- Tickets:
  [SIN-62752](/SIN/issues/SIN-62752) (this ADR — postmortem + convention),
  [SIN-62750](/SIN/issues/SIN-62750) (acute fix that relocated wallet adapter
  tests to the parent `postgres_test` package),
  [SIN-62726](/SIN/issues/SIN-62726) (same regression pattern, contacts
  adapter — established the precedent),
  [SIN-62728](/SIN/issues/SIN-62728) (PR thread on which the failing CI
  signal was first surfaced)
- Related: [ADR 0071](./0071-postgres-roles.md) (Postgres roles), `migrations/0001_roles.up.sql`,
  `internal/adapter/db/postgres/testpg/testpg.go`,
  `scripts/check-postgres-adapter-tests.sh`

## Context

For two days every CI run on `ia-dev-sindireceita/crm` failed inside
`go test` with:

```
failed SASL auth: FATAL: password authentication failed for user "app_admin"
(SQLSTATE 28P01)
```

The failure first surfaced on PR #80 ([SIN-62725](/SIN/issues/SIN-62725) wallet
migration) and then trailed every subsequent PR — including PR #82
([SIN-62728](/SIN/issues/SIN-62728), Authorizer) which was clean code blocked
behind red CI.

The original triage hypothesis ([SIN-62752](/SIN/issues/SIN-62752)
description) was that a GitHub Actions secret had drifted out of sync with
the `app_admin` role password baked into a migration. That hypothesis was
wrong: no Postgres role password is ever set by a migration (see
[ADR 0071 — Credential injection](./0071-postgres-roles.md#credential-injection))
and the CI workflow's only Postgres credential is a literal,
non-secret superuser password (`ci-superuser`) used to bring the
`postgres:16-alpine` service container up. There is nothing to drift.

The real cause was a race condition in how the test harness bootstraps role
passwords against the shared CI Postgres cluster, and the fix already
landed in [SIN-62750](/SIN/issues/SIN-62750). This ADR documents the
credential model so the diagnosis is durable, the convention that prevents
recurrence, and what is explicitly *not* needed (no secret rotation, no
workflow change).

## The credential model in CI

1. **Postgres service container.** `.github/workflows/ci.yml` brings up
   `postgres:16-alpine` with `POSTGRES_USER=postgres` and
   `POSTGRES_PASSWORD=ci-superuser`. The password is literal and visible
   in source on purpose — the container is reachable only from the
   workflow's job network and torn down at the end of the run. Treating
   it as a secret would add rotation noise without changing the threat
   model.

2. **`TEST_DATABASE_URL`.** The same workflow exports
   `TEST_DATABASE_URL="host=127.0.0.1 port=5432 user=postgres
   password=ci-superuser dbname=postgres sslmode=disable"`. Test packages
   that need real Postgres consume this DSN through
   `internal/adapter/db/postgres/testpg`.

3. **`testpg` bootstrap.** When `TEST_DATABASE_URL` is set,
   `testpg.Start()` attaches to the cluster (rather than spawning its
   own ephemeral `pg_ctl` cluster) and, **as the cluster superuser**:
   - applies `migrations/0001_roles.up.sql` once (idempotent), which
     creates `app_runtime`, `app_admin`, `app_master_ops` with **no
     password** — exactly the posture
     [ADR 0071](./0071-postgres-roles.md#credential-injection)
     requires;
   - runs `ALTER ROLE <role> WITH PASSWORD '<runtimePassword>'` for
     each role, where `runtimePassword` is a **per-process** value
     generated when the test binary is loaded.

4. **Per-test pools.** `Harness.DB(t)` then opens `pgxpool` connections
   as `app_runtime`, `app_admin`, `app_master_ops`, and the superuser,
   each using the same per-process `runtimePassword`. Tests read/write
   through these pools.

No GitHub Actions secret is involved. No production credential is
involved. The `runtimePassword` never leaves the test binary's memory
and is regenerated on every CI run.

## The race that broke main

`go test -race -count=1 ./...` launches one test binary **per Go
package**. Each binary runs its own `TestMain`, which calls
`testpg.Start()`, which calls
`ALTER ROLE app_admin WITH PASSWORD '<this-binary's-runtimePassword>'`
against the shared CI cluster.

When two test binaries are scheduled concurrently — for example, the
parent `postgres_test` package and a subpackage `wallet_test` that both
import `testpg` — the sequence is:

1. Binary A: `ALTER ROLE app_admin WITH PASSWORD 'test_AAAA'`.
2. Binary A opens its pool with password `test_AAAA`, runs some tests.
3. Binary B: `ALTER ROLE app_admin WITH PASSWORD 'test_BBBB'`.
4. Binary A's pool needs a new connection (pool growth, ping after
   idle, new test acquiring a fresh conn) — Postgres now expects
   `test_BBBB`, the pool still presents `test_AAAA`, and the auth
   handshake fails with SQLSTATE 28P01.

Whichever binary lost the ALTER race fails non-deterministically, but
on a single shared cluster the failure converges fast: once any
subpackage adds itself to the race the parent package's tests start
flaking the moment the subpackage's TestMain finishes its ALTER.

`ALTER ROLE` is global, not database-scoped — there is no namespace
inside one cluster that would let two binaries hold different
`app_admin` passwords at the same time.

This is exactly the regression
[SIN-62726](/SIN/issues/SIN-62726) caught in `contacts` and
[SIN-62750](/SIN/issues/SIN-62750) caught (after PR #80 merged) in
`wallet`. Local single-package runs and ephemeral-cluster runs
(`TEST_DATABASE_URL` unset) hide it because there is only one binary
talking to that cluster.

## Decision

The CI Postgres credential model stays as-is — workflow injects a
literal superuser password, `testpg` derives role passwords
per-process from the superuser DSN, migrations never store
passwords — because the model itself is correct. The defect was
multiple test binaries sharing one cluster, not credential drift.

Two durable rules govern future work:

### Rule 1 — All Postgres adapter tests live in the parent `postgres_test` package

Adapter code may live in a subpackage of `internal/adapter/db/postgres/`
(e.g. `internal/adapter/db/postgres/wallet/`), but its tests **must**
declare `package postgres_test` and live directly under
`internal/adapter/db/postgres/`. Examples:

- `internal/adapter/db/postgres/contacts_adapter_test.go`
  (SIN-62726, commit `7d9cf39`)
- `internal/adapter/db/postgres/wallet_adapter_test.go`
  (SIN-62750, commit `4993a3f`)

The parent package has one `TestMain` that calls `testpg.Start()`
exactly once per `go test` invocation, regardless of how many adapters
it tests. No second binary, no race.

### Rule 2 — CI lints for Rule 1

`scripts/check-postgres-adapter-tests.sh` is run from the `lint` job
in `.github/workflows/ci.yml` and fails the build if any
`*_test.go` file is found under a **subpackage** of
`internal/adapter/db/postgres/`. The script is `find -mindepth 2
-name '*_test.go'`, so the parent-package files are allowed and any
subpackage-level test file is blocked.

The CTO authorized this guard in
[SIN-62750](/SIN/issues/SIN-62750); it is now baseline CI. New adapter
PRs that violate it fail before reaching the `test` job.

### Non-rules (explicitly rejected)

- **Do not rotate `POSTGRES_PASSWORD` in CI.** It is `ci-superuser` on
  purpose and is not a secret.
- **Do not move any role password into a migration.** ADR 0071
  forbids it; the `0001_roles.up.sql` block intentionally does not
  set passwords.
- **Do not add a GitHub Actions repository secret named
  `POSTGRES_PASSWORD` / `APP_ADMIN_PASSWORD` / etc. for tests.**
  Production secrets live in the secret manager (ADR 0071 table);
  CI uses an ephemeral cluster password and per-process test
  passwords. Mixing the two would couple test runs to secret
  rotation cadence, which is the exact failure mode the original
  SIN-62752 hypothesis assumed already existed (it didn't, and we
  don't want it to).
- **Do not give every test binary its own ephemeral `pg_ctl`
  cluster.** That escape hatch exists (`TEST_DATABASE_URL` unset)
  and is useful locally, but it would cost ~3–4× CI wall-clock and
  require `pg_ctl`/`initdb` in the runner image. Rule 1 keeps the
  shared-cluster model fast and safe.

## Consequences

Positive:

- Future SASL 28P01 failures in CI are not a credential-drift problem
  to chase through GitHub secrets — they are either (a) a Rule 1
  violation (caught by Rule 2 before merge), or (b) a genuine
  Postgres regression worth real triage. The Rule 2 lint failure
  message names the precedent files, so the fix recipe is on-screen.
- ADR 0071's credential-injection model is preserved end-to-end: no
  password in migration SQL, no password in workflow secrets, no
  password in any committed file beyond the literal CI-only
  `ci-superuser`.
- PR #82 ([SIN-62728](/SIN/issues/SIN-62728)) and every subsequent
  PR sees green CI on the wallet/contacts/inbox adapters without
  any per-PR mitigation. (Verified: the five most recent `ci.yml`
  runs on the fork — including PR #82 — are green at the time of
  writing.)

Negative / costs:

- Engineers adding a brand-new adapter under
  `internal/adapter/db/postgres/<new>` cannot keep its tests next to
  its code in a subpackage. They must place tests in the parent
  directory as `package postgres_test`. The Rule 2 lint message
  spells this out, but it is a non-obvious convention coming from
  most Go codebases that allow subpackage tests freely.
- The shared CI cluster is a single point of contention. A future
  test that needs cluster-level isolation (e.g. exercising
  `pg_hba.conf` or `pg_authid` mutations beyond ALTER ROLE …
  PASSWORD) will need to spawn its own ephemeral cluster
  explicitly. This ADR does not address that path; the workflow
  steps for it would be added in a follow-up ADR if and when the
  need arises.

## Rollback

If Rule 1 / Rule 2 turn out to be too restrictive (e.g. an adapter
genuinely needs internal-only helpers that cannot be exposed across a
package boundary even via `internal/`), the rollback is:

1. Move the relevant adapter and its tests into their **own
   top-level package**, outside `internal/adapter/db/postgres/`, so
   `scripts/check-postgres-adapter-tests.sh`'s `find` does not see
   them.
2. Give that package its own `TestMain` that **does not** call
   `testpg.Start()` — instead spawn an ephemeral cluster via the
   existing `testpg` ephemeral path (`TEST_DATABASE_URL` unset).
3. Document the exception in a follow-up ADR pointing back here.

Rolling back to "multiple test binaries share one cluster's
`ALTER ROLE`" is **not** a supported path — the race is
deterministic on any cluster with more than one binary calling
`testpg.Start()`.

## Out of scope

- Production secret rotation. Owned by ops; ADR 0071 covers the
  three runtime roles' secret sources.
- Per-test database isolation. Already handled by
  `Harness.DB(t)`'s per-test `CREATE DATABASE` + `DROP DATABASE`
  cycle; not affected by this ADR.
- Anything about
  [`scripts/check-postgres-adapter-tests.sh`](../../scripts/check-postgres-adapter-tests.sh)
  beyond the existence of the lint and its location in the `lint`
  job — script-level details (find flags, error message format)
  belong in the script's own header comment, which is already
  written.
