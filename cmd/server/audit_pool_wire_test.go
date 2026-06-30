package main

import (
	"context"
	"testing"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

// auditExecutorOr must return the runtime fallback (and avoid the typed-nil
// interface trap) when the dedicated audit pool is absent — the dev path.
// Reuses fakeAuditExecutor from iam_authz_wire_test.go.
func TestAuditExecutorOr_NilPoolFallsBackToRuntime(t *testing.T) {
	t.Parallel()

	fallback := &fakeAuditExecutor{}
	got := auditExecutorOr(nil, fallback)
	if got == nil {
		t.Fatal("got nil AuditExecutor, want the runtime fallback")
	}
	if got != postgresadapter.AuditExecutor(fallback) {
		t.Fatalf("got %T, want the runtime fallback", got)
	}
}

// buildAuditPool is fail-soft when AUDIT_DATABASE_URL is unset: nil pool, no
// panic — cmd/server then routes audit writes through the runtime pool in dev.
func TestBuildAuditPool_UnsetReturnsNil(t *testing.T) {
	t.Parallel()

	getenv := func(k string) string {
		if k == postgresadapter.EnvAuditDSN {
			return ""
		}
		return ""
	}
	if pool := buildAuditPool(context.Background(), getenv); pool != nil {
		pool.Close()
		t.Fatal("got a non-nil pool for unset AUDIT_DATABASE_URL, want nil")
	}
}
