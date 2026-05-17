// Package rules is the HTMX editor for the per-tenant funnel-rules
// surface (SIN-62961, Fase 4). It mounts under /funnel/rules and lets
// the gerente role of a tenant create, edit, toggle, and delete the
// trigger→action rules that drive the cascade resolver delivered by
// SIN-62955 (internal/funnel/rules).
//
// Architecture (Ports & Adapters):
//
//   - Domain port: [rules.RuleAdminRepository] from
//     internal/funnel/rules. The handler depends on the port
//     interface, NOT on the pgx adapter — tests pass an in-memory
//     fake (the same [rules.InMemoryRepository] the cascade resolver
//     uses, which also satisfies the admin port).
//   - Cascade preview re-uses [rules.Resolver] from the same domain
//     package so the editor surfaces exactly the rule the production
//     auto-handoff would fire on a hypothetical event.
//
// Authorization is handled at the router level — the routes are
// mounted behind RequireAuth + RequireAction(ActionTenantFunnelRuleManage)
// so the handler itself can assume an authenticated, gerente principal
// with a tenant in context. Tenant scoping flows through tenancy.FromContext
// the same way every other tenant-scoped surface uses it.
//
// HTMX patterns used:
//
//   - List view at GET /funnel/rules is a full-page shell whose table
//     body is its own partial; create/update/delete responses swap that
//     partial back inline without a full reload (AC #1).
//   - The create/edit form select for trigger_type/action_type fires
//     hx-get on /funnel/rules/trigger-fields and /funnel/rules/action-fields
//     so the per-type input set re-renders as the user picks a value
//     (no JS state machine, no SPA).
//   - The enabled toggle is a button with hx-patch on
//     /funnel/rules/{id}/toggle that swaps the row back inline.
//   - The cascade preview panel HTMX-gets /funnel/rules/preview with
//     channel and team_id as query params; the response is the same
//     resolver output an inbound event would receive (AC #2).
//
// No JS framework is added (AC #5). The shared htmx.min.js vendor file
// and the existing /static/css/* stylesheets carry every interaction.
package rules
