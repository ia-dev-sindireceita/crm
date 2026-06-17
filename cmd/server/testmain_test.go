package main

import (
	"os"
	"testing"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

// TestMain bounds the boot-time DB ping retry budget for the whole cmd/server
// test binary. Many wire tests point DATABASE_URL at an unreachable host
// (e.g. postgres://x) and exercise the real boot path, which funnels through
// pgpool.New. Production retries that ping for the full defaultPingRetryBudget
// (60s) so the pool self-heals when Postgres is still starting after a reboot
// (SIN-65041). Left unbounded in tests, ~N independent boot pools × 60s would
// blow the 10-minute go-test timeout, so we shrink the budget to 1ms here:
// each unreachable pool fails fast and total boot stays sub-second, while
// production keeps its full self-heal window. pgpool.New reads this via
// os.Getenv, so it applies to every boot path regardless of the injected
// getenv fixture.
func TestMain(m *testing.M) {
	os.Setenv(pgpool.EnvPingRetryBudget, "1ms")
	os.Exit(m.Run())
}
