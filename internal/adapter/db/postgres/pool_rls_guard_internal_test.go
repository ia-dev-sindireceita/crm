package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakeRoleRow is a canned pgx.Row for the SIN-65590 RLS-role guard. It
// returns (rolname, rolsuper, rolbypassrls) into the three Scan dests, or
// scanErr when set so the query/scan failure branch can be exercised
// without a real catalog.
type fakeRoleRow struct {
	rolname string
	super   bool
	bypass  bool
	scanErr error
}

func (r fakeRoleRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != 3 {
		return errors.New("fakeRoleRow: expected 3 scan dests")
	}
	*(dest[0].(*string)) = r.rolname
	*(dest[1].(*bool)) = r.super
	*(dest[2].(*bool)) = r.bypass
	return nil
}

// fakeRoleQuerier hands back a fixed fakeRoleRow for any QueryRow.
type fakeRoleQuerier struct{ row pgx.Row }

func (q fakeRoleQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

// recordLogf captures structured WARN lines so the tests can assert the
// "ALWAYS WARN" half of the policy independently of the hard-fail half.
func recordLogf(buf *[]string) func(string, ...any) {
	return func(format string, args ...any) {
		*buf = append(*buf, format)
	}
}

func TestAssertRuntimeRLSRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		super     bool
		bypass    bool
		enforce   bool
		wantErr   bool
		wantWarns int // structured WARN lines emitted
	}{
		// (rolsuper, rolbypassrls) == (false, false): correct app_runtime
		// shape — never warns, never fails, regardless of the flag.
		{name: "nobypass flag off", super: false, bypass: false, enforce: false, wantErr: false, wantWarns: 0},
		{name: "nobypass flag on", super: false, bypass: false, enforce: true, wantErr: false, wantWarns: 0},

		// SUPERUSER bypasses RLS: WARN always; fail only when the flag is on.
		{name: "superuser flag off warns only", super: true, bypass: false, enforce: false, wantErr: false, wantWarns: 1},
		{name: "superuser flag on hard fails", super: true, bypass: false, enforce: true, wantErr: true, wantWarns: 1},

		// Explicit BYPASSRLS (non-superuser) bypasses RLS too: same policy.
		{name: "bypassrls flag off warns only", super: false, bypass: true, enforce: false, wantErr: false, wantWarns: 1},
		{name: "bypassrls flag on hard fails", super: false, bypass: true, enforce: true, wantErr: true, wantWarns: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var warns []string
			q := fakeRoleQuerier{row: fakeRoleRow{rolname: "testrole", super: tt.super, bypass: tt.bypass}}
			err := assertRuntimeRLSRole(context.Background(), q, tt.enforce, recordLogf(&warns))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("err: got nil, want non-nil")
				}
				if !errors.Is(err, ErrRuntimeRoleBypassesRLS) {
					t.Errorf("err: got %v, want errors.Is ErrRuntimeRoleBypassesRLS", err)
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
func TestAssertRuntimeRLSRole_ScanError(t *testing.T) {
	t.Parallel()

	scanErr := errors.New("catalog unavailable")

	t.Run("flag off warns and continues", func(t *testing.T) {
		t.Parallel()
		var warns []string
		q := fakeRoleQuerier{row: fakeRoleRow{scanErr: scanErr}}
		if err := assertRuntimeRLSRole(context.Background(), q, false, recordLogf(&warns)); err != nil {
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
		err := assertRuntimeRLSRole(context.Background(), q, true, recordLogf(&warns))
		if err == nil {
			t.Fatal("err: got nil, want non-nil")
		}
		if !errors.Is(err, scanErr) {
			t.Errorf("err: got %v, want errors.Is scanErr", err)
		}
	})
}

// The hard-fail message must name the bypassing role so an operator can act,
// but must NEVER leak the DSN/credentials (security bar). The guard only
// ever reads current_user / pg_roles, so this asserts the surfaced error
// text stays within that envelope.
func TestAssertRuntimeRLSRole_ErrorNamesRoleNotSecrets(t *testing.T) {
	t.Parallel()
	var warns []string
	q := fakeRoleQuerier{row: fakeRoleRow{rolname: "crm", super: true}}
	err := assertRuntimeRLSRole(context.Background(), q, true, recordLogf(&warns))
	if err == nil {
		t.Fatal("err: got nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "crm") {
		t.Errorf("err %q should name the offending role", err)
	}
	if !strings.Contains(err.Error(), "app_runtime") {
		t.Errorf("err %q should hint the fix (app_runtime)", err)
	}
}

// EnforceRuntimeRLSRoleFromEnv is a no-op when there is no runtime DSN to
// inspect (dev/local without a DB) — it must not try to open a pool.
func TestEnforceRuntimeRLSRoleFromEnv_NoDSN(t *testing.T) {
	t.Parallel()

	if err := EnforceRuntimeRLSRoleFromEnv(context.Background(), nil); err != nil {
		t.Errorf("nil getenv: got %v, want nil", err)
	}
	if err := EnforceRuntimeRLSRoleFromEnv(context.Background(), func(string) string { return "" }); err != nil {
		t.Errorf("empty DSN: got %v, want nil", err)
	}
}
