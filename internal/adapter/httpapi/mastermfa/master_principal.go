package mastermfa

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/pericles-luz/crm/internal/iam"
)

// normalizeHost lowercases, trims, and strips the port from an HTTP Host
// header so a host comparison is robust to ":8080" suffixes and casing.
// The port-strip mirrors slugreservation.stripPort (bracket-aware for
// IPv6 literals) — duplicated rather than imported because pulling
// slugreservation into the mastermfa adapter would invert the layering;
// SIN-65264 / SecEng C2. A naive `r.Host == MasterHost` is explicitly
// rejected (fails on any non-default port).
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

// MasterHostOnly is the outermost gate of the master operator surface
// (the relocated /master/* routes). It 404s every request whose Host
// does not match the configured master-console host, BEFORE any auth or
// session processing runs — so an off-host probe cannot even tell the
// subtree exists, and no master-session side effects (redirect to
// /m/login) leak the route.
//
// SecEng C2 (the SIN-63340 line): the master operator routes MUST be
// served only on MasterHost. An empty/unset MasterHost disables the
// entire surface (fail closed) — it MUST NOT fall back to "match any
// host". The comparison is normalized (lower/trim/strip-port).
//
// 404 (not 403/redirect) is deliberate: it does not confirm the route
// exists to a caller on the wrong host.
func MasterHostOnly(masterHost string, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	want := normalizeHost(masterHost)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if want == "" || normalizeHost(r.Host) != want {
				// Observability for off-host probes of the operator
				// surface (debug — these can be frequent and carry no
				// principal, so they stay below the info threshold).
				logger.DebugContext(r.Context(), "mastermfa: off-host master surface probe 404",
					slog.String("event", "master_host_pin_reject"),
					slog.String("route", r.URL.Path),
				)
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePrincipalFromMasterConfig is the constructor input.
type RequirePrincipalFromMasterConfig struct {
	// MasterHost is the operator-console host. Synthesis is host-pinned to
	// it (defense in depth alongside MasterHostOnly); empty disables
	// synthesis (fail closed). SecEng C2.
	MasterHost string
	Logger     *slog.Logger
}

// RequirePrincipalFromMaster is the cross-tenant principal-synthesis
// bridge (SIN-65264 Gap 3): it converts a verified master-MFA session
// into an iam.Principal{Roles:[RoleMaster]} so the /master/* operator
// handlers — which read iam.PrincipalFromContext — work on the master
// host without a tenant session.
//
// It MUST be composed strictly AFTER RequireMasterAuth (session) AND
// RequireMasterMFA (TOTP verified). It re-derives the master from context
// and fails closed on a miss rather than relying on mount order alone
// (SecEng C1, complete mediation). It is host-pinned to MasterHost
// (SecEng C2) so a RoleMaster principal can never be synthesized off the
// master host.
//
// The synthesized principal carries:
//   - UserID = master.ID (the iam.Principal field is UserID, not ID),
//   - Roles  = [RoleMaster],
//   - TenantID = zero (uuid.Nil) — the surface runs outside TenantScope so
//     no tenant resolves into context (SecEng C4, no tenant-scope
//     confusion),
//   - MasterImpersonating = false (this is the direct operator surface,
//     not impersonation),
//   - MFAVerifiedAt = nil — left unset by design: the Authorizer's PII
//     freshness gate consults it only on the impersonation path, which
//     this surface is not (SecEng C4 documents nil is acceptable here).
//
// Deny-by-default is preserved by the per-route RequireAction gates that
// run AFTER this middleware (SecEng C3): the principal is an identity, not
// an authorization. The crossing is logged for observability (SecEng C6).
func RequirePrincipalFromMaster(cfg RequirePrincipalFromMasterConfig) func(http.Handler) http.Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	want := normalizeHost(cfg.MasterHost)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// C2: host pin — never synthesize a master principal off the
			// configured master host (empty host fails closed).
			if want == "" || normalizeHost(r.Host) != want {
				http.NotFound(w, r)
				return
			}
			// C1: re-derive and gate. A miss means the auth/MFA chain did
			// not run (wiring bug) — fail closed, do NOT synthesize an
			// empty/zero-UUID principal.
			master, ok := MasterFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			p := iam.Principal{
				UserID: master.ID,
				Roles:  []iam.Role{iam.RoleMaster},
			}
			// C6: observe the cross-tenant crossing independent of the
			// per-action authz outcome.
			logger.InfoContext(r.Context(), "mastermfa: master principal synthesized",
				slog.String("event", "master_principal_synthesized"),
				slog.String("user_id", master.ID.String()),
				slog.String("route", r.URL.Path),
			)
			next.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), p)))
		})
	}
}
