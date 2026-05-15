package password

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// fakePwned implements PwnedPasswordChecker. Mode 'hit' returns true,
// 'miss' returns false, 'down' returns ErrPwnedCheckUnavailable. The
// triggers map lets tests opt into per-plaintext behaviour.
type fakePwned struct {
	defaultMode string
	triggers    map[string]string // plain -> mode override
	calls       int
}

func (f *fakePwned) IsPwned(_ context.Context, plain string) (bool, error) {
	f.calls++
	mode := f.defaultMode
	if m, ok := f.triggers[plain]; ok {
		mode = m
	}
	switch mode {
	case "hit":
		return true, nil
	case "miss":
		return false, nil
	case "down":
		return false, ErrPwnedCheckUnavailable
	case "boom":
		return false, errors.New("network: unexpected EOF")
	}
	return false, nil
}

// TestPolicyCheck_AcceptanceMatrix is acceptance criterion #2 in one
// table — it covers the five required cases (12 chars OK, 11 chars
// rejected, "Password123!" rejected by HIBP-mock, password=email
// rejected, > 128 chars rejected) plus a few neighbours that keep the
// behaviour boundary-stable.
func TestPolicyCheck_AcceptanceMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		plain   string
		ctx     PolicyContext
		wantOK  bool
		wantWhy PolicyReason
	}{
		{
			name:   "12 chars passes",
			plain:  "abcdefghijkl",
			ctx:    PolicyContext{Email: "u@x.test"},
			wantOK: true,
		},
		{
			name:    "11 chars rejected (too short)",
			plain:   "abcdefghijk",
			ctx:     PolicyContext{Email: "u@x.test"},
			wantOK:  false,
			wantWhy: ReasonTooShort,
		},
		{
			name:    "Password123! rejected by HIBP mock",
			plain:   "Password123!",
			ctx:     PolicyContext{Email: "u@x.test"},
			wantOK:  false,
			wantWhy: ReasonBreached,
		},
		{
			name:    "password equals email rejected",
			plain:   "alice@acme.test",
			ctx:     PolicyContext{Email: "alice@acme.test"},
			wantOK:  false,
			wantWhy: ReasonMatchesIdentity,
		},
		{
			name:    "password equals email case-insensitive rejected",
			plain:   "Alice@Acme.Test",
			ctx:     PolicyContext{Email: "alice@acme.test"},
			wantOK:  false,
			wantWhy: ReasonMatchesIdentity,
		},
		{
			name:    "password equals username rejected",
			plain:   "alice-the-admin",
			ctx:     PolicyContext{Username: "alice-the-admin"},
			wantOK:  false,
			wantWhy: ReasonMatchesIdentity,
		},
		{
			name:    "password equals tenant name rejected",
			plain:   "acme-tenant-co",
			ctx:     PolicyContext{TenantName: "acme-tenant-co"},
			wantOK:  false,
			wantWhy: ReasonMatchesIdentity,
		},
		{
			name:    "129 chars rejected (too long)",
			plain:   strings.Repeat("a", 129),
			ctx:     PolicyContext{Email: "u@x.test"},
			wantOK:  false,
			wantWhy: ReasonTooLong,
		},
		{
			name:   "128 chars exact passes",
			plain:  strings.Repeat("a", 128),
			ctx:    PolicyContext{Email: "u@x.test"},
			wantOK: true,
		},
		{
			name:   "12 utf-8 runes passes (multi-byte)",
			plain:  strings.Repeat("é", 12),
			ctx:    PolicyContext{Email: "u@x.test"},
			wantOK: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Per-subtest fixture: shared *fakePwned pointers leak the
			// internal `calls` counter into a parallel data race even
			// though no subtest reads it. Keep each subtest's state
			// independent so the race detector stays honest.
			pol := &Policy{Pwned: &fakePwned{
				defaultMode: "miss",
				triggers: map[string]string{
					"Password123!": "hit",
				},
			}}
			err := pol.PolicyCheck(context.Background(), tc.plain, tc.ctx)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("expected pass, got %v", err)
				}
				return
			}
			var pe *PolicyError
			if !errors.As(err, &pe) {
				t.Fatalf("expected *PolicyError, got %T (%v)", err, err)
			}
			if pe.Reason != tc.wantWhy {
				t.Fatalf("Reason=%q want %q", pe.Reason, tc.wantWhy)
			}
		})
	}
}

