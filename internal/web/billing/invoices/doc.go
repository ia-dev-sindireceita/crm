// Package invoices is the HTMX UI for the per-tenant PIX-invoice
// surface (SIN-62963, Fase 4 follow-up to SIN-62956 / SIN-62957).
// Server-renders the tenant's invoice list, the per-invoice detail
// page with the embedded PIX BR Code (qr_code + copia-e-cola), and a
// status partial that HTMX polls every 10 s while the charge is
// pending. A small dunning-banner partial mirrors the subscription's
// dunning state (warn / suspended_outbound / suspended_full) at the
// top of every page in the surface and is exposed standalone at
// /billing/dunning-banner for other pages to hx-get.
//
// All routes require RequireAction(iam.ActionTenantBillingView)
// (RoleTenantGerente in the production matrix) — same envelope as
// the master billing console.
//
// The package owns the read ports it needs (InvoiceLister,
// InvoiceGetter, PIXChargeLister, DunningStateReader); concrete
// adapters live alongside the existing billing/dunning/pix postgres
// stores. The PIX postgres adapter lands in SIN-62958 / C7; until
// then the wire injects a "no-charge" implementation and the detail
// page renders a "cobrança em processamento" placeholder.
//
// Defense in depth: the banner is a UI affordance only — the
// server-side dunning gate (C14) is the authoritative block on
// outbound traffic and read-only mode. See ADR-0100.
package invoices
