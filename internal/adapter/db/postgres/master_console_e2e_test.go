package postgres_test

// SIN-65264 — master console end-to-end real-pg regression.
//
// Drives the FULL master operator flow against a live Postgres instance
// via the testpg harness:
//
//	POST /m/login (not-enrolled) → 303 /m/2fa/enroll   (CTO guardrail: Bug 2c + Enrollment wired)
//	GET  /m/2fa/enroll           → 200 start page       (Bug 2b fix)
//	POST /m/2fa/enroll           → 200 seed + QR page   (Bug 2 fix: not stuck in loop)
//	POST /m/2fa/verify (TOTP)    → 303 /master/tenants  (MFA gate)
//	GET  /master/tenants         → 200                  (Gap 3 fix: bridge works)
//
// Before this fix, all three bugs would have caused an earlier step to fail
// (infinite 303 loop, 405 GET, or 404 on /master/*), so a green run proves
// the full stack is wired correctly end-to-end.
//
// CSRF note: every unsafe POST carries Host + Origin matching the master
// console host (CSRF-6 requirement; safe methods pass the gate without Origin).

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/crypto/aesgcm"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/mastersession"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	usermfaadapter "github.com/pericles-luz/crm/internal/adapter/usermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/mfa"
	"github.com/pericles-luz/crm/internal/tenancy"
)

const (
	masterE2EHost = "master.crm.local"
	masterE2EUser = "master-e2e@crm.local"
	masterE2EPass = "correct-horse-battery-e2e"
)

// passthroughCipher is a test SeedCipher whose Encrypt and Decrypt are
// identity functions. It allows the e2e test to compute TOTP codes from
// the stored bytes without a real AES key.
type passthroughCipher struct{}

func (passthroughCipher) Encrypt(b []byte) ([]byte, error) { return b, nil }
func (passthroughCipher) Decrypt(b []byte) ([]byte, error) { return b, nil }

// noopMFAAudit satisfies mfa.AuditLogger. The e2e test asserts HTTP
// flow correctness; audit persistence is covered by the audit adapter
// unit tests elsewhere.
type noopMFAAudit struct{}

func (noopMFAAudit) LogEnrolled(context.Context, uuid.UUID) error                    { return nil }
func (noopMFAAudit) LogVerified(context.Context, uuid.UUID) error                    { return nil }
func (noopMFAAudit) LogRecoveryUsed(context.Context, uuid.UUID) error                { return nil }
func (noopMFAAudit) LogRecoveryRegenerated(context.Context, uuid.UUID) error         { return nil }
func (noopMFAAudit) LogMFARequired(context.Context, uuid.UUID, string, string) error { return nil }

