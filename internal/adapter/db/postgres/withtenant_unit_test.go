package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/obs"
)

// Pure-Go unit tests for WithTenant / WithMasterOps error paths that the
// real-Postgres integration tests can't reach (begin-tx failure, exec
// failure mid-tx, commit failure). Together with withtenant_test.go they
// keep package coverage above the 85% bar set in SIN-62221.

// stubBeginner implements postgres.TxBeginner so we can drive every error
// branch without a live Postgres.
type stubBeginner struct {
	beginErr error
	tx       pgx.Tx
}

func (s *stubBeginner) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	return s.tx, nil
}

// stubTx implements pgx.Tx by embedding a nil interface (calls to non-
// overridden methods nil-panic, but the helper only calls Exec/Commit/
// Rollback so we override exactly those).
type stubTx struct {
	pgx.Tx
	execErr     error
	execCalls   int
	commitErr   error
	rollbackErr error
	rolledBack  bool
}

func (s *stubTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	s.execCalls++
	return pgconn.CommandTag{}, s.execErr
}

func (s *stubTx) Commit(_ context.Context) error {
	return s.commitErr
}

func (s *stubTx) Rollback(_ context.Context) error {
	s.rolledBack = true
	return s.rollbackErr
}

// ---------------------------------------------------------------------------
// WithTenant unit branches
// ---------------------------------------------------------------------------

func TestWithTenant_BeginTxError(t *testing.T) {
	t.Parallel()
	beginErr := errors.New("connect refused")
	beginner := &stubBeginner{beginErr: beginErr}

	err := postgresadapter.WithTenant(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		t.Fatal("fn must not be called when BeginTx fails")
		return nil
	})
	if !errors.Is(err, beginErr) {
		t.Errorf("err: got %v, want wraps %v", err, beginErr)
	}
	if !strings.Contains(err.Error(), "WithTenant begin tx") {
		t.Errorf("expected wrap prefix, got %v", err)
	}
}

func TestWithTenant_SetConfigError_RollsBack(t *testing.T) {
	t.Parallel()
	tx := &stubTx{execErr: errors.New("network blip")}
	beginner := &stubBeginner{tx: tx}
	called := false

	err := postgresadapter.WithTenant(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "WithTenant set tenant") {
		t.Errorf("err: got %v, want WithTenant set tenant wrap", err)
	}
	if called {
		t.Error("fn ran despite set_config failure")
	}
	if !tx.rolledBack {
		t.Error("expected rollback on set_config failure")
	}
}

func TestWithTenant_CommitError_RollsBack(t *testing.T) {
	t.Parallel()
	commitErr := errors.New("commit blew up")
	tx := &stubTx{commitErr: commitErr}
	beginner := &stubBeginner{tx: tx}

	err := postgresadapter.WithTenant(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		return nil
	})
	if err == nil || !errors.Is(err, commitErr) {
		t.Errorf("err: got %v, want wraps %v", err, commitErr)
	}
	if !strings.Contains(err.Error(), "WithTenant commit") {
		t.Errorf("expected WithTenant commit wrap, got %v", err)
	}
	if !tx.rolledBack {
		t.Error("expected rollback after commit failure (defer)")
	}
}