// TestPolicyCheck_HIBPDown_LocalListRejects — when HIBP is degraded but
// the local top-100k list flags the password, the policy must reject
// (defense-in-depth: HIBP outage doesn't open a hole for known-weak
// passwords).
func TestPolicyCheck_HIBPDown_LocalListRejects(t *testing.T) {
	t.Parallel()
	pol := &Policy{
		Pwned:     &fakePwned{defaultMode: "down"},
		LocalList: &fakePwned{defaultMode: "hit"},
		Logger:    silentLogger(),
	}
	err := pol.PolicyCheck(context.Background(), "very-strong-12c", PolicyContext{Email: "u@x.test"})
	var pe *PolicyError
	if !errors.As(err, &pe) || pe.Reason != ReasonBreached {
		t.Fatalf("err=%v want ReasonBreached", err)
	}
}

// TestPolicyCheck_HIBPDown_LocalListPasses — when HIBP is degraded and
// the local list passes, the policy passes WITH a WARN log entry under
// event=iam_password_hibp_unavailable so ops can graph the rate.
func TestPolicyCheck_HIBPDown_LocalListPasses(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pol := &Policy{
		Pwned:     &fakePwned{defaultMode: "down"},
		LocalList: &fakePwned{defaultMode: "miss"},
		Logger:    logger,
	}
	err := pol.PolicyCheck(context.Background(), "valid-strong-12", PolicyContext{Email: "u@x.test"})
	if err != nil {
		t.Fatalf("policy should pass with degraded HIBP + local-list miss, got %v", err)
	}
	if !strings.Contains(buf.String(), "iam_password_hibp_unavailable") {
		t.Fatalf("expected WARN event=iam_password_hibp_unavailable in log; got: %s", buf.String())
	}
}

// TestPolicyCheck_HIBPDown_NoLocalList_FailsClosed — when HIBP is
// degraded and there is no local fallback configured, the policy MUST
// fail closed rather than silently let an unscreened password through.
func TestPolicyCheck_HIBPDown_NoLocalList_FailsClosed(t *testing.T) {
	t.Parallel()
	pol := &Policy{
		Pwned:  &fakePwned{defaultMode: "down"},
		Logger: silentLogger(),
	}
	err := pol.PolicyCheck(context.Background(), "valid-strong-12", PolicyContext{Email: "u@x.test"})
	var pe *PolicyError
	if !errors.As(err, &pe) || pe.Reason != ReasonBreached {
		t.Fatalf("err=%v want ReasonBreached when HIBP down + no local list", err)
	}
}

// TestPolicyCheck_HIBPUnexpectedError treats non-sentinel HIBP errors the
// same as "unavailable" — fall through to LocalList — and emits an extra
// log line so ops can spot transient bugs.
func TestPolicyCheck_HIBPUnexpectedError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pol := &Policy{
		Pwned:     &fakePwned{defaultMode: "boom"},
		LocalList: &fakePwned{defaultMode: "miss"},
		Logger:    logger,
	}
	err := pol.PolicyCheck(context.Background(), "valid-strong-12", PolicyContext{Email: "u@x.test"})
	if err != nil {
		t.Fatalf("policy should pass when HIBP errors but local-list misses, got %v", err)
	}
	if !strings.Contains(buf.String(), "iam_password_hibp_error") {
		t.Fatalf("expected event=iam_password_hibp_error in log; got: %s", buf.String())
	}
}

// TestPolicyCheck_NoPwnedConfigured — when no remote checker is wired,
// length + identity rules still apply and a clean password passes.
func TestPolicyCheck_NoPwnedConfigured(t *testing.T) {
	t.Parallel()
	pol := &Policy{}
	if err := pol.PolicyCheck(context.Background(), "plenty-strong-12", PolicyContext{Email: "u@x.test"}); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
	// Identity rule still fires.
	err := pol.PolicyCheck(context.Background(), "u@x.test-12345", PolicyContext{Email: "U@X.test-12345"})
	var pe *PolicyError
	if !errors.As(err, &pe) || pe.Reason != ReasonMatchesIdentity {
		t.Fatalf("err=%v want ReasonMatchesIdentity", err)
	}
}

// TestPolicyError_Format — Error() produces a stable, log-friendly format
// so the *PolicyError is useful to ops without leaking the plaintext.
func TestPolicyError_Format(t *testing.T) {
	t.Parallel()
	pe := &PolicyError{Reason: ReasonTooShort, Detail: "min 12 chars"}
	if !strings.Contains(pe.Error(), "too_short") || !strings.Contains(pe.Error(), "min 12 chars") {
		t.Fatalf("Error() format unexpected: %q", pe.Error())
	}
	var nilPE *PolicyError
	if got := nilPE.Error(); got != "" {
		t.Fatalf("(*PolicyError)(nil).Error() = %q want empty", got)
	}
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
