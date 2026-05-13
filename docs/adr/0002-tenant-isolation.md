# ADR 0002 — Tenant isolation: principle and `tenants` table carve-out

- Status: Accepted
- Date: 2026-05-07
- Related: [ADR 0071](0071-postgres-roles.md), [ADR 0072](0072-rls-policies.md),
  [ADR 0080](0080-uploads.md), SIN-62209, SIN-62214, SIN-62305

## Principle

Defense in depth at the DB: every tenanted table has `tenant_id NOT
NULL`, RLS `FORCE`-enabled, and the four-policy template `TO
app_runtime`. The app sets `SET LOCAL app.tenant_id` per request via
`WithTenant`; missing GUC fails closed. See ADRs 0071/0072.

## Bootstrap exception: `tenants` table

`tenants(id, name, host, created_at)` maps an incoming HTTP host to a
`tenant_id`. The scope can only be set **after** we know the tenant —
chicken-and-egg. `migrations/0004_create_tenant.up.sql` `GRANT SELECT
ON tenants TO app_runtime`; `tenants` is **not** tenant-scoped (no
`tenant_id`, no RLS).

This does not break defense in depth: the columns are public-by-design
(`host` is the client-sent `Host` header, not a secret; `id`/`name`
appear in URLs once resolved), no tenant data lives in `tenants`, RLS
on every other tenanted table remains the gate (knowing a `tenant_id`
does not read its rows), and writes to `tenants` are reserved for
`app_master_ops` (audited per ADR 0071) — `app_runtime` has SELECT only.

## Host-enumeration mitigations

A `host → tenant` lookup is a timing oracle. Three layers close it:

- **TTL cache** (`internal/tenancy/cache.go`): positive AND negative
  results cached 5 min — probes don't reach DB.
- **Generic 404** in the tenant middleware: fixed body, `nosniff`,
  same shape as a known-but-unrouted path.
- **Login timing-equalize** (SIN-62305): dummy bcrypt verify on
  unknown-host login paths so wall-clock matches wrong-password.

## `notenant` linter carve-out

`tools/lint/notenant` forbids direct `*pgxpool.Pool.{Exec,Query,
QueryRow,SendBatch,CopyFrom}` under `internal/`. Exempt prefixes
(`analyzer.go` `allowedPkgPrefixes`):
`github.com/pericles-luz/crm/internal/adapter/db/postgres` — owns
`WithTenant`/`WithMasterOps` and the bootstrap host resolver. New
exemptions require updating this ADR.
