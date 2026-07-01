// Package channels serves the SIN-66391 (P2) multi-channel-per-tenant
// admin surface at /settings/channels: the gerente-only HTMX registry
// (list), the create/edit modal form with the in-form access roster, and
// the activate/deactivate toggle.
//
// It is the HTTP/transport adapter for the internal/channels bounded
// context (SIN-66389, P1): it depends on channels.Repository (channel
// CRUD) and channels.AccessRepository (roster listing + per-channel grant
// writes) through ports, holds no SQL, and renders server-side HTML with
// hx-* partial swaps + OOB updates under the strict CSP (no inline on*= /
// hx-on: handlers; the surface loads its own nonce'd htmx — the app-shell
// does not inject it).
//
// Scope boundary: this surface authors a channel's *initial / edited*
// access roster from the management form, gerente-gated at the route. The
// per-resource access *enforcement* contract (atendente cannot self-grant,
// audit line on every change, user-deactivation cascade) and the
// standalone access-maintenance screen are the P3 concern (SIN-66392,
// SecurityEngineer loop-in) and are intentionally NOT implemented here.
package channels
