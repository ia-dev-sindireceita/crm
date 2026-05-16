//go:build integration

package channeltest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Harness owns the shared Postgres pool, the migrations directory used to
// (re)apply the schema, and a cleanup hook that releases the container or
// external DSN on TestMain exit.
type Harness struct {
	pool          *pgxpool.Pool
	dsn           string
	migrationsDir string
	cleanup       func()
}

var (
	harnessOnce sync.Once
	harnessInst *Harness
	harnessErr  error
)

// Start returns the shared harness, lazily booting Postgres + applying
// every up migration on the first call. The process-wide singleton matches
// the webhook integration suite's pattern (one container per `go test`
// process so the wall-clock startup cost is paid once).
//
// Tests MUST NOT call cleanup directly; ReleaseOnExit installs an os.Exit
// hook the integration packages wire from their own TestMain.
func Start(t *testing.T) *Harness {
	t.Helper()
	harnessOnce.Do(func() {
		harnessInst, harnessErr = boot()
	})
	if harnessErr != nil {
		t.Fatalf("channeltest: harness boot: %v", harnessErr)
	}
	return harnessInst
}

// Pool returns the pgxpool the harness is bound to. Tests use it directly
// to seed tenant rows and assert on the projected state.
func (h *Harness) Pool() *pgxpool.Pool {
	return h.pool
}

// DSN returns the connection string the pool was opened against. Useful
// when a test needs a second pool (e.g. to verify FK behaviour with a
// non-superuser).
func (h *Harness) DSN() string {
	return h.dsn
}

// Truncate clears every inbox / dedup / tenant table touched by a webhook
// E2E so a follow-up test starts from a known empty state. The schema is
// preserved. Every TRUNCATE uses CASCADE so dependent tables not in the
// list (assignment, funnel_transition, identity_link, identity, …) get
// wiped via FK propagation instead of erroring out mid-list.
func (h *Harness) Truncate(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stmts := []string{
		`TRUNCATE TABLE inbound_message_dedup CASCADE`,
		`TRUNCATE TABLE assignment_history CASCADE`,
		`TRUNCATE TABLE message CASCADE`,
		`TRUNCATE TABLE conversation CASCADE`,
		`TRUNCATE TABLE contact_channel_identity CASCADE`,
		`TRUNCATE TABLE contact CASCADE`,
		`TRUNCATE TABLE tenant_channel_associations CASCADE`,
		`TRUNCATE TABLE users CASCADE`,
		`TRUNCATE TABLE tenants CASCADE`,
	}
	for _, s := range stmts {
		if _, err := h.pool.Exec(ctx, s); err != nil {
			t.Fatalf("channeltest: truncate %q: %v", s, err)
		}
	}
}

// ReleaseOnExit installs an os.Exit-time hook that closes the pool and
// stops the container if one was started. Call once from each
// integration package's TestMain.
func ReleaseOnExit() func() {
	return func() {
		if harnessInst != nil && harnessInst.cleanup != nil {
			harnessInst.cleanup()
		}
	}
}

func boot() (*Harness, error) {
	migrationsDir, err := findMigrationsDir()
	if err != nil {
		return nil, err
	}
	if dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN")); dsn != "" {
		return openExternal(dsn, migrationsDir)
	}
	return startContainer(migrationsDir)
}

