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
// preserved. CASCADE handles the FKs between tenant → contact →
// conversation → message → assignment_history.
func (h *Harness) Truncate(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stmts := []string{
		`TRUNCATE TABLE inbound_message_dedup`,
		`TRUNCATE TABLE assignment_history`,
		`TRUNCATE TABLE message`,
		`TRUNCATE TABLE conversation`,
		`TRUNCATE TABLE contact_channel_identity`,
		`TRUNCATE TABLE contact`,
		`TRUNCATE TABLE tenant_channel_associations`,
		// users + tenants live behind FKs from rows we just dropped; cascade
		// keeps the slate clean for the next case.
		`TRUNCATE TABLE users CASCADE`,
		`TRUNCATE TABLE tenants CASCADE`,
	}
	for _, s := range stmts {
		if _, err := h.pool.Exec(ctx, s); err != nil {
			// Some tables may not exist on very early migration cuts; skip
			// silently rather than failing the suite — the per-test schema
			// invariant comes from later migrations.
			if strings.Contains(err.Error(), "does not exist") {
				continue
			}
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
// container, bring-your-own dev DB). Migrations are applied unconditionally
// and the cleanup hook rolls them back, so re-runs against a shared DB
// stay safe.
func openExternal(dsn, migrationsDir string) (*Harness, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %q: %w", dsn, err)
	}
	h := &Harness{
		pool:          pool,
		dsn:           dsn,
		migrationsDir: migrationsDir,
		cleanup: func() {
			_ = applyMigrations(context.Background(), pool, migrationsDir, "down")
			pool.Close()
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
