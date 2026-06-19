package main

// SIN-65254 — composition tests for buildMasterLogin. The credential
// resolution + lockout logic is covered against a live cluster in
// internal/adapter/db/postgres (master_credential_reader_test.go) and
// internal/iam (master_login_test.go); these pin the wire-level fail-soft
// contract: a missing/invalid MASTER_OPS_DATABASE_URL or MASTER_OPS_ACTOR_ID
// yields a nil login fn + nil pool (so buildMasterMFAStack falls back to the
// noop stack and /m/* stays unmounted), and a valid pair assembles the fn.

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

func TestBuildMasterLogin_MissingInputs_ReturnsNil(t *testing.T) {
	t.Parallel()
	validActor := uuid.New().String()
	goodDSN := "postgres://u:p@localhost:5432/crm"

	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "dsn unset", env: map[string]string{envMasterOpsActorID: validActor}},
		{name: "actor unset", env: map[string]string{envMasterOpsDSN: goodDSN}},
		{name: "actor invalid", env: map[string]string{envMasterOpsDSN: goodDSN, envMasterOpsActorID: "not-a-uuid"}},
		{name: "actor nil-uuid", env: map[string]string{envMasterOpsDSN: goodDSN, envMasterOpsActorID: uuid.Nil.String()}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn, pool, cleanup := buildMasterLogin(context.Background(), nil, nil, nil, slog.Default(), envFunc(tc.env))
			defer cleanup()
			if fn != nil {
				t.Errorf("expected nil login fn for %q", tc.name)
			}
			if pool != nil {
				t.Errorf("expected nil pool for %q", tc.name)
			}
		})
	}
}

func TestBuildMasterLogin_ValidInputs_Assembles(t *testing.T) {
	t.Parallel()
	// pgxpool.New is lazy — it parses the DSN but does not dial until first
	// use, so a syntactically-valid DSN assembles the fn + pool without a
	// live cluster. The credential reader / lockouts constructors only
	// nil-check their inputs, so the stack is fully built here.
	env := envFunc(map[string]string{
		envMasterOpsDSN:     "postgres://u:p@localhost:5432/crm",
		envMasterOpsActorID: uuid.New().String(),
	})
	fn, pool, cleanup := buildMasterLogin(context.Background(), nil, nil, nil, slog.Default(), env)
	defer cleanup()
	if fn == nil {
		t.Fatal("expected non-nil master login fn")
	}
	if pool == nil {
		t.Fatal("expected non-nil master-ops pool")
	}
}

func TestBuildMasterLogin_MalformedDSN_ReturnsNil(t *testing.T) {
	t.Parallel()
	env := envFunc(map[string]string{
		envMasterOpsDSN:     "::::not-a-dsn",
		envMasterOpsActorID: uuid.New().String(),
	})
	fn, pool, cleanup := buildMasterLogin(context.Background(), nil, nil, nil, slog.Default(), env)
	defer cleanup()
	if fn != nil || pool != nil {
		t.Fatal("malformed DSN must yield nil fn + nil pool")
	}
}
