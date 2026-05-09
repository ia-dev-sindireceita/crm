// Package testpg spins up a real Postgres 14+ cluster for integration tests
// and applies the SIN-62232 migrations against it. No Docker required: we
// shell out to the system pg_ctl binary, which the local dev environment and
// the CI postgres:16 service both ship.
//
// Two modes:
//
//   - TEST_DATABASE_URL set: connect to an existing Postgres (CI service /
//     bring-your-own). The cluster is shared; each test binary gets its own
//     fresh DB to avoid cross-test bleed.
//   - TEST_DATABASE_URL unset: pg_ctl initdb a brand-new cluster in a
//     tempdir, start it on an ephemeral port, tear it down on TestMain exit.
//
// In both modes we apply migrations/0001_roles.up.sql as the superuser and
// migrations/0002_token_ledger.up.sql as app_admin, matching the operational
// posture documented in docs/adr/0071-postgres-roles.md.
package testpg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// sharedPassword is the deterministic password used when multiple test
// binaries attach to the same external Postgres (TEST_DATABASE_URL mode).
// All processes must agree on it: each binary's bootstrap ALTERs the
// application roles' passwords, and a per-process random would let two
// concurrent `go test ./...` packages overwrite each other and break the
// loser's pools with SASL auth failures (SIN-62391). Test DBs are not
// long-lived and the cluster itself is gated by the superuser DSN, so a
// constant password adds no real risk.
const sharedPassword = "testpg_shared_app_password"

// ephemeralPassword is the password used when this process owns its own
// pg_ctl cluster. It is generated once per process so logs/dumps from one
// dev test run cannot replay against another. Only meaningful in ephemeral
// mode, where the cluster is private to this process.
var ephemeralPassword = "test_" + randHex(16)

// dbCounter gives every test database in a process a unique name without
// requiring tests to coordinate.
var dbCounter atomic.Uint64

// pgBinSearchPaths lists the well-known locations where Debian/Ubuntu install
// pg_ctl, in version-descending order. PATH is consulted first.
var pgBinSearchPaths = []string{
	"/usr/lib/postgresql/16/bin",
	"/usr/lib/postgresql/15/bin",
	"/usr/lib/postgresql/14/bin",
}

// Harness is the live test-postgres environment. It is safe to share across
// tests in the same package: every test should call DB(t) to get its own
// freshly-migrated database.
type Harness struct {
	superuserDSN string
	host         string
	port         int
	stopCluster  func() error
	migrationDir string
	// appPassword is the password used to authenticate the application
	// roles (app_runtime / app_admin / app_master_ops) against this
	// harness. It is sharedPassword when attached to an external Postgres
	// (TEST_DATABASE_URL set) so concurrent test binaries can't fight over
	// ALTER ROLE PASSWORD, and ephemeralPassword for a private pg_ctl
	// cluster owned by this process.
	appPassword string
}

// Start brings up (or attaches to) a Postgres cluster and applies the
// SIN-62232 bootstrap migrations against a template database. Tests obtain a
// per-test database via Harness.DB.
//
// Start is intended to be called from TestMain. The returned Harness is safe
// to share across goroutines.
func Start(ctx context.Context) (*Harness, error) {
	migrations, err := findMigrationsDir()
	if err != nil {
		return nil, fmt.Errorf("testpg: locate migrations: %w", err)
	}

	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		host, port, err := parseHostPort(dsn)
		if err != nil {
			return nil, fmt.Errorf("testpg: parse TEST_DATABASE_URL: %w", err)
		}
		h := &Harness{
			superuserDSN: dsn,
			host:         host,
			port:         port,
			stopCluster:  func() error { return nil },
			migrationDir: migrations,
			appPassword:  sharedPassword,
		}
		if err := h.bootstrap(ctx); err != nil {
			return nil, err
		}
		return h, nil
	}

	cluster, err := startEphemeralCluster(ctx)
	if err != nil {
		return nil, err
	}
	h := &Harness{
		superuserDSN: cluster.dsn,
		host:         cluster.host,
		port:         cluster.port,
		stopCluster:  cluster.stop,
		migrationDir: migrations,
		appPassword:  ephemeralPassword,
	}
	if err := h.bootstrap(ctx); err != nil {
		_ = cluster.stop()
		return nil, err
	}
	return h, nil
}