// TestWithTenant_ZeroTenant_IncrementsRLSCanary is the SIN-66295 end-to-end
// regression: it proves Break A + Break B are fixed together. It wires a real
// *obs.Metrics as the package default (Break B — without obs.SetDefault the
// helper is a permanent no-op and this test fails), then drives WithTenant with
// uuid.Nil and asserts that (a) enforcement is intact — ErrZeroTenant is still
// returned and fn never runs — and (b) the rls_misses_total canary went 0->1
// (Break A — the detector in the uuid.Nil branch). Cleanup resets the default
// to nil so no other test in the package sees a stale instance.
func TestWithTenant_ZeroTenant_IncrementsRLSCanary(t *testing.T) {
	// NOT t.Parallel(): obs.SetDefault mutates package-global state, so
	// running alongside other tests that wire metrics would race.
	m := obs.NewMetrics()
	obs.SetDefault(m)
	t.Cleanup(func() { obs.SetDefault(nil) })

	if got := testutil.ToFloat64(m.RLSMisses); got != 0 {
		t.Fatalf("precondition: rls_misses_total = %v, want 0", got)
	}

	// A non-nil beginner so we reach the uuid.Nil guard rather than
	// ErrNilPool; the guard returns before BeginTx is ever called, so the
	// stub needs no tx.
	beginner := &stubBeginner{}
	err := postgresadapter.WithTenant(context.Background(), beginner, uuid.Nil, func(pgx.Tx) error {
		t.Fatal("fn must not run for a uuid.Nil tenant")
		return nil
	})
	if !errors.Is(err, postgresadapter.ErrZeroTenant) {
		t.Errorf("err: got %v, want ErrZeroTenant (enforcement must not regress)", err)
	}
	if got := testutil.ToFloat64(m.RLSMisses); got != 1 {
		t.Errorf("rls_misses_total: got %v, want 1 (canary must fire on uuid.Nil)", got)
	}
}

// ---------------------------------------------------------------------------
// WithMasterOps unit branches
// ---------------------------------------------------------------------------

func TestWithMasterOps_BeginTxError(t *testing.T) {
	t.Parallel()
	beginErr := errors.New("connect refused")
	beginner := &stubBeginner{beginErr: beginErr}

	err := postgresadapter.WithMasterOps(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		t.Fatal("fn must not run when BeginTx fails")
		return nil
	})
	if !errors.Is(err, beginErr) {
		t.Errorf("err: got %v, want wraps %v", err, beginErr)
	}
	if !strings.Contains(err.Error(), "WithMasterOps begin tx") {
		t.Errorf("expected wrap prefix, got %v", err)
	}
}

func TestWithMasterOps_SetActorError(t *testing.T) {
	t.Parallel()
	tx := &stubTx{execErr: errors.New("network blip")}
	beginner := &stubBeginner{tx: tx}

	err := postgresadapter.WithMasterOps(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		t.Fatal("fn must not run when set actor fails")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "WithMasterOps set actor") {
		t.Errorf("err: got %v, want WithMasterOps set actor wrap", err)
	}
	if !tx.rolledBack {
		t.Error("expected rollback on set actor failure")
	}
}

// stubTxAuditFail succeeds on the first Exec (set_config) but fails on the
// second Exec (the audit-row insert). Used to cover the "audit open" error
// branch in WithMasterOps without a real DB.
type stubTxAuditFail struct {
	stubTx
	failOnCall int
	auditErr   error
}

func (s *stubTxAuditFail) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	s.execCalls++
	if s.execCalls == s.failOnCall {
		return pgconn.CommandTag{}, s.auditErr
	}
	return pgconn.CommandTag{}, nil
}

func TestWithMasterOps_AuditOpenError(t *testing.T) {
	t.Parallel()
	tx := &stubTxAuditFail{failOnCall: 2, auditErr: errors.New("audit table missing")}
	beginner := &stubBeginner{tx: tx}

	err := postgresadapter.WithMasterOps(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		t.Fatal("fn must not run when audit insert fails")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "WithMasterOps audit open") {
		t.Errorf("err: got %v, want WithMasterOps audit open wrap", err)
	}
	if !tx.rolledBack {
		t.Error("expected rollback on audit failure")
	}
}

func TestWithMasterOps_CommitError(t *testing.T) {
	t.Parallel()
	commitErr := errors.New("commit blew up")
	tx := &stubTx{commitErr: commitErr}
	beginner := &stubBeginner{tx: tx}

	err := postgresadapter.WithMasterOps(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		return nil
	})
	if err == nil || !errors.Is(err, commitErr) {
		t.Errorf("err: got %v, want wraps %v", err, commitErr)
	}
	if !strings.Contains(err.Error(), "WithMasterOps commit") {
		t.Errorf("expected WithMasterOps commit wrap, got %v", err)
	}
}
