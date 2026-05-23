package main

// SIN-63362 — boot-time gate tests for LGPDMasterOpsRequired. Lives in
// its own file (parallel to customdomain_redis_wire_test.go) so the
// existing lgpd_wire_test.go is untouched.
//
// The regression that motivated this gate: on staging, MASTER_OPS_DATABASE_URL
// was unset, buildLGPDStack returned the noop stack, and every
// /admin/lgpd/* route silently 404'd — the LGPD admin compliance
// surface was invisible. The gate fails boot closed instead so the
// operator sees the misconfiguration before serving the first
// request.

import (
	"errors"
	"testing"
)

func TestLGPDMasterOpsRequired_NilGetenv_NilError(t *testing.T) {
	t.Parallel()
	if err := LGPDMasterOpsRequired(nil); err != nil {
		t.Fatalf("nil getenv: expected nil error, got %v", err)
	}
}

func TestLGPDMasterOpsRequired_DevEnv_NilEvenWhenDSNUnset(t *testing.T) {
	t.Parallel()
	// Dev / CI / docker compose: APP_ENV is unset (or anything outside
	// {"staging","production"}). The gate stays permissive so unit
	// tests and local boots that legitimately have no master-ops pool
	// still come up — the existing noopLGPDStack path still handles
	// these.
	for _, env := range []string{"", "dev", "test", "stg", "prod", "STAGING", "PRODUCTION"} {
		env := env
		t.Run("APP_ENV="+env, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string {
				if k == envAppEnv {
					return env
				}
				return ""
			}
			if err := LGPDMasterOpsRequired(getenv); err != nil {
				t.Fatalf("APP_ENV=%q + DSN unset: expected nil, got %v", env, err)
			}
		})
	}
}

func TestLGPDMasterOpsRequired_StagingDSNUnset_HardErrors(t *testing.T) {
	t.Parallel()
	// AC: when CRM_ENV (= APP_ENV here, see envAppEnv doc) is staging
	// and MASTER_OPS_DATABASE_URL is empty, boot MUST fail with the
	// sentinel error. This is the exact pin requested by the SIN-63362
	// remediation section.
	getenv := func(k string) string {
		if k == envAppEnv {
			return appEnvStaging
		}
		return ""
	}
	err := LGPDMasterOpsRequired(getenv)
	if err == nil {
		t.Fatal("APP_ENV=staging + DSN unset: expected ErrLGPDMasterOpsRequired, got nil")
	}
	if !errors.Is(err, ErrLGPDMasterOpsRequired) {
		t.Fatalf("error is not ErrLGPDMasterOpsRequired: %v", err)
	}
}

func TestLGPDMasterOpsRequired_ProductionDSNUnset_HardErrors(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envAppEnv {
			return appEnvProduction
		}
		return ""
	}
	err := LGPDMasterOpsRequired(getenv)
	if err == nil {
		t.Fatal("APP_ENV=production + DSN unset: expected ErrLGPDMasterOpsRequired, got nil")
	}
	if !errors.Is(err, ErrLGPDMasterOpsRequired) {
		t.Fatalf("error is not ErrLGPDMasterOpsRequired: %v", err)
	}
}

func TestLGPDMasterOpsRequired_StagingDSNSet_NilError(t *testing.T) {
	t.Parallel()
	// Happy path on staging: DSN is configured, gate passes.
	getenv := func(k string) string {
		switch k {
		case envAppEnv:
			return appEnvStaging
		case envMasterOpsDSN:
			return "postgres://master:pw@db:5432/crm?sslmode=require"
		}
		return ""
	}
	if err := LGPDMasterOpsRequired(getenv); err != nil {
		t.Fatalf("staging + DSN set: expected nil, got %v", err)
	}
}

func TestLGPDMasterOpsRequired_ProductionDSNSet_NilError(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envAppEnv:
			return appEnvProduction
		case envMasterOpsDSN:
			return "postgres://master:pw@db:5432/crm?sslmode=require"
		}
		return ""
	}
	if err := LGPDMasterOpsRequired(getenv); err != nil {
		t.Fatalf("production + DSN set: expected nil, got %v", err)
	}
}

func TestLGPDMasterOpsRequired_StagingDSNWhitespaceOnly_HardErrors(t *testing.T) {
	t.Parallel()
	// Defence-in-depth: a DSN that is just whitespace (operator typo
	// in .env.stg) must trip the gate the same way an empty value
	// does — otherwise the pgxpool.New call later panics with a
	// useless URL-parse error instead of the operator-facing
	// "MASTER_OPS_DATABASE_URL is required" message.
	getenv := func(k string) string {
		switch k {
		case envAppEnv:
			return appEnvStaging
		case envMasterOpsDSN:
			return "   \t  "
		}
		return ""
	}
	err := LGPDMasterOpsRequired(getenv)
	if err == nil {
		t.Fatal("staging + whitespace DSN: expected ErrLGPDMasterOpsRequired, got nil")
	}
	if !errors.Is(err, ErrLGPDMasterOpsRequired) {
		t.Fatalf("error is not ErrLGPDMasterOpsRequired: %v", err)
	}
}
