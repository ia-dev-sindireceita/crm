# Cookies — inventory, security attributes, and LGPD analysis

> **Source:** [SIN-62983](/SIN/issues/SIN-62983) LOW-2 (follow-up of [SIN-62959](/SIN/issues/SIN-62959) Fase 4 review).
> **Scope:** every cookie this CRM emits and the per-cookie LGPD posture.
> **Owner of the next review:** Fase 6 LGPD work in [SIN-62199](/SIN/issues/SIN-62199).

This document is the authoritative inventory of cookies the CRM sets at the public + tenanted surfaces, why each cookie exists, the security attributes it carries, and the LGPD classification (qualification, base legal, retention proportionality). It is referenced from the privacy notice rendered to data subjects and from operator runbooks.

## Inventory

| Cookie | Set by | Purpose | Path | Domain | Secure | HttpOnly | SameSite | TTL | LGPD class |
|---|---|---|---|---|---|---|---|---|---|
| `__Host-sess-tenant` | `internal/adapter/httpapi/sessioncookie` (auth middleware) | Carries the per-tenant session id for authenticated operators. | `/` | _unset_ | yes | yes | Lax | operator session lifetime | Personal data (Art. 5° I) |
| `__Host-crm_click_id` | `internal/web/public/campaign` (public redirect handler) | Browser-supplied idempotency token used by the campaign click ledger so a reload / double-tap never inflates the per-browser counter. | `/` | _unset_ | yes | yes | Lax | 90 days | Personal data (Art. 5° I) |
| `_csrf` | `internal/adapter/transport/http/customdomain` (CSRF middleware on the custom-domain surface) | Anti-CSRF random token for the custom-domain widget POSTs. | `/` | _unset_ | yes | yes | Lax | per-request (rotation on use) | Not personal data (random token, no subject linkage) |

The `__Host-` prefix on the two long-lived cookies (`__Host-sess-tenant`, `__Host-crm_click_id`) is enforced by the browser per RFC 6265bis §4.1.3.2: the cookie is refused unless it is Secure, has `Path=/`, and carries no `Domain` attribute. This is belt-and-braces against a future refactor that quietly scopes a session or attribution cookie to a parent domain. Local HTTP dev (`CAMPAIGNS_PUBLIC_COOKIE_INSECURE=1`) will see the prefixed cookie rejected by the browser; the campaign-click idempotency path is a documented degraded mode in that wiring.

## `__Host-crm_click_id` — LGPD analysis

### 1. Qualification under Art. 5° I

The cookie value is a v4 UUID minted server-side and stored as `campaign_clicks.click_id` (UNIQUE per `(tenant_id, click_id)`). It is a **persistent, per-browser identifier**:

- It is **not** an account identifier (no `user_id`, no contact name, no email).
- It **does** become a personal identifier the moment the contact's first inbound WhatsApp / Telegram message arrives carrying the same `[crm:<click_id>.<hmac>]` marker (SIN-62982). At that point `internal/inbox/usecase.linkContactToCampaign` joins the click row to a `contacts.id`, and the row becomes data about an identified natural person.

By the **ANPD's broad reading** of "dado pessoal" (Art. 5° I), a browser identifier that can be linked to a contact through a foreseeable downstream operation **is personal data from the moment it is set**, even before the link is realised. We classify `__Host-crm_click_id` as personal data and apply the same LGPD posture as if the cookie carried a name.

### 2. Base legal de tratamento (Art. 7°)

We treat the cookie under **legítimo interesse** (Art. 7° IX). The grounds are:

- The treatment is **necessary for a service contracted by the operator** (the marketer running the campaign) — without per-browser idempotency, click attribution is wrong by construction.
- The cookie is **scoped tightly**: HttpOnly (no JavaScript access), SameSite=Lax (no cross-site bleed), Path=/ + no Domain (no sibling-host reuse), 90-day cap (see §3 below).
- The data subject's expectations are **proportional**: clicking a marketing link signed `[crm:…]` carries an implicit attribution intent; the marker is visible in the inbound message body the contact sends, so the treatment is not covert.

This is **not consent** (Art. 7° I) — clicking a campaign link cannot be construed as informed, free, unequivocal consent. Opt-in banners would be theatre. The privacy notice (rendered from `internal/legal/dpa.md`, SIN-62354) discloses the cookie family and links here for the per-cookie detail.

### 3. Retention proportionality — 90-day TTL

The 90-day TTL was set at SIN-62959 design-time on the rationale: "a contact who clicks a campaign on day 1 still attributes the message they send on day 30; short enough that a stale shared device eventually rolls." After SIN-62983 LOW-2, we record the proportionality check explicitly:

- **Median attribution window** (campaign click → first inbound). Empirical data is not yet available from production (Fase 4 is new). The pre-launch estimate was P50 ≤ 48 h, P95 ≤ 14 days.
- **Tail traffic.** The 30 → 90 day tail catches re-engagement campaigns ("Black Friday → Q1 retargeting"). The marketing team flagged this as the load-bearing reason.
- **Decision.** Keep 90 days as the launch default. Add a Fase 6 follow-up under [SIN-62199](/SIN/issues/SIN-62199) to:
  - measure the realised attribution-window P95 from `campaign_clicks.created_at` → `contacts.linked_at`;
  - if realised P95 ≤ 60 days, cut TTL to 60 days;
  - if realised P95 ≤ 30 days, cut TTL to 30 days.

The TTL is encoded in code at `internal/web/public/campaign/handler.go:CookieMaxAge` (constant). Any future change is a one-constant edit + a docs-update here.

### 4. Data subject rights

| Right | How we honour it |
|---|---|
| Access (Art. 9°) | The `click_id` is browser-side only until link-time. After link-time, the row is reachable via the contact's tenanted view; the operator's data-subject-request flow surfaces it together with the rest of the contact profile. |
| Anonymisation (Art. 16° IV) | Clearing the cookie ends the per-browser identity; pre-link rows are not attributable to a natural person and are retained for tenant analytics. |
| Deletion (Art. 18° VI) | A data-subject deletion request triggers a cascade on `contacts` that includes their linked `campaign_clicks` (FK with `ON DELETE CASCADE`). |
| Objection (Art. 18° §2°) | The privacy notice exposes a "don't track me" mechanism per tenant (Fase 6, [SIN-62199](/SIN/issues/SIN-62199)). Until that ships, contacts who object can clear their browser cookies; we do not have a mechanism that re-issues a click_id against a stated objection. This is a known gap tracked under [SIN-62199](/SIN/issues/SIN-62199). |

## Operator-visible consent UI

The tenant-facing privacy / DPA surface lives at `internal/legal/dpa.md` (SIN-62354) and is rendered at `GET /settings/privacy`. The current revision discloses the cookie family in narrative form. A per-cookie inventory rendered from this document is part of the Fase 6 LGPD work in [SIN-62199](/SIN/issues/SIN-62199).

There is **no opt-in banner** for `__Host-crm_click_id`. The Brazilian LGPD does not mandate cookie banners the way the EU ePrivacy Directive does; treating campaign attribution under legítimo interesse with a clear privacy notice is the conservative posture against the ANPD's published guidance.

## Change log

| Date | Change | Source |
|---|---|---|
| 2026-05-17 | Document created. Inventory + LGPD analysis for `__Host-crm_click_id`. 90-day TTL kept; revisit gated by Fase 6. | [SIN-62983](/SIN/issues/SIN-62983) LOW-2 |
