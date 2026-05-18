// Package campaigns holds the marketing-campaign bounded context:
// per-tenant UTM-tagged short links and a click ledger that feeds
// downstream funnel triggers and reporting.
//
// The package is the domain core: it imports neither database/sql nor
// pgx, neither net/http nor any vendor SDK. Storage lives behind
// Repository; the clock is injectable on aggregate construction.
//
// Two aggregates live here:
//
//   - Campaign: the per-tenant short link a marketer creates. Owns
//     slug normalization, UTM defaults, and the IsExpired predicate.
//   - CampaignClick: one row in the click ledger. ClickID is the
//     browser-supplied idempotency token (a page reload that re-fires
//     the redirect handler is a no-op).
//
// The package is intentionally tenant-strict: every constructor and
// every port method takes a tenantID, and uuid.Nil collapses to a
// typed error rather than a row leak (RLS in 0102 plus belt-and-
// braces validation here).
//
// SIN-62954 (Fase 4 internal/campaigns, child of SIN-62197). The
// 0102_phase4_marketing_billing_dunning migration ships the underlying
// tables; this package wraps them with a hexagonal repository port
// and a pgx adapter under internal/adapter/db/postgres/campaigns.
package campaigns
