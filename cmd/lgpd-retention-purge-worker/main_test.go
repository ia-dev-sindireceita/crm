// Tests pin the env-parsing helpers so a deploy mistake fails at boot
// with a message that names the env knob. The heavy worker tests live
// in internal/worker/lgpd_retention.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/worker/lgpd_retention"
)

// stubRepo satisfies lgpd.DeletionRepository and lgpd.PurgeRepository
// with no-op implementations. Used by runWith tests.
type stubRepo struct{}

func (stubRepo) Upsert(context.Context, lgpd.DeletionRequest) (lgpd.DeletionRequest, error) {
	return lgpd.DeletionRequest{}, errors.New("not used")
}
func (stubRepo) Get(context.Context, uuid.UUID) (lgpd.DeletionRequest, error) {
	return lgpd.DeletionRequest{}, lgpd.ErrDeletionRequestNotFound
}
func (stubRepo) ListReady(context.Context, time.Time, int) ([]lgpd.DeletionRequest, error) {
	return nil, nil
}
func (stubRepo) MarkCompleted(context.Context, uuid.UUID, time.Time) error { return nil }
func (stubRepo) MarkFailed(context.Context, uuid.UUID, time.Time) error    { return nil }
func (stubRepo) PurgeContact(context.Context, uuid.UUID, uuid.UUID) error  { return nil }

// clearWorkerEnv resets every env knob loadConfig reads.
func clearWorkerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DATABASE_URL",
		"DATABASE_MASTER_OPS_URL",
		"LGPD_FISCAL_RETENTION_YEARS",
		"LGPD_RETENTION_INTERVAL",
		"LGPD_RETENTION_BATCH_SIZE",
		"LGPD_RETENTION_ENABLED",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfig_RejectsMissingDatabaseURL(t *testing.T) {
	clearWorkerEnv(t)
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("loadConfig: want DATABASE_URL error, got %v", err)
	}
}

func TestLoadConfig_RejectsMissingMasterDSN(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_MASTER_OPS_URL") {
		t.Fatalf("loadConfig: want DATABASE_MASTER_OPS_URL error, got %v", err)
	}
}

func TestLoadConfig_RejectsNegativeRetention(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DATABASE_MASTER_OPS_URL", "postgres://y")
	t.Setenv("LGPD_FISCAL_RETENTION_YEARS", "-1")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("loadConfig: want retention error, got %v", err)
	}
}

func TestLoadConfig_AppliesDefaults(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DATABASE_MASTER_OPS_URL", "postgres://y")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.policy.FiscalYears != lgpd.DefaultFiscalRetentionYears {
		t.Errorf("policy.FiscalYears = %d, want %d", cfg.policy.FiscalYears, lgpd.DefaultFiscalRetentionYears)
	}
	if cfg.interval != lgpd_retention.DefaultInterval {
		t.Errorf("interval = %s, want %s", cfg.interval, lgpd_retention.DefaultInterval)
	}
	if cfg.batch != lgpd_retention.DefaultBatchSize {
		t.Errorf("batch = %d, want %d", cfg.batch, lgpd_retention.DefaultBatchSize)
	}
	if !cfg.enabled {
		t.Error("enabled = false, want true (default 1)")
	}
}

func TestLoadConfig_OverridesPropagate(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DATABASE_MASTER_OPS_URL", "postgres://y")
	t.Setenv("LGPD_FISCAL_RETENTION_YEARS", "7")
	t.Setenv("LGPD_RETENTION_INTERVAL", "15m")
	t.Setenv("LGPD_RETENTION_BATCH_SIZE", "50")
	t.Setenv("LGPD_RETENTION_ENABLED", "0")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.policy.FiscalYears != 7 || cfg.interval != 15*time.Minute || cfg.batch != 50 || cfg.enabled {
		t.Errorf("cfg = %+v, overrides not honoured", cfg)
	}
}

func TestRun_PropagatesLoadConfigErr(t *testing.T) {
	clearWorkerEnv(t)
	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("run with empty env = nil, want non-nil")
	}
}

func TestRun_DisabledExitsCleanly(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "postgres://user:pass@127.0.0.1:1/db")
	t.Setenv("DATABASE_MASTER_OPS_URL", "postgres://user:pass@127.0.0.1:1/db")
	t.Setenv("LGPD_RETENTION_ENABLED", "0")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestRun_BadRuntimeDSN(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "::not-a-valid-dsn::")
	t.Setenv("DATABASE_MASTER_OPS_URL", "postgres://y")
	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("run = %v, want runtime pool error", err)
	}
}

func TestRun_BadMasterDSN(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("DATABASE_MASTER_OPS_URL", "::not-a-valid-dsn::")
	err := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "master") {
		t.Fatalf("run = %v, want master pool error", err)
	}
}

func TestRunWith_DisabledLoopExitsOnCtxCancel(t *testing.T) {
	cfg := config{interval: time.Second, batch: 10, enabled: false, policy: lgpd.RetentionPolicy{FiscalYears: 5}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := runWith(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, stubRepo{}, stubRepo{})
	if err != nil {
		t.Fatalf("disabled runWith err = %v", err)
	}
}

func TestRunWith_RunsAndExitsOnCtxCancel(t *testing.T) {
	cfg := config{interval: 30 * time.Millisecond, batch: 10, enabled: true, policy: lgpd.RetentionPolicy{FiscalYears: 5}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	err := runWith(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, stubRepo{}, stubRepo{})
	if err != nil {
		t.Fatalf("enabled runWith err = %v", err)
	}
}

func TestRunWith_WorkerNewError(t *testing.T) {
	// Force lgpd_retention.New to error by sending a nil Purge.
	cfg := config{interval: time.Second, batch: 10, enabled: true, policy: lgpd.RetentionPolicy{FiscalYears: 5}}
	err := runWith(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, stubRepo{}, nil)
	if err == nil || !strings.Contains(err.Error(), "worker.New") {
		t.Fatalf("runWith with nil purge = %v, want worker.New err", err)
	}
}

func TestEnvInt_UnsetAndInvalidFallBackToDefault(t *testing.T) {
	if got := envInt("DOES_NOT_EXIST_LGPD_X", 7); got != 7 {
		t.Errorf("envInt unset = %d, want 7", got)
	}
	t.Setenv("LGPD_BAD", "not-a-number")
	if got := envInt("LGPD_BAD", 3); got != 3 {
		t.Errorf("envInt invalid = %d, want fallback 3", got)
	}
	t.Setenv("LGPD_GOOD", "42")
	if got := envInt("LGPD_GOOD", 0); got != 42 {
		t.Errorf("envInt = %d, want 42", got)
	}
}

func TestEnvDuration_UnsetInvalidAndValid(t *testing.T) {
	if got := envDuration("DOES_NOT_EXIST_LGPD_DUR", 5*time.Second); got != 5*time.Second {
		t.Errorf("envDuration unset = %s", got)
	}
	t.Setenv("LGPD_DUR_BAD", "not-a-duration")
	if got := envDuration("LGPD_DUR_BAD", 2*time.Second); got != 2*time.Second {
		t.Errorf("envDuration invalid = %s", got)
	}
	t.Setenv("LGPD_DUR_OK", "30s")
	if got := envDuration("LGPD_DUR_OK", 0); got != 30*time.Second {
		t.Errorf("envDuration = %s, want 30s", got)
	}
}
