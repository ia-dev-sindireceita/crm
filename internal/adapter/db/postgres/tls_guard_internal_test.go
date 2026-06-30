package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// recordTLSLogf returns a logf that appends each formatted line to dst so a
// test can assert the guard WARNed (or did not).
func recordTLSLogf(dst *[]string) func(string, ...any) {
	return func(format string, args ...any) {
		*dst = append(*dst, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}
}

// TestSSLModeFromDSN covers both DSN forms and the absent/malformed cases.
func TestSSLModeFromDSN(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"url require", "postgres://u:p@host:5432/db?sslmode=require", "require"},
		{"url verify-full", "postgresql://u:p@host:5432/db?sslmode=verify-full", "verify-full"},
		{"url disable", "postgres://u:p@host:5432/db?sslmode=disable", "disable"},
		{"url uppercase normalised", "postgres://u:p@host:5432/db?sslmode=REQUIRE", "require"},
		{"url absent", "postgres://u:p@host:5432/db", ""},
		{"url extra params", "postgres://u:p@host:5432/db?application_name=crm&sslmode=require", "require"},
		{"keyword require", "host=host user=u password=p dbname=db sslmode=require", "require"},
		{"keyword disable", "host=host sslmode=disable", "disable"},
		{"keyword quoted", "host=host sslmode='require'", "require"},
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"malformed url fails closed", "postgres://u:p@ho st:5432/db?sslmode=require", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sslModeFromDSN(tc.dsn); got != tc.want {
				t.Errorf("sslModeFromDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}

// TestAssertDSNTLS_Policy is the core regression: insecure sslmode hard-fails
// under enforcement and only WARNs without it; secure modes pass silently.
func TestAssertDSNTLS_Policy(t *testing.T) {
	t.Parallel()

	insecure := []string{
		"postgres://u:p@postgres:5432/crm?sslmode=disable",
		"postgres://u:p@postgres:5432/crm?sslmode=allow",
		"postgres://u:p@postgres:5432/crm?sslmode=prefer",
		"postgres://u:p@postgres:5432/crm", // absent → libpq `prefer` (cleartext fallback)
	}
	for _, dsn := range insecure {
		dsn := dsn
		t.Run("enforce hard-fails "+dsn, func(t *testing.T) {
			t.Parallel()
			var warns []string
			err := assertDSNTLS("DATABASE_URL", dsn, true, recordTLSLogf(&warns))
			if !errors.Is(err, ErrInsecureDBTLS) {
				t.Fatalf("dsn %q under enforcement: got %v, want ErrInsecureDBTLS", dsn, err)
			}
			if !strings.Contains(err.Error(), "DATABASE_URL") {
				t.Errorf("err %q should name the offending env var", err)
			}
			if !strings.Contains(err.Error(), "require") {
				t.Errorf("err %q should hint the fix (sslmode=require)", err)
			}
			if len(warns) == 0 {
				t.Error("expected a structured WARNING even under enforcement")
			}
		})
		t.Run("warn-only without enforcement "+dsn, func(t *testing.T) {
			t.Parallel()
			var warns []string
			if err := assertDSNTLS("DATABASE_URL", dsn, false, recordTLSLogf(&warns)); err != nil {
				t.Fatalf("dsn %q without enforcement: got %v, want nil (warn-only)", dsn, err)
			}
			if len(warns) != 1 {
				t.Fatalf("expected exactly 1 WARNING, got %d: %v", len(warns), warns)
			}
			if strings.Contains(warns[0], ":p@") || strings.Contains(warns[0], "password") {
				t.Errorf("WARNING %q must not echo DSN credentials", warns[0])
			}
		})
	}

	secure := []string{
		"postgres://u:p@db.example.internal:5432/crm?sslmode=require",
		"postgres://u:p@db.example.internal:5432/crm?sslmode=verify-ca",
		"postgres://u:p@db.example.internal:5432/crm?sslmode=verify-full",
		"host=db user=u sslmode=require",
	}
	for _, dsn := range secure {
		dsn := dsn
		t.Run("secure passes "+dsn, func(t *testing.T) {
			t.Parallel()
			var warns []string
			if err := assertDSNTLS("DATABASE_URL", dsn, true, recordTLSLogf(&warns)); err != nil {
				t.Fatalf("secure dsn %q under enforcement: got %v, want nil", dsn, err)
			}
			if len(warns) != 0 {
				t.Errorf("secure dsn should not WARN, got %v", warns)
			}
		})
	}
}

// TestAssertDSNTLS_EmptyDSN: an unset env var is a no-op (no warn, no error).
func TestAssertDSNTLS_EmptyDSN(t *testing.T) {
	t.Parallel()
	var warns []string
	if err := assertDSNTLS("WA_SESSION_DATABASE_URL", "", true, recordTLSLogf(&warns)); err != nil {
		t.Fatalf("empty dsn: got %v, want nil", err)
	}
	if len(warns) != 0 {
		t.Errorf("empty dsn should not WARN, got %v", warns)
	}
}

// TestAssertDatabaseTLSFromEnv_Integration drives the env-facing entry point.
func TestAssertDatabaseTLSFromEnv_Integration(t *testing.T) {
	t.Parallel()

	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("nil getenv is a no-op", func(t *testing.T) {
		t.Parallel()
		if err := AssertDatabaseTLSFromEnv(nil); err != nil {
			t.Errorf("nil getenv: got %v, want nil", err)
		}
	})

	t.Run("all unset is a no-op", func(t *testing.T) {
		t.Parallel()
		if err := AssertDatabaseTLSFromEnv(env(map[string]string{})); err != nil {
			t.Errorf("all unset: got %v, want nil", err)
		}
	})

	t.Run("DATABASE_URL disable hard-fails under enforcement", func(t *testing.T) {
		t.Parallel()
		err := AssertDatabaseTLSFromEnv(env(map[string]string{
			EnvDSN:          "postgres://u:p@postgres:5432/crm?sslmode=disable",
			EnvEnforceDBTLS: "1",
		}))
		if !errors.Is(err, ErrInsecureDBTLS) {
			t.Fatalf("got %v, want ErrInsecureDBTLS", err)
		}
	})

	t.Run("WA_SESSION_DATABASE_URL is also guarded", func(t *testing.T) {
		t.Parallel()
		err := AssertDatabaseTLSFromEnv(env(map[string]string{
			EnvDSN:          "postgres://u:p@db:5432/crm?sslmode=require",
			EnvWASessionDSN: "postgres://u:p@wa:5432/wa?sslmode=disable",
			EnvEnforceDBTLS: "1",
		}))
		if !errors.Is(err, ErrInsecureDBTLS) {
			t.Fatalf("got %v, want ErrInsecureDBTLS for WA session DSN", err)
		}
		if !strings.Contains(err.Error(), EnvWASessionDSN) {
			t.Errorf("err %q should name WA_SESSION_DATABASE_URL", err)
		}
	})

	t.Run("secure DSNs boot clean under enforcement", func(t *testing.T) {
		t.Parallel()
		if err := AssertDatabaseTLSFromEnv(env(map[string]string{
			EnvDSN:          "postgres://u:p@db:5432/crm?sslmode=require",
			EnvWASessionDSN: "postgres://u:p@wa:5432/wa?sslmode=verify-full",
			EnvEnforceDBTLS: "1",
		})); err != nil {
			t.Fatalf("secure DSNs: got %v, want nil", err)
		}
	})

	t.Run("insecure DSN only warns when flag unset", func(t *testing.T) {
		t.Parallel()
		if err := AssertDatabaseTLSFromEnv(env(map[string]string{
			EnvDSN: "postgres://u:p@postgres:5432/crm?sslmode=disable",
		})); err != nil {
			t.Fatalf("warn-only: got %v, want nil", err)
		}
	})
}
