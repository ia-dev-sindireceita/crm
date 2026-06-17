package main

import (
	"context"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

// buildInstagramWiring is the production assembly entry point and dials
// Postgres+Redis directly; covering the dial path here would re-test
// pgxpool / redis, so we focus on the env-gating contract: the wire
// MUST return nil when any required env var is unset. The happy-path
// (HMAC → dedup → ReceiveInbound → Postgres) is covered end-to-end by
// the integration test in internal/adapter/channels/instagram.

func TestBuildInstagramWiring_DisabledWhenSecretMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramWiring(context.Background(), func(string) string { return "" })
	if got != nil {
		t.Fatal("expected nil wiring when META_APP_SECRET unset")
	}
}

func TestBuildInstagramWiring_DisabledWhenVerifyTokenMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramWiring(context.Background(), func(k string) string {
		if k == instagram.EnvAppSecret {
			return "s"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when META_INSTAGRAM_VERIFY_TOKEN unset")
	}
}

func TestBuildInstagramWiring_DisabledWhenDSNMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramWiring(context.Background(), func(k string) string {
		switch k {
		case instagram.EnvAppSecret:
			return "s"
		case instagram.EnvVerifyToken:
			return "v"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when DATABASE_URL unset")
	}
}

func TestBuildInstagramWiring_DisabledWhenRedisMissing(t *testing.T) {
	t.Parallel()
	got := buildInstagramWiring(context.Background(), func(k string) string {
		switch k {
		case instagram.EnvAppSecret:
			return "s"
		case instagram.EnvVerifyToken:
			return "v"
		case "DATABASE_URL":
			return "postgres://x"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when REDIS_URL unset")
	}
}
