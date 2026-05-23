package postgres_test

// SIN-63361 — end-to-end regression for the user-side TOTP enforcement
// wireup. Before the SIN-63361 wireup landed, every probed enrolment
// surface returned 404 and POST /login as admin@acme.crm.local minted
// a tenant session unconditionally — even though the staging seed
// already stamped totp_required_at=now() on that row. This file pins
// the post-wireup contract:
//
//   - POST /login with the seeded admin@acme + stg-password 303s to
//     /admin/2fa/setup (the enrolment surface) instead of 302 to
//     /hello-tenant.
//   - The response carries a __Host-mfa-pending cookie and NO
//     __Host-sess-tenant cookie — the credential check succeeded but
//     the principal is in the pending-MFA state per ADR 0102.
//
// The test boots httpapi.NewRouter with Deps.UserMFA populated by the
// same usermfa.LoginPost the production wire builds. The per-request
// tenant bridges live in cmd/server (cannot be imported from
// internal/), so the test wires the postgres adapters directly with
// the known per-test tenant id — the LoginPost code path under
// exercise is identical.
//
// The companion negative case (Deps.UserMFA.LoginPost nil → fallback
// to handler.LoginPost → 302 /hello-tenant) is the contract the
// existing TestRouter_Login_E2E_StgSeed_Gerente test already pins, so
// we do not re-assert it here.

import (
	"crypto/rand"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/crypto/aesgcm"
	pg "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/usermfa"
	usermfaadapter "github.com/pericles-luz/crm/internal/adapter/usermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/mfa"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// TestRouter_Login_E2E_StgSeed_Gerente_RedirectsToTOTPSetup is the
// SIN-63361 regression gate. Before the wireup it fails because the
// router falls through to handler.LoginPost (302 to /hello-tenant);
// after the wireup it passes because usermfa.LoginPost sees
// TOTPRequired=true on the admin@acme row and 303s to
// /admin/2fa/setup instead.
//
// This is the test the issue's "regression test (non-negotiable,
// security bar)" clause names. It must fail against HEAD before this
// file's sibling cmd/server/usermfa_wire.go lands and pass after.
func TestRouter_Login_E2E_StgSeed_Gerente_RedirectsToTOTPSetup(t *testing.T) {
	db := freshDBWithIAM(t)
	const baseDomain = "crm.local"
	acmeTenant, _ := applyStgSeed(t, db, baseDomain)

	const acmeHost = "acme." + baseDomain
	const adminEmail = "admin@acme." + baseDomain
	const seedPassword = "stg-password"

	pool := db.RuntimePool()
	users := pg.NewUserCredentialReader(pool)
	sessions := pg.NewSessionStore(pool)
	iamSvc := &iam.Service{
		Tenants:  fixedTenantResolver{host: acmeHost, tenantID: acmeTenant},
		Users:    users,
		Sessions: sessions,
		TTL:      time.Hour,
	}

	requirements, err := pg.NewTenantUserMFARequirement(pool, acmeTenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFARequirement: %v", err)
	}
	pendings, err := pg.NewTenantUserMFAPending(pool, acmeTenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFAPending: %v", err)
	}

	loginPost := usermfa.LoginPost(usermfa.LoginConfig{
		IAM:          iamSvc,
		Sessions:     sessions,
		Pendings:     usermfa.NewPendingsBridge(pendings),
		Requirements: usermfa.NewRequirementsBridge(requirements),
		Logger:       slog.Default(),
	})

	h := httpapi.NewRouter(httpapi.Deps{
		IAM: iamSvc,
		TenantResolver: tenancyResolver{
			host:   acmeHost,
			tenant: &tenancy.Tenant{ID: acmeTenant, Name: "acme", Host: acmeHost},
		},
		MasterHost: "master." + baseDomain,
		UserMFA: httpapi.UserMFARoutes{
			LoginPost: loginPost,
		},
	})

	form := url.Values{}
	form.Set("email", adminEmail)
	form.Set("password", seedPassword)
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Host = acmeHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/admin/2fa/setup" {
		t.Fatalf("Location=%q, want /admin/2fa/setup (admin@acme is seeded with totp_required_at=now() and IS NOT enrolled — the wireup MUST 303 to setup, not /hello-tenant)", loc)
	}

	var pending, session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case sessioncookie.NameTenantPending:
			pending = c
		case sessioncookie.NameTenant:
			session = c
		}
	}
	if pending == nil || pending.Value == "" {
		t.Fatalf("missing __Host-mfa-pending cookie — usermfa.LoginPost must mint it on TOTP-required principals")
	}
	if session != nil {
		t.Fatalf("__Host-sess-tenant cookie was set on a TOTP-required principal (value=%q) — credential check succeeded but the session should NOT exist until /admin/2fa/verify completes", session.Value)
	}
}

