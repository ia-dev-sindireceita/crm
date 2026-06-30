package postgres

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
)

// EnvEnforceDBTLS names the env var that turns the DATABASE_URL TLS-in-transit
// boot guard (SIN-66324) from a WARNING into a hard boot failure. When its
// value is "1", AssertDatabaseTLSFromEnv returns ErrInsecureDBTLS (so the
// process never finishes booting) if a configured DSN disables TLS — i.e.
// sslmode is disable/allow/prefer or absent (libpq's default `prefer` falls
// back to plaintext silently). It mirrors EnvEnforceRLSRole / DB_ENFORCE_RLS_ROLE:
// compose.stg.yml / compose.yml carry the flag, dev `make up` leaves it unset
// so a same-host bundled-Postgres DSN only WARNs.
const EnvEnforceDBTLS = "DB_ENFORCE_DB_TLS"

// EnvWASessionDSN names the optional dedicated whatsmeow-session Postgres DSN
// (ADR 0107). When set it is guarded for TLS exactly like DATABASE_URL. It is
// duplicated here as a plain string constant (the wiring package owns the
// authoritative envWASessionDSN) so the boot guard has no dependency on
// cmd/server. The values MUST stay in sync; a guard test pins the literal.
const EnvWASessionDSN = "WA_SESSION_DATABASE_URL"

// ErrInsecureDBTLS is returned by the TLS-in-transit boot guard when a
// configured Postgres DSN disables TLS and enforcement is on
// (DB_ENFORCE_DB_TLS=1). Callers can errors.Is on it to surface a
// deterministic boot-failure hint. TLS-in-transit protects credentials and
// every row in flight between the app and Postgres against a same-host or
// on-path observer; sslmode=disable/allow/prefer (or an absent sslmode, which
// libpq treats as `prefer`) lets the connection silently run in cleartext, so
// the only safe response when enforcement is on is to refuse to boot. This is
// a distinct risk class from encryption-at-rest (LUKS), which the operator
// accepted in SIN-66301 — at-rest is out of scope here (SIN-66324).
var ErrInsecureDBTLS = errors.New("postgres: DATABASE_URL disables TLS in transit")

// secureSSLModes is the allow-list of libpq sslmode values that actually
// establish TLS before sending any data. `require` is the floor (encrypts but
// does not verify the server cert); `verify-ca` / `verify-full` additionally
// authenticate the server and are preferred when a CA bundle is available.
// Everything else — disable, allow, prefer, or an absent sslmode — can transmit
// in cleartext and is rejected. Allow-list over deny-list (secure default): an
// unknown/typo'd mode (e.g. "verify_full") fails closed rather than slipping
// through.
var secureSSLModes = map[string]bool{
	"require":     true,
	"verify-ca":   true,
	"verify-full": true,
}

// sslModeFromDSN extracts the lowercased sslmode token from a Postgres DSN in
// either supported form, or "" when absent:
//
//   - URL form    postgres://user:pass@host:5432/db?sslmode=require  → query param
//   - keyword form host=host user=user sslmode=require               → key=value token
//
// It reads ONLY the sslmode token — never the user, password, host, or any
// other secret (security bar: nothing here is logged or returned that could
// leak a credential). A malformed URL yields "" so the guard treats it as
// insecure (fail closed) rather than panicking.
func sslModeFromDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(u.Query().Get("sslmode")))
	}
	// Keyword/DSN form: space-separated key=value pairs.
	for _, field := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(field, "sslmode="); ok {
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), `'"`))
		}
	}
	return ""
}

// assertDSNTLS applies the SIN-66324 boot policy to a single DSN:
//
//   - dsn is empty                         → nil (env var unset; nothing to guard).
//   - sslmode is require/verify-ca/-full   → nil (TLS is established before data flows).
//   - any other / absent sslmode           → ALWAYS emit a structured WARNING; return
//     ErrInsecureDBTLS only when enforce is true.
//
// envName is the variable the DSN came from (DATABASE_URL / WA_SESSION_DATABASE_URL)
// so the WARNING and error name the offending source without echoing the DSN
// (which carries credentials). logf is injected (log.Printf in production) so
// tests can capture the warning without a real logger.
func assertDSNTLS(envName, dsn string, enforce bool, logf func(string, ...any)) error {
	if strings.TrimSpace(dsn) == "" {
		return nil
	}
	mode := sslModeFromDSN(dsn)
	if secureSSLModes[mode] {
		return nil
	}
	shown := mode
	if shown == "" {
		shown = "<absent>"
	}
	logf("level=WARN component=postgres event=db_tls_disabled env=%s sslmode=%q enforce=%t msg=%q",
		envName, shown, enforce,
		"Postgres DSN disables TLS in transit; credentials and tenant rows can be observed on the wire. Set sslmode=require (verify-full when a CA bundle is available). See deploy/compose/.env.example.")
	if enforce {
		return fmt.Errorf("%w: %s has sslmode=%s; set sslmode=require (verify-full preferred)",
			ErrInsecureDBTLS, envName, shown)
	}
	return nil
}

// AssertDatabaseTLSFromEnv is the boot-time defense-in-depth guard for
// TLS-in-transit on every application Postgres DSN (SIN-66324). cmd/server
// calls it once early in boot, alongside EnforceRuntimeRLSRoleFromEnv. It:
//
//   - no-ops (nil) when getenv is nil and skips any DSN that is unset, so
//     dev/local without a DB and the fail-soft feature wires are unaffected;
//   - inspects ONLY the DSN string (DATABASE_URL and, when set, the dedicated
//     whatsmeow-session DSN WA_SESSION_DATABASE_URL) — it never opens a
//     connection, reads a password, or touches the master_ops/audit pools;
//   - WARNs (always) and hard-fails (ErrInsecureDBTLS) only when
//     DB_ENFORCE_DB_TLS=1 for the first DSN that disables TLS.
//
// Unlike the RLS-role guard it needs no database round-trip — sslmode is a
// property of the DSN — so it is pure string parsing and cannot itself brick a
// boot on a transient catalog/connectivity hiccup.
//
// NOTE: the guard inspects the sslmode baked into the DSN. A secure mode
// supplied only via the PGSSLMODE environment variable (libpq's fallback) is
// intentionally NOT honoured — keep sslmode in the DSN so the secure
// configuration is explicit and self-documenting (fail closed).
func AssertDatabaseTLSFromEnv(getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	enforce := getenv(EnvEnforceDBTLS) == "1"
	for _, envName := range []string{EnvDSN, EnvWASessionDSN} {
		if err := assertDSNTLS(envName, getenv(envName), enforce, log.Printf); err != nil {
			return err
		}
	}
	return nil
}