// TestMasterConsole_E2E_FullFlow is the SIN-65264 end-to-end
// regression: a freshly-seeded not-enrolled master operator completes
// the full /m/login → enroll → verify → /master/tenants flow.
func TestMasterConsole_E2E_FullFlow(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	ctx := context.Background()

	// Seed a master actor (used for master_ops_audit) and a separate
	// operator user with a known password.
	actorID := uuid.New()
	operatorID := uuid.New()
	seedMasterUserRaw(t, ctx, db, actorID, "actor-e2e@crm.local")
	hashPW := hashPasswordForTest(t, masterE2EPass)
	seedMasterUserRaw(t, ctx, db, operatorID, masterE2EUser)
	// Update the password hash to a real argon2id one.
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE users SET password_hash=$1 WHERE id=$2`, hashPW, operatorID); err != nil {
		t.Fatalf("update password_hash: %v", err)
	}

	// Build real postgres adapters.
	sessions, err := mastersession.New(db.MasterOpsPool(), actorID)
	if err != nil {
		t.Fatalf("mastersession.New: %v", err)
	}
	credReader, err := postgres.NewMasterCredentialReader(db.MasterOpsPool(), actorID)
	if err != nil {
		t.Fatalf("NewMasterCredentialReader: %v", err)
	}
	mfaStorage, err := postgres.NewMasterMFA(db.MasterOpsPool(), actorID)
	if err != nil {
		t.Fatalf("NewMasterMFA: %v", err)
	}
	recStore, err := postgres.NewMasterRecoveryCodes(db.MasterOpsPool(), actorID)
	if err != nil {
		t.Fatalf("NewMasterRecoveryCodes: %v", err)
	}

	// mfa.Service wraps the postgres adapters with a passthrough cipher so
	// we can compute TOTP codes from the stored seed bytes directly.
	mfaSvc, err := mfa.NewService(mfa.Config{
		SeedCipher:     passthroughCipher{},
		SeedRepository: mfaStorage,
		RecoveryStore:  recStore,
		CodeHasher:     aesgcm.NewRecoveryHasher(),
		Audit:          noopMFAAudit{},
		Alerter:        usermfaadapter.NoopAlerter{},
		Issuer:         "CRM-E2E",
	})
	if err != nil {
		t.Fatalf("mfa.NewService: %v", err)
	}

	masterDir, err := postgres.NewMasterDirectory(db.MasterOpsPool(), actorID)
	if err != nil {
		t.Fatalf("NewMasterDirectory: %v", err)
	}

	// masterLogin adapts the real credential reader + session store into
	// the MasterLoginFunc signature.
	masterLoginFn := mastermfa.MasterLoginFunc(func(ctx context.Context, _, email, password string, _ net.IP, _, _ string) (iam.Session, error) {
		userID, hash, err := credReader.LookupMasterCredentials(ctx, email)
		if err != nil {
			return iam.Session{}, iam.ErrInvalidCredentials
		}
		ok, err := iam.VerifyPassword(password, hash)
		if err != nil || !ok {
			return iam.Session{}, iam.ErrInvalidCredentials
		}
		sess, err := sessions.Create(ctx, userID, mastermfa.DefaultMasterHardTTL)
		if err != nil {
			return iam.Session{}, err
		}
		return iam.Session{ID: sess.ID, UserID: userID}, nil
	})

	httpSession := mastermfa.NewHTTPSession(sessions)

	loginHandler := mastermfa.NewLoginHandler(mastermfa.LoginHandlerConfig{
		Login:      masterLoginFn,
		Sessions:   sessions,
		HardTTL:    mastermfa.DefaultMasterHardTTL,
		VerifyPath: "/m/2fa/verify",
		Enrollment: mfaStorage, // non-nil: CTO guardrail
		EnrollPath: "/m/2fa/enroll",
	})
	enrollStartHandler := mastermfa.NewEnrollStartHandler(nil)
	enrollHandler := mastermfa.NewEnrollHandler(mfaSvc, nil)
	verifyHandler := mastermfa.NewVerifyHandler(mastermfa.VerifyHandlerConfig{
		Verifier:  mfaSvc,
		Consumer:  mfaSvc,
		Sessions:  httpSession,
		Rotator:   httpSession,
		LoginPath: "/m/login",
	})
	requireAuth := mastermfa.RequireMasterAuth(mastermfa.RequireMasterAuthConfig{
		Sessions:  sessions,
		Directory: masterDir,
		LoginPath: "/m/login",
		IdleTTL:   mastermfa.DefaultMasterIdleTTL,
		Now:       time.Now,
	})
	requireMFA := mastermfa.RequireMasterMFA(mastermfa.RequireMasterMFAConfig{
		Enrollment: mfaStorage,
		Sessions:   httpSession,
		Audit:      noopMFAAudit{},
		EnrollPath: "/m/2fa/enroll",
		VerifyPath: "/m/2fa/verify",
	})
	requirePrincipal := mastermfa.RequirePrincipalFromMaster(mastermfa.RequirePrincipalFromMasterConfig{
		MasterHost: masterE2EHost,
	})

	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            &e2eNoopIAM{},
		TenantResolver: &e2eNoopResolver{},
		MasterHost:     masterE2EHost,
		Authorizer:     authz,
		Master: httpapi.MasterDeps{
			Login:                      loginHandler,
			Logout:                     mastermfa.NewLogoutHandler(mastermfa.LogoutHandlerConfig{Sessions: sessions, LoginPath: "/m/login"}),
			Enroll:                     enrollHandler,
			EnrollStart:                enrollStartHandler,
			Verify:                     verifyHandler,
			RequireMasterAuth:          requireAuth,
			RequireMasterMFA:           requireMFA,
			RequirePrincipalFromMaster: requirePrincipal,
		},
		MasterTenants: httpapi.MasterTenantsRoutes{
			List: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("<html>Tenants</html>"))
			}),
			GrantRequestsList: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("grant-requests"))
			}),
		},
	})

	// --- Step 1: POST /m/login (not-enrolled) → 303 /m/2fa/enroll ---
	t.Log("step 1: POST /m/login (not enrolled)")
	loginForm := url.Values{"email": {masterE2EUser}, "password": {masterE2EPass}}
	r1 := httptest.NewRequest(http.MethodPost, "/m/login", strings.NewReader(loginForm.Encode()))
	r1.Host = masterE2EHost
	r1.Header.Set("Origin", "https://"+masterE2EHost)
	r1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, r1)
	if w1.Code != http.StatusSeeOther {
		t.Fatalf("POST /m/login: status = %d, want 303; body=%q", w1.Code, w1.Body.String())
	}
	loc1 := w1.Header().Get("Location")
	if !strings.HasPrefix(loc1, "/m/2fa/enroll") {
		t.Fatalf("POST /m/login: Location = %q, want /m/2fa/enroll (not-enrolled guard; CTO guardrail)", loc1)
	}

	var sessCookie *http.Cookie
	for _, sc := range w1.Result().Cookies() {
		if sc.Name == sessioncookie.NameMaster {
			sessCookie = sc
			break
		}
	}
	if sessCookie == nil {
		t.Fatalf("POST /m/login: no %s cookie", sessioncookie.NameMaster)
	}

	// --- Step 2: GET /m/2fa/enroll → 200 start page (Bug 2b) ---
	t.Log("step 2: GET /m/2fa/enroll (start page)")
	r2 := httptest.NewRequest(http.MethodGet, "/m/2fa/enroll", nil)
	r2.Host = masterE2EHost
	r2.AddCookie(sessCookie)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("GET /m/2fa/enroll: status = %d, want 200 (Bug 2b); body=%q", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `method="post"`) {
		t.Fatalf("GET /m/2fa/enroll: response missing POST form; body=%q", w2.Body.String())
	}

	// --- Step 3: POST /m/2fa/enroll (mint) → 200 seed page (Bug 2 fix) ---
	t.Log("step 3: POST /m/2fa/enroll (mint)")
	r3 := httptest.NewRequest(http.MethodPost, "/m/2fa/enroll", nil)
	r3.Host = masterE2EHost
	r3.Header.Set("Origin", "https://"+masterE2EHost)
	r3.AddCookie(sessCookie)
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, r3)
	if w3.Code != http.StatusOK {
		t.Fatalf("POST /m/2fa/enroll: status = %d, want 200 (Bug 2 fix); body=%q", w3.Code, w3.Body.String())
	}
	// Extract base32 secret from otpauth URI.
	otpauthRe := regexp.MustCompile(`secret=([A-Z2-7]+)`)
	m3 := otpauthRe.FindStringSubmatch(w3.Body.String())
	if m3 == nil {
		t.Fatalf("POST /m/2fa/enroll: otpauth URI not found in response; body=%q", w3.Body.String())
	}
	seed, err := mfa.DecodeSecret(m3[1])
	if err != nil {
		t.Fatalf("decode TOTP secret: %v", err)
	}

	// --- Step 4: POST /m/2fa/verify → 303 ---
	t.Log("step 4: POST /m/2fa/verify (TOTP)")
	totpCode, err := mfa.Generate(seed, time.Now())
	if err != nil {
		t.Fatalf("generate TOTP code: %v", err)
	}
	verifyForm := url.Values{"code": {totpCode}}
	r4 := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(verifyForm.Encode()))
	r4.Host = masterE2EHost
	r4.Header.Set("Origin", "https://"+masterE2EHost)
	r4.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r4.AddCookie(sessCookie)
	w4 := httptest.NewRecorder()
	router.ServeHTTP(w4, r4)
	if w4.Code != http.StatusSeeOther {
		t.Fatalf("POST /m/2fa/verify: status = %d, want 303; body=%q", w4.Code, w4.Body.String())
	}
	// Capture rotated session cookie.
	for _, sc := range w4.Result().Cookies() {
		if sc.Name == sessioncookie.NameMaster {
			sessCookie = sc
			break
		}
	}

	// --- Step 5: GET /master/tenants → 200 (Gap 3 fix) ---
	t.Log("step 5: GET /master/tenants (Gap 3)")
	r5 := httptest.NewRequest(http.MethodGet, "/master/tenants", nil)
	r5.Host = masterE2EHost
	r5.AddCookie(sessCookie)
	w5 := httptest.NewRecorder()
	router.ServeHTTP(w5, r5)
	if w5.Code != http.StatusOK {
		t.Fatalf("GET /master/tenants: status = %d, want 200 (Gap 3 fix); body=%q", w5.Code, w5.Body.String())
	}
}

// --- Test helpers ---

// seedMasterUserRaw inserts a master user with password_hash='x' (placeholder).
func seedMasterUserRaw(t *testing.T, ctx context.Context, db *testpg.DB, id uuid.UUID, email string) { //nolint:unparam
	t.Helper()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master) VALUES ($1, NULL, $2, 'x', 'master', true)`,
		id, email); err != nil {
		t.Fatalf("seedMasterUserRaw(%s): %v", email, err)
	}
}

// hashPasswordForTest calls iam.Service.HashPassword to produce a real
// argon2id PHC string matching the iam package parameters.
func hashPasswordForTest(t *testing.T, password string) string {
	t.Helper()
	hash, err := iam.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

// --- Minimal no-op adapters so httpapi.NewRouter is happy ---

type e2eNoopIAM struct{}

func (e2eNoopIAM) Login(_ context.Context, _, _, _ string, _ net.IP, _, _ string) (iam.Session, error) { //nolint:unparam
	return iam.Session{}, iam.ErrInvalidCredentials
}
func (e2eNoopIAM) Logout(_ context.Context, _, _ uuid.UUID) error { return nil }
func (e2eNoopIAM) ValidateSession(_ context.Context, _, _ uuid.UUID) (iam.Session, error) {
	return iam.Session{}, iam.ErrSessionNotFound
}

type e2eNoopResolver struct{}

func (e2eNoopResolver) ResolveByHost(_ context.Context, _ string) (*tenancy.Tenant, error) {
	return nil, tenancy.ErrTenantNotFound
}