// Stop tears the cluster down (no-op when attached to an external Postgres).
func (h *Harness) Stop() error {
	if h == nil || h.stopCluster == nil {
		return nil
	}
	return h.stopCluster()
}

// DB creates a fresh database, applies migrations 0002+0003 to it (0001 is
// applied once at TestMain time because CREATE ROLE is cluster-scoped), and
// returns pools for each application role. Cleanup runs on t.Cleanup.
func (h *Harness) DB(t *testing.T) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("crm_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	if err := h.execSuperuser(ctx, fmt.Sprintf(`CREATE DATABASE %q OWNER app_admin`, dbName)); err != nil {
		t.Fatalf("testpg: create database %s: %v", dbName, err)
	}

	if err := h.applyMigrationAs(ctx, dbName, "app_admin", "0002_master_ops_audit.up.sql"); err != nil {
		t.Fatalf("testpg: apply 0002 in %s: %v", dbName, err)
	}
	if err := h.applyMigrationAs(ctx, dbName, "app_admin", "0003_token_ledger.up.sql"); err != nil {
		t.Fatalf("testpg: apply 0003 in %s: %v", dbName, err)
	}

	db := &DB{
		harness: h,
		name:    dbName,
	}
	db.runtimePool = mustPool(t, h.dsnAs(dbName, "app_runtime"))
	db.adminPool = mustPool(t, h.dsnAs(dbName, "app_admin"))
	db.masterPool = mustPool(t, h.dsnAs(dbName, "app_master_ops"))
	db.superuserPool = mustPool(t, h.dsnFor(dbName, ""))

	t.Cleanup(func() {
		db.runtimePool.Close()
		db.adminPool.Close()
		db.masterPool.Close()
		db.superuserPool.Close()

		// Best-effort drop. If a test leaks a connection we don't care for the
		// purposes of cleanup; the cluster gets torn down at the end anyway.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = h.execSuperuser(dropCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, dbName))
	})
	return db
}

// SuperuserDSN returns the DSN to the postgres maintenance database as the
// cluster superuser. Useful for assertions that need to inspect pg_catalog
// without an RLS-bearing role in the picture.
func (h *Harness) SuperuserDSN() string { return h.superuserDSN }

// MigrationsDir reports the absolute path to the migrations directory. Tests
// that exercise the down migrations need this.
func (h *Harness) MigrationsDir() string { return h.migrationDir }

// DB is one isolated test database with pre-built pools for each app role.
type DB struct {
	harness       *Harness
	name          string
	runtimePool   *pgxpool.Pool
	adminPool     *pgxpool.Pool
	masterPool    *pgxpool.Pool
	superuserPool *pgxpool.Pool
}

// Name returns the database name (useful in error messages).
func (d *DB) Name() string { return d.name }

// RuntimePool is the pgxpool.Pool connecting as app_runtime — the role the
// production app uses. RLS applies; BYPASSRLS=false.
func (d *DB) RuntimePool() *pgxpool.Pool { return d.runtimePool }

// AdminPool connects as app_admin (BYPASSRLS=true). Use for setup and for
// "would the migrator see this" assertions.
func (d *DB) AdminPool() *pgxpool.Pool { return d.adminPool }

// MasterOpsPool connects as app_master_ops (BYPASSRLS=true with audit).
func (d *DB) MasterOpsPool() *pgxpool.Pool { return d.masterPool }

