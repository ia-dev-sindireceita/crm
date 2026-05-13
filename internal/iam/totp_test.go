package iam

import (
	"strings"
	"testing"
)

func TestNoopTOTP_Verify(t *testing.T) {
	v := NoopTOTP{}
	if v.Verify("secret", "") {
		t.Fatalf("empty code should fail closed even in NoopTOTP")
	}
	if !v.Verify("secret", "anything-non-empty") {
		t.Fatalf("non-empty code should be accepted by stub")
	}
}

func TestEnvIsProduction(t *testing.T) {
	if EnvIsProduction(nil) {
		t.Fatalf("nil getenv must not be considered production")
	}
	cases := map[string]bool{
		"":           false,
		"production": true,
		"prod":       false, // strict match — see EnvIsProduction doc
		"PRODUCTION": false,
		"staging":    false,
		"dev":        false,
	}
	for v, want := range cases {
		t.Run(v, func(t *testing.T) {
			env := v
			got := EnvIsProduction(func(string) string { return env })
			if got != want {
				t.Fatalf("ENV=%q: got %v want %v", v, got, want)
			}
		})
	}
}

func TestAssertProductionSafe_NoExitWhenNotProd(t *testing.T) {
	called := 0
	AssertProductionSafe(NoopTOTP{}, func(string) string { return "staging" }, func(int) { called++ })
	if called != 0 {
		t.Fatalf("exit invoked outside production: %d", called)
	}
}

func TestAssertProductionSafe_NoExitWhenRealVerifier(t *testing.T) {
	called := 0
	AssertProductionSafe(realTOTPFake{}, func(string) string { return "production" }, func(int) { called++ })
	if called != 0 {
		t.Fatalf("exit invoked for non-Noop verifier: %d", called)
	}
}

func TestAssertProductionSafe_ExitOnNoopInProd(t *testing.T) {
	called := 0
	gotCode := -1
	AssertProductionSafe(NoopTOTP{}, func(string) string { return "production" }, func(code int) {
		called++
		gotCode = code
	})
	if called != 1 {
		t.Fatalf("expected exit invoked once, got %d", called)
	}
	if gotCode != 1 {
		t.Fatalf("expected exit(1), got exit(%d)", gotCode)
	}
}

func TestAssertProductionSafe_NilExitFallback(t *testing.T) {
	// When exit is nil and we're NOT in production, the function must
	// still return without error (no panic, no exit). We only assert the
	// safe path here because passing nil exit + production + Noop would
	// kill the test binary.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	AssertProductionSafe(NoopTOTP{}, func(string) string { return "" }, nil)
}

// realTOTPFake is a non-Noop TOTPVerifier used to prove
// AssertProductionSafe only trips on the exact NoopTOTP type.
type realTOTPFake struct{}

func (realTOTPFake) Verify(_, code string) bool {
	return strings.HasPrefix(code, "real:")
}