// openExternal opens a pool against a DSN the caller supplies (CI service
// container, bring-your-own dev DB). To isolate from the webhook
// integration suite that CI runs first against the same TEST_POSTGRES_DSN,
// the channel suite creates its own dedicated database (<original>_channels).
//
// Attempts to reset the schema in place (DROP EXTENSION + DROP SCHEMA) fail
// because the pg_extension catalog row is the parent of its schema-resident
// functions in pg_depend, not a dependent — DROP SCHEMA CASCADE leaves the
// pg_extension row with a stale extnamespace OID, and subsequent DROP
// EXTENSION IF EXISTS silently no-ops on the orphaned row, so
// CREATE EXTENSION trips pg_extension_name_index. A fresh, empty database
// avoids all extension/schema residue by construction.
func openExternal(dsn, migrationsDir string) (*Harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN %q: %w", dsn, err)
	}

	// Derive a per-process channel-test database name. Including the PID
	// ensures that when go test runs multiple package binaries concurrently
	// (the default) each binary gets a unique, isolated database instead of
	// racing to CREATE the same name.
	channelDB := fmt.Sprintf("%s_channels_%d", cfg.ConnConfig.Database, os.Getpid())
	adminCfg := cfg.Copy()
	adminCfg.ConnConfig.Database = "postgres"

	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		return nil, fmt.Errorf("open admin pool: %w", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		return nil, fmt.Errorf("ping admin DB: %w", err)
	}
	// Terminate any leftover connections from a previous aborted run before
	// dropping so the DROP does not block.
	_, _ = adminPool.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		channelDB,
	)
	if _, err := adminPool.Exec(ctx, `DROP DATABASE IF EXISTS `+channelDB); err != nil {
		adminPool.Close()
		return nil, fmt.Errorf("drop channel DB %q: %w", channelDB, err)
	}
	if _, err := adminPool.Exec(ctx, `CREATE DATABASE `+channelDB); err != nil {
		adminPool.Close()
		return nil, fmt.Errorf("create channel DB %q: %w", channelDB, err)
	}
	adminPool.Close()

	// Open the main pool against the freshly created database.
	channelCfg := cfg.Copy()
	channelCfg.ConnConfig.Database = channelDB
	pool, err := pgxpool.NewWithConfig(ctx, channelCfg)
	if err != nil {
		return nil, fmt.Errorf("open channel pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping channel DB %q: %w", channelDB, err)
	}

	h := &Harness{
		pool:          pool,
		dsn:           channelCfg.ConnString(),
		migrationsDir: migrationsDir,
		cleanup: func() {
			pool.Close()
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cleanCancel()
			ap, apErr := pgxpool.NewWithConfig(cleanCtx, adminCfg)
			if apErr != nil {
				return
			}
			_, _ = ap.Exec(cleanCtx,
				`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
				channelDB,
			)
			_, _ = ap.Exec(cleanCtx, `DROP DATABASE IF EXISTS `+channelDB)
			ap.Close()
		},
	}
	if err := applyMigrations(ctx, pool, migrationsDir, "up"); err != nil {
		h.cleanup()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return h, nil
}

// startContainer boots a postgres:16-alpine testcontainer dedicated to
// this test process. Reused by local-dev runs that don't have a shared
// CI service container available.
func startContainer(migrationsDir string) (*Harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("crmtest"),
		tcpostgres.WithUsername("crm"),
		tcpostgres.WithPassword("crm"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("testcontainers postgres: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(context.Background())
		return nil, fmt.Errorf("container DSN: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = container.Terminate(context.Background())
		return nil, fmt.Errorf("pgxpool.New (container): %w", err)
	}

	h := &Harness{
		pool:          pool,
		dsn:           dsn,
		migrationsDir: migrationsDir,
		cleanup: func() {
			pool.Close()
			_ = container.Terminate(context.Background())
		},
	}
	if err := applyMigrations(ctx, pool, migrationsDir, "up"); err != nil {
		h.cleanup()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return h, nil
}

// findMigrationsDir walks up from this source file's directory looking
// for a sibling `migrations/` that contains the canonical inbox schema
// migration (0088). The repo layout is stable enough that the upward
// walk terminates in at most a handful of steps.
func findMigrationsDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("channeltest: cannot resolve caller path")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "migrations")
		if matches, _ := filepath.Glob(filepath.Join(candidate, "0088_inbox_contacts.up.sql")); len(matches) > 0 {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("channeltest: migrations/0088_inbox_contacts.up.sql not found")
}

// applyMigrations executes every *.up.sql (or reverse-ordered *.down.sql)
// from migrationsDir against pool. Each file runs as a single
// pool.Exec — none of the migrations we currently ship use CREATE INDEX
// CONCURRENTLY so an implicit transaction per file matches the production
// goose flow closely enough for our integration coverage.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, migrationsDir, direction string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("channeltest: invalid migration direction %q", direction)
	}
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return err
	}
	suffix := "." + direction + ".sql"
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	if direction == "down" {
		for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
			files[i], files[j] = files[j], files[i]
		}
	}
	for _, name := range files {
		path := filepath.Join(migrationsDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("exec %s: %w", name, err)
		}
	}
	return nil
}
