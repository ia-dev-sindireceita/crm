package ratelimit_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

func TestNewPolicy_Valid(t *testing.T) {
	t.Parallel()
	p, err := ratelimit.NewPolicy("login", []ratelimit.Bucket{
		{Name: "ip", Window: time.Minute, Max: 5},
		{Name: "email", Window: time.Hour, Max: 10},
	}, ratelimit.Lockout{Threshold: 10, Duration: 15 * time.Minute})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if p.Name != "login" {
		t.Fatalf("Name = %q, want login", p.Name)
	}
	if len(p.Buckets) != 2 {
		t.Fatalf("Buckets len = %d, want 2", len(p.Buckets))
	}
	if !p.LockoutEnabled() {
		t.Fatal("LockoutEnabled = false, want true")
	}
}

func TestNewPolicy_FreezesBucketSlice(t *testing.T) {
	t.Parallel()
	in := []ratelimit.Bucket{
		{Name: "ip", Window: time.Minute, Max: 5},
	}
	p, err := ratelimit.NewPolicy("x", in, ratelimit.Lockout{})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	in[0].Max = 999
	if p.Buckets[0].Max != 5 {
		t.Fatalf("policy must not alias caller's slice; got Max=%d", p.Buckets[0].Max)
	}
}

func TestNewPolicy_Invalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   func() (string, []ratelimit.Bucket, ratelimit.Lockout)
		wantSub string
	}{
		{
			name: "empty name",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "", []ratelimit.Bucket{{Name: "ip", Window: time.Second, Max: 1}}, ratelimit.Lockout{}
			},
			wantSub: "name is empty",
		},
		{
			name: "no buckets",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", nil, ratelimit.Lockout{}
			},
			wantSub: "has no buckets",
		},
		{
			name: "bucket empty name",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", []ratelimit.Bucket{{Window: time.Second, Max: 1}}, ratelimit.Lockout{}
			},
			wantSub: "name is empty",
		},
		{
			name: "bucket zero window",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", []ratelimit.Bucket{{Name: "ip", Max: 1}}, ratelimit.Lockout{}
			},
			wantSub: "window must be > 0",
		},
		{
			name: "bucket zero max",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", []ratelimit.Bucket{{Name: "ip", Window: time.Second}}, ratelimit.Lockout{}
			},
			wantSub: "max must be > 0",
		},
		{
			name: "duplicate bucket name",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", []ratelimit.Bucket{
					{Name: "ip", Window: time.Second, Max: 1},
					{Name: "ip", Window: time.Minute, Max: 5},
				}, ratelimit.Lockout{}
			},
			wantSub: "duplicate name",
		},
		{
			name: "negative threshold",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", []ratelimit.Bucket{{Name: "ip", Window: time.Second, Max: 1}},
					ratelimit.Lockout{Threshold: -1}
			},
			wantSub: "threshold must be >= 0",
		},
		{
			name: "negative duration",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", []ratelimit.Bucket{{Name: "ip", Window: time.Second, Max: 1}},
					ratelimit.Lockout{Duration: -1}
			},
			wantSub: "duration must be >= 0",
		},
		{
			name: "threshold without duration",
			input: func() (string, []ratelimit.Bucket, ratelimit.Lockout) {
				return "x", []ratelimit.Bucket{{Name: "ip", Window: time.Second, Max: 1}},
					ratelimit.Lockout{Threshold: 3}
			},
			wantSub: "requires duration > 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, buckets, lockout := tc.input()
			_, err := ratelimit.NewPolicy(name, buckets, lockout)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ratelimit.ErrInvalidPolicy) {
				t.Fatalf("err = %v, want errors.Is ErrInvalidPolicy", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestPolicy_LockoutEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		lockout ratelimit.Lockout
		want    bool
	}{
		{"both zero", ratelimit.Lockout{}, false},
		{"only duration", ratelimit.Lockout{Duration: time.Minute}, false},
		// Threshold > 0 + Duration == 0 is rejected by NewPolicy, so we
		// build the Policy struct directly here just to exercise the
		// predicate's belt-and-braces guard.
		{"only threshold (constructed manually)", ratelimit.Lockout{Threshold: 5}, false},
		{"both set", ratelimit.Lockout{Threshold: 5, Duration: time.Minute}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ratelimit.Policy{Name: "x", Buckets: []ratelimit.Bucket{{Name: "ip", Window: time.Second, Max: 1}}, Lockout: tc.lockout}
			if got := p.LockoutEnabled(); got != tc.want {
				t.Fatalf("LockoutEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefaultPolicies(t *testing.T) {
	t.Parallel()
	policies, err := ratelimit.DefaultPolicies()
	if err != nil {
		t.Fatalf("DefaultPolicies: %v", err)
	}
	wantNames := []string{"login", "2fa_verify", "password_reset", "m_login", "m_2fa_verify"}
	if len(policies) != len(wantNames) {
		t.Fatalf("policy count = %d, want %d", len(policies), len(wantNames))
	}
	for _, name := range wantNames {
		if _, ok := policies[name]; !ok {
			t.Fatalf("missing policy %q", name)
		}
	}

	// Spot-check the spec floor values that the regression tests in PR2
	// will assert against. If any of these change, the regression tests
	// for SIN-62341 acceptance criterion #1/#2 also need to move.
	loginEmail := bucketByName(t, policies["login"], "email")
	if loginEmail.Max != 10 || loginEmail.Window != time.Hour {
		t.Fatalf("login/email bucket = %#v, want max=10 window=1h", loginEmail)
	}
	if policies["login"].Lockout != (ratelimit.Lockout{Threshold: 10, Duration: 15 * time.Minute}) {
		t.Fatalf("login lockout = %#v", policies["login"].Lockout)
	}
	if !policies["m_login"].Lockout.AlertOnLock {
		t.Fatal("m_login policy must alert on lock (ADR 0073 §D4)")
	}
	if policies["login"].Lockout.AlertOnLock {
		t.Fatal("tenant login must not page on every lockout")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func bucketByName(t *testing.T, p ratelimit.Policy, name string) ratelimit.Bucket {
	t.Helper()
	for _, b := range p.Buckets {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("policy %q has no bucket %q", p.Name, name)
	return ratelimit.Bucket{}
}
