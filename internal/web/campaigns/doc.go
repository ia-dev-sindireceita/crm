// Package campaigns is the HTMX admin UI for the marketing-campaign
// dashboard (SIN-62962, Fase 4 follow-up to SIN-62954). Server-renders
// the per-tenant list of campaigns with rolled-up click and
// attribution counters, plus a detail drill-down that polls the
// click ledger every 10 seconds via hx-trigger so newly-recorded
// clicks (C8 webhook ingest) surface within the 30-second AC budget.
//
// The package follows the same pattern as internal/web/funnel and
// internal/web/catalog (SIN-62862 / SIN-62907): html/template, no JS
// framework, partial swaps via hx-* attributes. The single bit of
// vanilla JS lives in /static/js/campaigns.js and only handles the
// "copy link" affordance — CSP-safe (no inline JS).
//
// All routes require RequireAction(iam.ActionTenantCampaignManage),
// which the production matrix restricts to RoleTenantGerente; the
// router gate mirrors the W4C catalog admin envelope.
package campaigns