// SuperuserPool connects to this test DB as the cluster superuser. Used by
// tests that need to ALTER TABLE / DISABLE RLS to prove FORCE ROW LEVEL
// SECURITY can't be bypassed by ownership.
func (d *DB) SuperuserPool() *pgxpool.Pool { return d.superuserPool }

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (h *Harness) bootstrap(ctx context.Context) error {
	rolesSQL, err := os.ReadFile(filepath.Join(h.migrationDir, "0001_roles.up.sql"))
	if err != nil {
		return fmt.Errorf("read 0001_roles.up.sql: %w", err)
	}

	// Serialize the cluster-global bootstrap (CREATE ROLE / ALTER ROLE) under
	// a Postgres advisory lock. Without it, two `go test ./...` packages
	// attached to the same external Postgres race each other:
	// concurrent CREATE ROLE inside 0001's DO blocks throws
	// `tuple concurrently updated`, and concurrent ALTER ROLE PASSWORD lets
	// the loser's pools fail SASL auth with the stale password (SIN-62391).
	// The advisory lock id is an arbitrary constant; what matters is that
	// every binary uses the same one. The lock is released when the
	// connection is closed (session-scoped advisory locks).
	pool, err := pgxpool.New(ctx, h.superuserDSN)
	if err != nil {
		return fmt.Errorf("connect superuser: %w", err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire bootstrap conn: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", testpgBootstrapLockID); err != nil {
		return fmt.Errorf("acquire bootstrap advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", testpgBootstrapLockID)
	}()

	if _, err := conn.Exec(ctx, string(rolesSQL)); err != nil {
		return fmt.Errorf("apply 0001 as superuser: %w", err)
	}

	// Tests need to log in as app_runtime / app_admin / app_master_ops with
	// h.appPassword. In shared (TEST_DATABASE_URL) mode this is a constant
	// every test binary agrees on, so concurrent bootstraps are idempotent
	// and don't break the loser's pools (SIN-62391). In ephemeral mode the
	// cluster is private to this process so the password is a per-process
	// random value.
	for _, role := range []string{"app_runtime", "app_admin", "app_master_ops"} {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`ALTER ROLE %s WITH PASSWORD '%s'`, role, h.appPassword)); err != nil {
			return fmt.Errorf("set password for %s: %w", role, err)
		}
	}
	return nil
}

// testpgBootstrapLockID is the arbitrary constant advisory-lock id used to
// serialize cluster-global bootstrap across concurrent test binaries.
// Postgres advisory locks are process-wide; using the same constant
// everywhere is what makes them serialize.
const testpgBootstrapLockID int64 = 0x53494e3632333931

// applyMigrationAs runs a migration file against dbName as the given role.
// Empty role means "use the cluster superuser DSN".
func (h *Harness) applyMigrationAs(ctx context.Context, dbName, role, file string) error {
	sql, err := os.ReadFile(filepath.Join(h.migrationDir, file))
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	pool, err := pgxpool.New(ctx, h.dsnFor(dbName, role))
	if err != nil {
		return fmt.Errorf("connect as %q: %w", role, err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		return fmt.Errorf("exec %s as %q: %w", file, role, err)
	}
	return nil
}

func (h *Harness) execSuperuser(ctx context.Context, sql string) error {
	pool, err := pgxpool.New(ctx, h.superuserDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("exec %q: %w", firstLine(sql), err)
	}
	return nil
}

// dsnAs builds a DSN for the given role + password against the harness host.
func (h *Harness) dsnAs(dbName, role string) string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		h.host, h.port, role, h.appPassword, dbName)
}

// dsnFor builds a DSN for the cluster superuser. Empty role means "use the
// superuser DSN's existing user".
func (h *Harness) dsnFor(dbName, role string) string {
	if role != "" {
		return h.dsnAs(dbName, role)
	}
	return replaceDB(h.superuserDSN, dbName)
}

// ---------------------------------------------------------------------------
// pg_ctl-based ephemeral cluster
// ---------------------------------------------------------------------------

type ephemeralCluster struct {
	dataDir string
	host    string
	port    int
	dsn     string
	stop    func() error
}

