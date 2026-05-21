// Package usermfa implements the HTMX-driven tenant 2FA flow added by
// SIN-63184 (Fase 6 PR1). It owns the four user-facing routes:
//
//	GET  /admin/2fa/setup          — render the "begin enrolment" page
//	POST /admin/2fa/setup          — Enroll: mint seed + 10 recovery
//	                                 codes, render the one-shot recovery
//	                                 code download + QR + secret.
//	GET  /admin/2fa/verify         — render the verify form.
//	POST /admin/2fa/verify         — Verify TOTP code (or recovery
//	                                 code); on success delete the
//	                                 pending row, mint the real tenant
//	                                 session cookie, redirect to next.
//	POST /admin/2fa/regenerate     — fresh batch of 10 recovery codes,
//	                                 requires a recent TOTP verify on
//	                                 the active session.
//
// AC #1: the post-password redirect carries only the short-lived
// __Host-mfa-pending cookie. The full __Host-sess-tenant cookie is
// minted exclusively by the verify handler after a successful TOTP
// or recovery-code submission.
//
// AC #2: every unauthenticated/expired pending state on the verify
// endpoint returns 401 and writes an audit_log_security row with
// event_type=2fa_required.
//
// AC #8: a session-scoped failure counter (default 5 strikes in a
// 15-minute window) is enforced by the verify handler. When the
// threshold trips, the handler returns 429 with Retry-After and the
// pending row is deleted so the user must re-login.
package usermfa
