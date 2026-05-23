package iam

// SIN-63336 — per-user role lookup wired into Login. The table-driven
// cases mirror the acceptance criteria: each of the three tenant roles
// persists into Session.Role; the legacy 'agent' value falls back to
// RoleTenantCommon without 5xx; the empty string falls back the same
// way (defense against rows that pre-date the role column); an
// arbitrary string (a future typo or hostile write) falls back to
// RoleTenantCommon and emits a WARN log line carrying the raw value
// so the regression surfaces in observability without polluting auth
// shape.
//
// SIN-63340 §Item 1 — the critical STRIDE-E regression case:
// users.role='master' on a tenant Login MUST NOT mint a master-scope
// session. The tenant allowlist deliberately excludes RoleMaster; a
// future refactor that drifts back to iam.Role.Valid() (which DOES
// accept master) would fail this case loudly. The 'admin' MFA-marker
// string is also out-of-scope-for-Login and likewise downgrades.
//
// A further case asserts that a RoleByUser infra error does NOT fail
// the login — the user has already authenticated; degrading to
// RoleTenantCommon is the correct posture per the SIN-63336 defense-
// in-depth notes (avoid both DoS and privilege escalation paths).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLogin_PerUserRoleLookup(t *testing.T) {
	const password = "correct-horse-battery-staple"
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	userID := uuid.MustParse("22222222-2222-4222-8222-222222222222")

	cases := []struct {
		name        string
		storedRole  Role
		wantRole    Role
		wantWarnRaw string // substring expected in the WARN log; "" = no warn expected
	}{
		{
			name:       "gerente_persists_as_gerente",
			storedRole: RoleTenantGerente,
			wantRole:   RoleTenantGerente,
		},
		{
			name:       "atendente_persists_as_atendente",
			storedRole: RoleTenantAtendente,
			wantRole:   RoleTenantAtendente,
		},
		{
			name:       "common_persists_as_common",
			storedRole: RoleTenantCommon,
			wantRole:   RoleTenantCommon,
		},
		{
			name:        "legacy_agent_falls_back_to_common",
			storedRole:  Role("agent"),
			wantRole:    RoleTenantCommon,
			wantWarnRaw: "agent",
		},
		{
			name:        "empty_falls_back_to_common",
			storedRole:  Role(""),
			wantRole:    RoleTenantCommon,
			wantWarnRaw: `role_string=""`,
		},
		{
			name:        "garbage_falls_back_to_common",
			storedRole:  Role("not_a_real_role"),
			wantRole:    RoleTenantCommon,
			wantWarnRaw: "not_a_real_role",
		},
		// SIN-63340 §Item 1 — STRIDE-E privilege-escalation guard. A
		// tenant user row with users.role='master' MUST NOT mint a
		// master-scope session via the tenant Login path. Pinning this
		// behaviour here means any future refactor that drifts back to
		// iam.Role.Valid() (which DOES accept master) breaks CI.
		{
			name:        "master_on_tenant_row_downgrades_to_common",
			storedRole:  RoleMaster,
			wantRole:    RoleTenantCommon,
			wantWarnRaw: "master",
		},
		// MFA admin marker ('admin' — see internal/adapter/db/postgres/
		// user_mfa_requirement.go:AdminRole) is a TOTP-required flag,
		// not an authorizer role. It must also downgrade — the seed
		// uses 'tenant_gerente' + totp_required_at instead.
		{
			name:        "admin_marker_downgrades_to_common",
			storedRole:  Role("admin"),
			wantRole:    RoleTenantCommon,
			wantWarnRaw: "admin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hash, err := HashPassword(password)
			if err != nil {
				t.Fatalf("HashPassword: %v", err)
			}
			var logBuf bytes.Buffer
			svc := &Service{
				Tenants: fakeResolver{hosts: map[string]uuid.UUID{
					"acme.crm.local": tenantID,
				}},
				Users: fakeUsers{
					rows: map[string]struct {
						userID uuid.UUID
						hash   string
					}{
						tenantID.String() + "|alice@acme.test": {userID, hash},
					},
					roles: map[string]Role{
						tenantID.String() + "|" + userID.String(): tc.storedRole,
					},
				},
				Sessions: newFakeStore(),
				TTL:      time.Hour,
				Logger:   slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})),
			}

			sess, err := svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", password, nil, "", "")
			if err != nil {
				t.Fatalf("Login: %v", err)
			}
			if sess.Role != tc.wantRole {
				t.Fatalf("Session.Role=%q, want %q", sess.Role, tc.wantRole)
			}

			logged := logBuf.String()
			if tc.wantWarnRaw == "" {
				// Valid role -> no allowlist-downgrade warn line. The
				// login: ok info line is at INFO level which this
				// handler filters out, so the buffer should be empty.
				if strings.Contains(logged, "role not in tenant allowlist") {
					t.Fatalf("unexpected allowlist warn for valid role %q: %s", tc.storedRole, logged)
				}
				return
			}
			if !strings.Contains(logged, "role not in tenant allowlist") {
				t.Fatalf("expected 'role not in tenant allowlist' warn for stored=%q, got: %s", tc.storedRole, logged)
			}
			if !strings.Contains(logged, tc.wantWarnRaw) {
				t.Fatalf("warn missing raw role substring %q in: %s", tc.wantWarnRaw, logged)
			}
		})
	}
}

// TestLogin_RoleLookupInfraError_DegradesToCommon proves that a failure in
// the RoleByUser port does NOT fail the login — the user has already
// authenticated; the session is born with RoleTenantCommon and the
// degraded path is logged. Failing the login on this branch would 5xx
// every authenticated request after a transient DB blip, turning an
// auxiliary lookup into an availability incident.
func TestLogin_RoleLookupInfraError_DegradesToCommon(t *testing.T) {
	const password = "correct-horse-battery-staple"
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	userID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	var logBuf bytes.Buffer
	svc := &Service{
		Tenants: fakeResolver{hosts: map[string]uuid.UUID{
			"acme.crm.local": tenantID,
		}},
		Users: fakeUsers{
			rows: map[string]struct {
				userID uuid.UUID
				hash   string
			}{
				tenantID.String() + "|alice@acme.test": {userID, hash},
			},
			roleErr: errors.New("postgres: timeout"),
		},
		Sessions: newFakeStore(),
		TTL:      time.Hour,
		Logger:   slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	sess, err := svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", password, net.IPv4(127, 0, 0, 1), "", "")
	if err != nil {
		t.Fatalf("Login must not fail on role-lookup error, got: %v", err)
	}
	if sess.Role != RoleTenantCommon {
		t.Fatalf("Session.Role=%q on lookup error, want RoleTenantCommon", sess.Role)
	}
	if !strings.Contains(logBuf.String(), "role lookup failed") {
		t.Fatalf("expected 'role lookup failed' warn, got: %s", logBuf.String())
	}
}