func startEphemeralCluster(ctx context.Context) (*ephemeralCluster, error) {
	pgBin, err := findPgBinDir()
	if err != nil {
		return nil, err
	}

	dataDir, err := os.MkdirTemp("", "crm-testpg-")
	if err != nil {
		return nil, fmt.Errorf("mkdir tempdir: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(dataDir)
	}

	port, err := pickFreePort()
	if err != nil {
		cleanup()
		return nil, err
	}

	if err := runCmd(ctx, exec.Command(filepath.Join(pgBin, "initdb"),
		"-D", dataDir,
		"-A", "trust",
		"-U", "postgres",
		"-E", "UTF8",
		"--locale=C",
	)); err != nil {
		cleanup()
		return nil, fmt.Errorf("initdb: %w", err)
	}

	logFile := filepath.Join(dataDir, "server.log")
	startCmd := exec.Command(filepath.Join(pgBin, "pg_ctl"),
		"-D", dataDir,
		"-l", logFile,
		"-o", fmt.Sprintf("-p %d -k %s -h 127.0.0.1 -F", port, dataDir),
		"-w",
		"start",
	)
	if err := runCmd(ctx, startCmd); err != nil {
		// dump log so test failures are debuggable
		if data, _ := os.ReadFile(logFile); len(data) > 0 {
			err = fmt.Errorf("%w\n--- server.log ---\n%s", err, data)
		}
		cleanup()
		return nil, fmt.Errorf("pg_ctl start: %w", err)
	}

	stop := func() error {
		stopCmd := exec.Command(filepath.Join(pgBin, "pg_ctl"),
			"-D", dataDir, "-m", "immediate", "-w", "stop")
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runCmd(stopCtx, stopCmd)
		cleanup()
		return nil
	}

	dsn := fmt.Sprintf("host=127.0.0.1 port=%d user=postgres dbname=postgres sslmode=disable", port)

	// Wait for connectivity.
	if err := waitForPostgres(ctx, dsn, 20*time.Second); err != nil {
		_ = stop()
		return nil, err
	}
	return &ephemeralCluster{dataDir: dataDir, host: "127.0.0.1", port: port, dsn: dsn, stop: stop}, nil
}

func findPgBinDir() (string, error) {
	if env := os.Getenv("PG_BIN"); env != "" {
		if _, err := os.Stat(filepath.Join(env, "pg_ctl")); err == nil {
			return env, nil
		}
	}
	if path, err := exec.LookPath("pg_ctl"); err == nil {
		return filepath.Dir(path), nil
	}
	for _, p := range pgBinSearchPaths {
		if _, err := os.Stat(filepath.Join(p, "pg_ctl")); err == nil {
			return p, nil
		}
	}
	return "", errors.New("testpg: pg_ctl not found; install postgresql-client or set PG_BIN")
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func runCmd(ctx context.Context, cmd *exec.Cmd) error {
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stderr
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func waitForPostgres(ctx context.Context, dsn string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			pool.Close()
			if err == nil {
				return nil
			}
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timed out")
	}
	return fmt.Errorf("wait for postgres: %w", lastErr)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// findMigrationsDir walks up from the current package directory until it
// finds a directory containing both `go.mod` and `migrations/`.
func findMigrationsDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		mod := filepath.Join(dir, "go.mod")
		mig := filepath.Join(dir, "migrations")
		if _, err := os.Stat(mod); err == nil {
			if info, err := os.Stat(mig); err == nil && info.IsDir() {
				return mig, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("testpg: migrations/ not found above %s", cwd)
}

func mustPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testpg: connect %s: %v", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("testpg: ping %s: %v", dsn, err)
	}
	return pool
}

func parseHostPort(dsn string) (string, int, error) {
	host := "127.0.0.1"
	port := 5432
	for _, kv := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "host":
			host = v
		case "port":
			if _, err := fmt.Sscanf(v, "%d", &port); err != nil {
				return "", 0, fmt.Errorf("bad port %q: %w", v, err)
			}
		}
	}
	return host, port, nil
}

func replaceDB(dsn, dbName string) string {
	parts := strings.Fields(dsn)
	out := parts[:0]
	replaced := false
	for _, p := range parts {
		if strings.HasPrefix(p, "dbname=") {
			out = append(out, "dbname="+dbName)
			replaced = true
		} else {
			out = append(out, p)
		}
	}
	if !replaced {
		out = append(out, "dbname="+dbName)
	}
	return strings.Join(out, " ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

// randHex returns a random lowercase hex string of length n. Used for
// per-process passwords; not cryptographic-grade but adequate for test
// isolation.
func randHex(n int) string {
	const alphabet = "0123456789abcdef"
	b := make([]byte, n)
	now := time.Now().UnixNano()
	for i := range b {
		b[i] = alphabet[(now>>uint(i*4))&0xf]
	}
	return string(b)
}