// TestRouter_Login_E2E_StgSeed_TOTPSetup_RendersQRAndCodes proves the
// /admin/2fa/setup route the redirect points at actually renders. The
// SIN-63359 Lens 1 curl matrix saw 404 on every enrolment URL because
// the route was unmounted; this test wires the Setup handler the same
// way cmd/server does and asserts that a request bearing the pending
// cookie minted by LoginPost lands on a 200 with the otpauth secret
// embedded in the body.
func TestRouter_Login_E2E_StgSeed_TOTPSetup_RendersQRAndCodes(t *testing.T) {
	db := freshDBWithIAM(t)
	const baseDomain = "crm.local"
	acmeTenant, _ := applyStgSeed(t, db, baseDomain)

	const acmeHost = "acme." + baseDomain
	const adminEmail = "admin@acme." + baseDomain
	const seedPassword = "stg-password"

	pool := db.RuntimePool()
	users := pg.NewUserCredentialReader(pool)
	sessions := pg.NewSessionStore(pool)
	iamSvc := &iam.Service{
		Tenants:  fixedTenantResolver{host: acmeHost, tenantID: acmeTenant},
		Users:    users,
		Sessions: sessions,
		TTL:      time.Hour,
	}

	requirements, err := pg.NewTenantUserMFARequirement(pool, acmeTenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFARequirement: %v", err)
	}
	pendings, err := pg.NewTenantUserMFAPending(pool, acmeTenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFAPending: %v", err)
	}
	seeds, err := pg.NewTenantUserMFA(pool, acmeTenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFA: %v", err)
	}
	recoveryCodes, err := pg.NewTenantUserRecoveryCodes(pool, acmeTenant)
	if err != nil {
		t.Fatalf("NewTenantUserRecoveryCodes: %v", err)
	}
	// audit_log_security has RLS that the runtime role cannot satisfy
	// for the in-test wire-up. The split-audit logger is wired against
	// the admin (BYPASSRLS) pool so the Enroller's audit row lands
	// without a real app_audit role — the SIN-63361 unit-of-work being
	// pinned is the route reachability + redirect target, not the
	// audit grants which are exhaustively covered in
	// audit_logger_split_test.go.
	splitLogger, err := pg.NewSplitAuditLogger(db.AdminPool())
	if err != nil {
		t.Fatalf("NewSplitAuditLogger: %v", err)
	}
	auditLogger, err := usermfaadapter.NewTenantAuditLogger(splitLogger, acmeTenant)
	if err != nil {
		t.Fatalf("NewTenantAuditLogger: %v", err)
	}
	labels, err := pg.NewTenantUserLabel(pool)
	if err != nil {
		t.Fatalf("NewTenantUserLabel: %v", err)
	}

	keyBytes := make([]byte, aesgcm.KeySize)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	seedCipher, err := aesgcm.New(keyBytes, rand.Reader)
	if err != nil {
		t.Fatalf("aesgcm.New: %v", err)
	}
	mfaSvc, err := mfa.NewService(mfa.Config{
		SeedRepository: seeds,
		SeedCipher:     seedCipher,
		RecoveryStore:  recoveryCodes,
		CodeHasher:     aesgcm.NewRecoveryHasher(),
		Audit:          auditLogger,
		Alerter:        usermfaadapter.NoopAlerter{},
		Issuer:         "Sindireceita",
	})
	if err != nil {
		t.Fatalf("mfa.NewService: %v", err)
	}

	sessionMinter, err := usermfa.NewTenantSessionMinter(sessions, time.Hour)
	if err != nil {
		t.Fatalf("NewTenantSessionMinter: %v", err)
	}
	handler, err := usermfa.NewHandler(usermfa.HandlerConfig{
		Enroller:      mfaSvc,
		Verifier:      mfaSvc,
		Consumer:      mfaSvc,
		Regenerator:   mfaSvc,
		Pendings:      usermfa.NewPendingsBridge(pendings),
		Enrollment:    seeds,
		SessionMinter: sessionMinter,
		Failures:      usermfa.NewMemoryFailureCounter(0),
		Audit:         auditLogger,
		Labels:        labels,
		Logger:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	loginPost := usermfa.LoginPost(usermfa.LoginConfig{
		IAM:          iamSvc,
		Sessions:     sessions,
		Pendings:     usermfa.NewPendingsBridge(pendings),
		Requirements: usermfa.NewRequirementsBridge(requirements),
		Logger:       slog.Default(),
	})

	router := httpapi.NewRouter(httpapi.Deps{
		IAM: iamSvc,
		TenantResolver: tenancyResolver{
			host:   acmeHost,
			tenant: &tenancy.Tenant{ID: acmeTenant, Name: "acme", Host: acmeHost},
		},
		MasterHost: "master." + baseDomain,
		UserMFA: httpapi.UserMFARoutes{
			LoginPost: loginPost,
			Setup:     http.HandlerFunc(handler.Setup),
			Verify:    http.HandlerFunc(handler.Verify),
		},
	})

	// Step 1 — POST /login → 303 /admin/2fa/setup + pending cookie.
	form := url.Values{}
	form.Set("email", adminEmail)
	form.Set("password", seedPassword)
	loginReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	loginReq.Host = acmeHost
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusSeeOther {
		t.Fatalf("login status=%d, want 303; body=%q", loginRec.Code, loginRec.Body.String())
	}
	var pendingCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == sessioncookie.NameTenantPending {
			pendingCookie = c
		}
	}
	if pendingCookie == nil {
		t.Fatalf("login did not mint __Host-mfa-pending cookie")
	}

	// Step 2 — GET /admin/2fa/setup with the pending cookie. Expect
	// 200 with the otpauth:// URI in the body (the authenticator-app
	// QR payload — proves the route is reachable AND the Enroller
	// successfully encrypted + persisted a fresh seed).
	setupReq := httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
	setupReq.Host = acmeHost
	setupReq.AddCookie(pendingCookie)
	setupRec := httptest.NewRecorder()
	router.ServeHTTP(setupRec, setupReq)
	if setupRec.Code != http.StatusOK {
		t.Fatalf("setup status=%d, want 200; body=%q", setupRec.Code, setupRec.Body.String())
	}
	body := setupRec.Body.String()
	if !strings.Contains(body, "otpauth://") {
		t.Fatalf("setup body missing otpauth:// URI (the authenticator-app QR payload). Body:\n%s", body)
	}

	// Sanity — the seeded admin email is what the otpauth label
	// embeds, so reaching this point means LookupLabel saw the right
	// row through the per-test tenant pool.
	if !strings.Contains(body, adminEmail) {
		t.Fatalf("setup body missing %q in the otpauth label. Body:\n%s", adminEmail, body)
	}
}
