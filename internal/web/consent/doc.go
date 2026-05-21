// Package consent serves the LGPD cookie consent banner
// (SIN-63191 / Fase 6 PR4 AC #4).
//
// Two endpoints, both server-rendered (HTMX-over-SPA lens):
//
//   - GET  /consent/cookies-banner
//     Returns the banner partial when no decision cookie is set,
//     otherwise returns 204 No Content. Embedded into authenticated
//     layouts and into the public /privacy page via hx-get.
//
//   - POST /consent/cookies
//     Body: form-encoded `decision=accept|decline`. Sets the
//     `__Host-crm_consent_v1` cookie (1y TTL) and, when an iam.Principal
//     is on the context, additionally records the decision via
//     ConsentRegistry (purpose `cookies_analytics`) so the audit row
//     persists outside the cookie jar. Anonymous visitors get a
//     cookie-only experience (the registry needs a Subject).
//
// The banner is keyboard-accessible (no JS needed to operate the
// form), uses ARIA roles for screen readers, and degrades cleanly
// when JS is off: the POST hits the same handler whether HTMX swaps
// the response or the browser performs a regular form submit.
package consent
