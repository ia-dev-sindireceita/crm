// Package pix is the pgx-backed adapter for the PIX domain ports
// declared in internal/billing/pix:
//
//   - EventLog   — the webhook_events idempotency ledger (migration
//     0102). Lives outside RLS by design: webhooks arrive before the
//     authenticated tenant is known. Adapter writes route through
//     WithMasterOps so the master_ops_audit trigger fires on every
//     INSERT.
//   - Repository — pix_charges. Tenant-scoped via RLS for reads;
//     writes also route through WithMasterOps because that role owns
//     INSERT/UPDATE/DELETE on the table per migration 0102 grants.
//
// SIN-62964 (Fase 4 Inter webhook receiver).
package pix
