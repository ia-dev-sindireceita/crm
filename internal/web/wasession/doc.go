// Package wasession is the HTMX provisioning surface for the WhatsApp
// non-official session (Fase 4, SIN-66259; parent plan SIN-66252 rev 4,
// ADR 0107). It lets a tenant gerente pair the unofficial session by QR,
// watch its live status (connected / disconnected / banned), reconnect or
// disconnect it — and, before any of that, read an explicit ban-risk
// notice and record informed consent.
//
// Security posture (the task's non-negotiable bar):
//
//   - Auth deny-by-default. Every route is mounted behind RequireAuth +
//     RequireAction(iam.ActionTenantWASessionManage) (gerente only) by
//     the router; the handler additionally re-derives the tenant + user
//     from context and never trusts a client-supplied id.
//   - CSP-safe. The page carries zero inline on*= handlers and zero
//     inline <script>: every affordance is a declarative hx-* attribute,
//     and the CSRF token rides the app-shell <body> hx-headers. The QR is
//     an inline <svg> (document markup), never an <img src="data:…">
//     (blocked by the strict default-src 'self' CSP) and never an external
//     QR service (SSRF / secret leak).
//   - Consent persisted server-side with an audit trail. Consent is not a
//     UI checkbox: the handler records it through a ConsentGate (bound in
//     the wire to the audited internal/iam/consent RecordingRegistry —
//     who / when / notice-version + IP / UA). The connect path REFUSES to
//     activate the session unless a current-notice grant exists, so the
//     session's active state depends on the recorded consent.
//   - QR never in URL / log. The pairing payload is a bearer secret: it
//     is read only into the inline SVG and is never logged, never placed
//     in a querystring, and never echoed back to the client as text.
//   - Hexagonal. The handler depends only on the small Provisioner /
//     ConsentGate ports it declares here, never on whatsmeow, the session
//     Manager, or a storage driver — those are bound by the cmd/server
//     wire.
package wasession
