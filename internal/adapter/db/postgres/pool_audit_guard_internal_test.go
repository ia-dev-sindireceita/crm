package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// SIN-66332 — the audit-role guard is the INVERSE of the runtime guard
// (TestAssertRuntimeRLSRole): the audit role is correct precisely when it
// bypasses RLS, because the SplitAuditLogger writes NULL-tenant master rows
// and bare INSERTs outside any WithTenant scope. A NOBYPASSRLS audit role
// re-introduces the 42501 the 2FA-enroll 500 surfaced. Reuses fakeRoleRow /
// fakeRoleQuerier / recordLogf from pool_rls_guard_internal_test.go.
func TestAssertAuditRLSRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		super     bool
		bypass    bool
		enforce   bool
		wantErr   bool
		wantWarns int
	}{
		// (super, bypass) bypasses RLS == correct app_audit shape: never
		// warns, never fails, regardless of the enforce flag.
		{name: "bypassrls flag off", super: false, bypass: true, enforce: false, wantErr: false, wantWarns: 0},
		{name: "bypassrls flag on", super: false, bypass: true, enforce: true, wantErr: false, wantWarns: 0},
		{name: "superuser flag off", super: true, bypass: false, enforce: false, wantErr: false, wantWarns: 0},
		{name: "superuser flag on", super: true, bypass: false, enforce: true, wantErr: false, wantWarns: 0},

		// NOBYPASSRLS audit role: WARN always; hard-fail only with the flag.
		{name: "nobypass flag off warns only", super: false, bypass: false, enforce: false, wantErr: false, wantWarns: 1},
		{name: "nobypass flag on hard fails", super: false, bypass: false, enforce: true, wantErr: true, wantWarns: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var warns []string
			q := fakeRoleQuerier{row: fakeRoleRow{rolname: "app_audit", super: tt.super, bypass: tt.bypass}}
			err := assertAuditRLSRole(context.Background(), q, tt.enforce, recordLogf(&warns))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("err: got nil, want non-nil")
				}
				if !errors.Is(err, ErrAuditRoleEnforcesRLS) {
					t.Errorf("err: got %v, want errors.Is ErrAuditRoleEnforcesRLS", err)
				}
			} else if err != nil {
				t.Fatalf("err: got %v, want nil", err)
			}

			if len(warns) != tt.wantWarns {
				t.Errorf("warns: got %d %v, want %d", len(warns), warns, tt.wantWarns)
			}
		})
	}
}

// A query/scan error fails the boot only under enforcement; otherwise it
// degrades to a WARN so a transient pg_roles read never bricks a dev boot.
func TestAssertAuditRLSRole_ScanError(t *testing.T) {
	t.Parallel()

	scanErr := errors.New("catalog unavailable")

	t.Run("flag off warns and continues", func(t *testing.T) {
		t.Parallel()
		var warns []string
		q := fakeRoleQuerier{row: fakeRoleRow{scanErr: scanErr}}
		if err := assertAuditRLSRole(context.Background(), q, false, recordLogf(&warns)); err != nil {
			t.Fatalf("err: got %v, want nil", err)
		}
		if len(warns) != 1 {
			t.Errorf("warns: got %d, want 1", len(warns))
		}
	})

	t.Run("flag on surfaces the error", func(t *testing.T) {
		t.Parallel()
		var warns []string
		q := fakeRoleQuerier{row: fakeRoleRow{scanErr: scanErr}}
		err := assertAuditRLSRole(context.Background(), q, true, recordLogf(&warns))
		if err == nil {
			t.Fatal("err: got nil, want non-nil")
		}
		if !errors.Is(err, scanErr) {
			t.Errorf("err: got %v, want errors.Is scanErr", err)
		}
	})
}

// The hard-fail message must name the offending role and hint the fix
// (app_audit) without leaking the DSN/credentials (security bar).
func TestAssertAuditRLSRole_ErrorNamesRoleNotSecrets(t *testing.T) {
	t.Parallel()
	var warns []string
	q := fakeRoleQuerier{row: fakeRoleRow{rolname: "app_runtime"}}
	err := assertAuditRLSRole(context.Background(), q, true, recordLogf(&warns))
	if err == nil {
		t.Fatal("err: got nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "app_runtime") {
		t.Errorf("err %q should name the offending role", err)
	}
	if !strings.Contains(err.Error(), "app_audit") {
		t.Errorf("err %q should hint the fix (app_audit)", err)
	}
}

// NewAuditFromEnv surfaces ErrEmptyDSN (not a panic / nil pool) when there is
// no AUDIT_DATABASE_URL, so cmd/server can fall back to the runtime pool in
// dev via a clean errors.Is check.
func TestNewAuditFromEnv_EmptyDSN(t *testing.T) {
	t.Parallel()

	if _, err := NewAuditFromEnv(context.Background(), nil); !errors.Is(err, ErrEmptyDSN) {
		t.Errorf("nil getenv: got %v, want ErrEmptyDSN", err)
	}
	getenv := func(string) string { return "" }
	if _, err := NewAuditFromEnv(context.Background(), getenv); !errors.Is(err, ErrEmptyDSN) {
		t.Errorf("empty DSN: got %v, want ErrEmptyDSN", err)
	}
}

// EnforceAuditRLSRoleFromEnv must not open a pool when there is no audit DSN.
// The unset case is fail-HARD only when the app actually connects to a DB
// (DATABASE_URL set) in staging/production — audit is a non-repudiation
// control that must not silently route through the NOBYPASSRLS runtime pool.
// With no DATABASE_URL (or in dev) it is fail-soft, so narrow boot-gate paths
// and dev `make up` are unaffected.
func TestEnforceAuditRLSRoleFromEnv_NoDSN(t *testing.T) {
	t.Parallel()

	if err := EnforceAuditRLSRoleFromEnv(context.Background(), nil); err != nil {
		t.Errorf("nil getenv: got %v, want nil", err)
	}

	tests := []struct {
		name      string
		appEnv    string
		runtimDSN string
		wantErr   bool
	}{
		{name: "dev unset is fail-soft", appEnv: "", runtimDSN: "postgres://x", wantErr: false},
		{name: "development unset is fail-soft", appEnv: "development", runtimDSN: "postgres://x", wantErr: false},
		{name: "staging with DB fails hard", appEnv: "staging", runtimDSN: "postgres://x", wantErr: true},
		{name: "production with DB fails hard", appEnv: "production", runtimDSN: "postgres://x", wantErr: true},
		// No DATABASE_URL → no audit writes to route → fail-soft even in prod
		// (narrow boot-gate tests / DB-less configs).
		{name: "staging without DB is fail-soft", appEnv: "staging", runtimDSN: "", wantErr: false},
		{name: "production without DB is fail-soft", appEnv: "production", runtimDSN: "", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string {
				switch k {
				case EnvAppEnv:
					return tt.appEnv
				case EnvDSN:
					return tt.runtimDSN
				default:
					return "" // AUDIT_DATABASE_URL unset
				}
			}
			err := EnforceAuditRLSRoleFromEnv(context.Background(), getenv)
			if tt.wantErr {
				if !errors.Is(err, ErrAuditDSNRequired) {
					t.Errorf("err: got %v, want errors.Is ErrAuditDSNRequired", err)
				}
			} else if err != nil {
				t.Errorf("err: got %v, want nil", err)
			}
		})
	}
}
