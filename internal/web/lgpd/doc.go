// Package lgpd is the HTTP adapter for the LGPD data-subject surface
// (SIN-63186 / Fase 6 PR3). It mounts two endpoints under /admin/lgpd:
//
//   - GET /admin/lgpd/export?contact_id=...
//     Streams a ZIP (data.json + data.csv) of every personal datum
//     the tenant holds about the contact.
//
//   - POST /admin/lgpd/delete
//     Body: {"contact_id":"...", "justification":"..."}
//     Persists a deletion request, schedules the contact's data for
//     anonymisation, and returns the queued request descriptor as
//     JSON. Idempotent: a repeat POST while a pending row exists
//     updates that row instead of creating a second one.
//
// Security envelope (composed by the router):
//
//   - middleware.TenantScope resolves the tenant from the host.
//   - middleware.Auth + RequireAuth attach iam.Principal.
//   - CSRF middleware short-circuits GET; POST consults the session
//     token.
//   - middleware.RequireAction(ActionTenantLGPDExport /
//     ActionTenantLGPDDelete) gates the methods. The matrix in
//     internal/iam restricts both actions to RoleTenantGerente; under
//     master impersonation the same gate runs with the PII step-up
//     bit checked.
//   - The handler writes one audit_log_data row per request through
//     iam/audit.SplitLogger; the row carries contact_id, actor, IP,
//     and event_type ∈ {lgpd_export, lgpd_forget} (AC #6).
//
// Rate limiting (AC #7): the handler relies on the per-route limiter
// the router wires onto the /admin/lgpd group. The policy used is
// "lgpd_admin" (10/min/tenant); construction of that policy is the
// caller's responsibility — the handler stays unaware so unit tests
// can drive the same shape without standing up a limiter.
package lgpd
