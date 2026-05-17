// Package campaign is the public, unauthenticated redirect endpoint for
// per-tenant campaign short links (SIN-62959, Fase 4 child of
// SIN-62197). It serves GET /c/{slug}: a marketer hands a contact a
// link like https://acme.crm.example/c/blackfriday-2026, the contact
// clicks, we persist a CampaignClick row keyed on the browser cookie,
// and 302-redirect to the campaign's configured redirect_url (typically
// a wa.me/<number>?text=… deep-link that ferries the contact into the
// CRM inbox).
//
// Secure-by-default exception. AC #1 requires the endpoint to be
// reachable without authentication — that is the whole point. The
// compensating controls are:
//
//  1. Tenant scope by Host. The tenanted chi group resolves the request
//     Host to a Tenant before this handler runs (middleware.TenantScope);
//     a request that hits an unknown host renders the generic 404 from
//     the middleware and never reaches the handler. This stops
//     cross-tenant slug enumeration.
//  2. Idempotent click ledger. The cookie __Host-crm_click_id (httpOnly,
//     SameSite=Lax, Secure, Path=/, no Domain, 90d) carries the
//     browser-supplied idempotency token; the storage adapter dedups
//     on click_id so a reload / double-tap never inflates the counter.
//     AC #2. See docs/security/cookies.md for the LGPD analysis behind
//     the 90-day TTL.
//  3. Per-IP rate limit. The wire wraps the handler in the
//     httpapi/ratelimit middleware with a single bucket (default
//     100/min/IP, env CAMPAIGNS_PUBLIC_CLICK_RATE_PER_MIN). AC #4.
//  4. Open-redirect / SSRF guard. The campaign's redirect_url is
//     validated at create-time (scheme http/https) AND re-validated at
//     click-time against an allowlist of safe hosts (CAMPAIGNS_REDIRECT_ALLOWED_HOSTS,
//     comma-separated). Out-of-allowlist URLs render 502 Bad Gateway
//     and emit a structured log entry so operators see the abuse
//     attempt. AC #7.
//  5. Lifecycle gate. A campaign whose ExpiresAt has passed serves
//     410 Gone rather than 302 so a stale link cannot exfiltrate
//     traffic indefinitely.
//  6. Best-effort bot detection. User-Agent matching tags clicks with
//     meta.bot=true so the dashboard (C11) can filter automated
//     traffic without dropping the row.
//
// Wiring lives in cmd/server/campaigns_public_wire.go. Tests in this
// package use the campaigns.InMemoryRepository fake (documented
// in-memory adapter — Quality bar rule 5).
package campaign
