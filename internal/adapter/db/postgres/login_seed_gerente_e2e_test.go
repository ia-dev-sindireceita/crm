package postgres_test

// SIN-63336 — staging-seed regression for the new acme tenant_gerente
// user. The login_seed_e2e_test in this directory drives the agent user
// (role='agent', the legacy fallback path). This file proves the new
// admin@acme row lands cleanly under the same seed and that the role
// flows into iam.Session.Role as RoleTenantGerente — so a deploy that
// re-runs make seed-stg picks up the row that unblocks the LGPD admin-
// authz positive case in [SIN-63331](/SIN/issues/SIN-63331) /
// [SIN-63335](/SIN/issues/SIN-63335).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// TestRouter_Login_E2E_StgSeed_Gerente proves admin@acme.crm.local logs
// in with the seeded "stg-password", redirects to /hello-tenant, and
// that the persisted users.role='tenant_gerente' lands on the freshly
// created session via the SIN-63336 RoleByUser wireup. Per-tenant tenant
// isolation is also covered by the existing globex/agent path — globex
// stays gerente-less in the seed so this assertion does not need to
// duplicate it.
func TestRouter_Login_E2E_StgSeed_Gerente(t *testing.T) {
	db := freshDBWithIAM(t)
	const baseDomain = "crm.local"
	acmeTenant, _ := applyStgSeed(t, db, baseDomain)

	const acmeHost = "acme." + baseDomain
	const adminEmail = "admin@acme." + baseDomain
	const seedPassword = "stg-password"

	users := postgres.NewUserCredentialReader(db.RuntimePool())
	sessions := postgres.NewSessionStore(db.RuntimePool())
	svc := &iam.Service{
		Tenants:  fixedTenantResolver{host: acmeHost, tenantID: acmeTenant},
		Users:    users,
		Sessions: sessions,
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

	form := url.Values{}
	form.Set("email", adminEmail)
	form.Set("password", seedPassword)
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Host = acmeHost
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/hello-tenant" {
		t.Fatalf("Location=%q, want /hello-tenant", got)
	}
	var sessCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessioncookie.NameTenant {
			sessCookie = c
			break
		}
	}
	if sessCookie == nil {
		t.Fatalf("missing %s cookie", sessioncookie.NameTenant)
	}
	sessID, err := uuid.Parse(sessCookie.Value)
	if err != nil {
		t.Fatalf("session cookie %q is not a uuid: %v", sessCookie.Value, err)
	}

	// Round-trip the session through the runtime pool to assert the
	// persisted Role is exactly tenant_gerente. SIN-63158 wrote the role
	// column on Create; SIN-63336 sources its value from users.role via
	// the new RoleByUser port. If either step regresses the assertion
	// catches it without depending on log capture.
	got, err := sessions.Get(context.Background(), acmeTenant, sessID)
	if err != nil {
		t.Fatalf("sessions.Get: %v", err)
	}
	if got.Role != iam.RoleTenantGerente {
		t.Fatalf("Session.Role=%q, want %q (admin@acme seed must flow tenant_gerente via RoleByUser)", got.Role, iam.RoleTenantGerente)
	}
}

// TestStgSeed_GerenteRowShape pins the shape the LGPD authorizer relies
// on: SELECT email, role FROM users WHERE tenant_id IS NOT NULL must
// return at least the three seeded tenant rows, including the new
// admin@acme/tenant_gerente row. AC#4 names this exact query as the
// staging post-deploy spot check; encoding it here makes a regression
// in the seed visible in CI rather than on the next deploy.
func TestStgSeed_GerenteRowShape(t *testing.T) {
	db := freshDBWithIAM(t)
	const baseDomain = "crm.local"
	_, _ = applyStgSeed(t, db, baseDomain)

	rows, err := db.AdminPool().Query(context.Background(),
		`SELECT email, role FROM users WHERE tenant_id IS NOT NULL ORDER BY email`)
	if err != nil {
		t.Fatalf("query tenant users: %v", err)
	}
	defer rows.Close()

	type entry struct {
		Email, Role string
	}
	var got []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Email, &e.Role); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// SIN-63858: seed agent rows migrated from 'tenant_common' (the
	// SIN-63342 shape) to 'tenant_atendente' so the SIN-63821 /inbox
	// gate {tenant_atendente, tenant_gerente} admits them. This test
	// is a fixture-shape mirror of migrations/seed/stg.sql; its
	// expected values must move whenever the seed's role strings
	// move. It is NOT the behavioral-spec guard for the inbox gate —
	// that contract lives in internal/iam/authorizer_inbox_test.go
	// (SIN-63821) and internal/iam/authorizer_inbox_seed_test.go
	// (SIN-63858).
	want := []entry{
		{"admin@acme." + baseDomain, "tenant_gerente"},
		{"agent@acme." + baseDomain, "tenant_atendente"},
		{"agent@globex." + baseDomain, "tenant_atendente"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestStgSeed_Idempotent re-applies the staging seed to assert AC#1: a
// second `make seed-stg` is a no-op. Without this, a deploy that re-
// applies the seed (e.g. cd-stg) would 23505 on the new admin row and
// stall the pipeline. ON CONFLICT (id) DO NOTHING is the canonical
// idempotency guard, but it has to actually be present on every INSERT
// — this test catches a future edit that adds a row without the clause.
func TestStgSeed_Idempotent(t *testing.T) {
	db := freshDBWithIAM(t)
	const baseDomain = "crm.local"
	_, _ = applyStgSeed(t, db, baseDomain)
	// Second apply must not error.
	_, _ = applyStgSeed(t, db, baseDomain)
}
