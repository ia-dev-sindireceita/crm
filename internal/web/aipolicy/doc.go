// Package aipolicy is the HTMX admin UI for AI policy configuration
// (SIN-62906 / Fase 3 W4A, child of SIN-62196). It mounts under
// /settings/ai-policy and serves the per-scope (tenant/team/channel)
// configuration screens that drive the cascade resolver in
// internal/aipolicy (SIN-62351 / W2A).
//
// Routes:
//
//   - GET    /settings/ai-policy
//     Full page: lists every policy row for the current tenant
//     (ordered by scope_type, scope_id) plus the cascade preview
//     widget. Read access is gated by the same RBAC action as write
//     (tenant.ai_policy.write) — the page only renders for admins
//     who could mutate the configuration anyway.
//
//   - GET    /settings/ai-policy/new
//     HTMX partial form for creating a new policy. ?scope= and
//     ?scope_id= seed the inputs.
//
//   - GET    /settings/ai-policy/{scope_type}/{scope_id}/edit
//     HTMX partial form pre-populated with the existing row.
//
//   - GET    /settings/ai-policy/preview
//     HTMX partial: given ?team_id= and ?channel_id= query params,
//     runs the resolver against the current tenant and returns a
//     small card showing which row applied (channel/team/tenant/
//     default) plus the resolved model/tone/language plus the
//     ai_enabled toggle state. AC #1, #2, #3.
//
//   - POST   /settings/ai-policy
//     Upserts the policy keyed by (scope_type, scope_id) from the
//     form. Validation: model ∈ {gemini-flash, claude-haiku}, tone
//     ∈ {neutro, formal, casual}, language ∈ {pt-BR, en-US, es-ES},
//     scope_type ∈ {tenant, team, channel}, scope_id non-blank.
//     ai_enabled / anonymize / opt_in are bools; the form omitting
//     a checkbox sends nothing → false; anonymize defaults to true
//     when the form did not render it at all (deny-by-default per
//     ADR-0041).
//
//   - PATCH  /settings/ai-policy/{scope_type}/{scope_id}
//     Same validation as POST but the URL pins the scope identity,
//     so a form that tries to rename the scope is rejected. The
//     PATCH body carries the new column values.
//
//   - DELETE /settings/ai-policy/{scope_type}/{scope_id}
//     Removes the row. Cascade falls through to the parent scope.
//     Returns the refreshed table partial so HTMX can swap the row
//     out and update the table footer count.
//
// URL design: the description in the parent issue used
// `/settings/ai-policy/:id` for PATCH and DELETE. The domain port
// (aipolicy.Repository) keys rows by the natural composite
// (tenant, scope_type, scope_id) and does not surface the synthetic
// uuid PK, so this package uses the natural key in the URL too. The
// behavior matches what the description asked for; only the URL
// shape differs.
//
// Security envelope (composed by the router):
//
//   - middleware.TenantScope resolves the tenant from the host.
//   - middleware.Auth + RequireAuth attach iam.Principal.
//   - CSRF middleware short-circuits on GET/HEAD/OPTIONS; POST,
//     PATCH, DELETE consult the session token.
//   - middleware.RequireAction(iam.ActionTenantAIPolicyWrite) gates
//     every method. Read access is gated on the same action so the
//     admin who cannot mutate also cannot inspect — a deliberate
//     simplification for W4A.
//   - csp.Middleware emits the per-request nonce; the templates
//     here ship zero <script>/<style> blocks so the nonce is unused
//     but the page complies with the strict CSP envelope from
//     SIN-62237.
//
// SafeText: every dynamic value reaches the templates via
// html/template which auto-escapes in HTML / attribute / URL
// contexts. No template uses template.HTML or template.JS to bypass
// escaping.
package aipolicy
