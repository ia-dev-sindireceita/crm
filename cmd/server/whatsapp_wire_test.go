package main

import (
	"context"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
)

// buildWhatsAppWiring is the production assembly entry point and dials
// Postgres+Redis directly; covering the dial path here would re-test
// pgxpool / redis, so we focus on the env-gating contract: the wire
// MUST return nil when any required env var is unset. The happy-path
// is covered indirectly by the handler tests in
// internal/adapter/channels/whatsapp.

func TestBuildWhatsAppWiring_DisabledWhenSecretMissing(t *testing.T) {
	t.Parallel()
	got := buildWhatsAppWiring(context.Background(), func(string) string { return "" })
	if got != nil {
		t.Fatal("expected nil wiring when META_APP_SECRET unset")
	}
}

func TestBuildWhatsAppWiring_DisabledWhenVerifyTokenMissing(t *testing.T) {
	t.Parallel()
	got := buildWhatsAppWiring(context.Background(), func(k string) string {
		if k == whatsapp.EnvAppSecret {
			return "s"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when META_VERIFY_TOKEN unset")
	}
}

func TestBuildWhatsAppWiring_DisabledWhenDSNMissing(t *testing.T) {
	t.Parallel()
	got := buildWhatsAppWiring(context.Background(), func(k string) string {
		switch k {
		case whatsapp.EnvAppSecret:
			return "s"
		case whatsapp.EnvVerifyToken:
			return "v"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when DATABASE_URL unset")
	}
}

func TestBuildWhatsAppWiring_DisabledWhenRedisMissing(t *testing.T) {
	t.Parallel()
	got := buildWhatsAppWiring(context.Background(), func(k string) string {
		switch k {
		case whatsapp.EnvAppSecret:
			return "s"
		case whatsapp.EnvVerifyToken:
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
