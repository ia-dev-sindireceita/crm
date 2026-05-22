package postgres_test

// SIN-63269 / F10 end-to-end POST /login regression. Drives the real
// httpapi.NewRouter chain against a Postgres-backed iam.Service whose
// tenants + users come from migrations/seed/stg.sql (the same seed cd-stg
// applies after every deploy). The structural defect the F10 disclosure
// caught was that NO test in the suite exercised POST /login with the
// staging seed end-to-end: SIN-63154's password_seed_test only re-derives
// the argon2id hashes, csrf_e2e_test seeds an ad-hoc tenant with its own
// hash, and every router_*_test wires an in-memory IAM fake. So a fresh
// staging environment could 500 on every login (column missing, RLS
// posture wrong, etc.) and unit + integration suites would stay green.
//
// This file fills the gap. The three table-driven cases mirror the curl
// matrix the F10 ticket attaches: valid creds -> 302 + cookies + Location
// /hello-tenant; wrong password -> 401; unknown email -> 401. The valid-
// creds case is the one that exploded with "internal server error\n" on
// staging after PR #104; restoring it here gives CI a deterministic gate
// for the next deploy.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// applyStgSeed loads migrations/seed/stg.sql, substitutes the
// :'base_domain' psql variable with the supplied baseDomain, and applies
// the result via the admin (BYPASSRLS) pool — matching the
// `psql -v base_domain=...` invocation in the seed-stg Makefile target.
//
// Returns the tenant IDs the seed minted so each caller can drive
// per-tenant assertions without re-parsing the SQL.
func applyStgSeed(t *testing.T, db *testpg.DB, baseDomain string) (acmeTenant, globexTenant uuid.UUID) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	seedPath := filepath.Join(root, "migrations", "seed", "stg.sql")
	raw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read %s: %v", seedPath, err)
	}
	// psql's :'base_domain' expands to the quoted SQL literal
	// 'crm.local'. The pgx driver does not interpret psql metacommands,
	// so we substitute the literal text before exec.
	sql := strings.ReplaceAll(string(raw), `:'base_domain'`, "'"+baseDomain+"'")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx, sql); err != nil {
		t.Fatalf("apply staging seed: %v", err)
	}
	return uuid.MustParse("00000000-0000-0000-0000-00000000ac01"),
		uuid.MustParse("00000000-0000-0000-0000-00000000eb02")
}

// repoRoot walks up from the test binary's CWD until it finds the
// repository's go.mod. The httpapi tests run from the package directory
// so the seed path is several levels up; we cannot hard-code "../../.."
// because that would silently break if the file moves.
func repoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// TestRouter_Login_E2E_StgSeed is the SIN-63269 regression gate. It boots
// the full httpapi.NewRouter against a Postgres-backed iam.Service whose
// users + tenants come from the actual staging seed file, then drives the
// three curl cases from the F10 disclosure:
//
//   - POST /login with seed creds  -> 302 + __Host-sess-tenant +
//     __Host-csrf + Location /hello-tenant
//   - POST /login with wrong password -> 401
//   - POST /login with unknown email  -> 401
//
// Before any fix that restores the 302 path, the valid-creds case
// reproduces the staging symptom (500 "internal server error\n") so the
// test fails red; after the fix it returns 302 and the test passes.
func TestRouter_Login_E2E_StgSeed(t *testing.T) {
	db := freshDBWithIAM(t)
	const baseDomain = "crm.local"
	acmeTenant, _ := applyStgSeed(t, db, baseDomain)

	const acmeHost = "acme." + baseDomain
	const acmeEmail = "agent@acme." + baseDomain
	const seedPassword = "stg-password"

	svc := &iam.Service{
		Tenants:  fixedTenantResolver{host: acmeHost, tenantID: acmeTenant},
		Users:    postgres.NewUserCredentialReader(db.RuntimePool()),
		Sessions: postgres.NewSessionStore(db.RuntimePool()),
		TTL:      time.Hour,
	}

	h := httpapi.NewRouter(httpapi.Deps{
		IAM: svc,
		TenantResolver: tenancyResolver{
			host:   acmeHost,
			tenant: &tenancy.Tenant{ID: acmeTenant, Name: "acme", Host: acmeHost},
		},
		MasterHost: "master." + baseDomain,
	})

	cases := []struct {
		name           string
		email          string
		password       string
		wantStatus     int
		wantLocation   string
		wantSessCookie bool
	}{
		{
			name:           "valid_creds_redirects_to_hello_tenant",
			email:          acmeEmail,
			password:       seedPassword,
			wantStatus:     http.StatusFound,
			wantLocation:   "/hello-tenant",
			wantSessCookie: true,
		},
		{
			name:       "wrong_password_returns_401",
			email:      acmeEmail,
			password:   "wrong-password",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unknown_email_returns_401",
			email:      "noone@acme." + baseDomain,
			password:   seedPassword,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{}
			form.Set("email", tc.email)
			form.Set("password", tc.password)
			req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
			req.Host = acmeHost
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}

			cookies := rec.Result().Cookies()
			var sess, csrf *http.Cookie
			for _, c := range cookies {
				switch c.Name {
				case sessioncookie.NameTenant:
					sess = c
				case sessioncookie.NameCSRF:
					csrf = c
				}
			}
			if tc.wantSessCookie {
				if sess == nil {
					t.Fatalf("missing %s cookie", sessioncookie.NameTenant)
				}
				if csrf == nil {
					t.Fatalf("missing %s cookie", sessioncookie.NameCSRF)
				}
				if csrf.Value == "" {
					t.Fatalf("CSRF cookie value is empty; iam.Login should mint and the handler should mirror")
				}
				if got := rec.Header().Get("Location"); got != tc.wantLocation {
					t.Fatalf("Location=%q, want %q", got, tc.wantLocation)
				}
				// Sanity check: the cookie value must be a UUID. A 500
				// path that 302s with "" would otherwise sneak through.
				if _, err := uuid.Parse(sess.Value); err != nil {
					t.Fatalf("%s cookie value %q is not a uuid: %v", sessioncookie.NameTenant, sess.Value, err)
				}
			} else {
				if sess != nil {
					t.Fatalf("failure path leaked %s cookie: %+v", sessioncookie.NameTenant, sess)
				}
			}
		})
	}
}
