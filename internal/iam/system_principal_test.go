package iam

// SIN-66305 (R3 / SIN-66292) — unit coverage for the reserved system
// principal helpers and the gate-1 "fails closed" property of its password
// sentinel. The DB-backed exclusion (gate 2) and the seeded row shape (gate
// 3/4) are proven against a live cluster in the postgres adapter tests.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSystemPrincipalID_StableAndReserved(t *testing.T) {
	t.Parallel()
	got := SystemPrincipalID()
	// Must equal the fixed UUID seeded by migration 0126.
	want := uuid.MustParse("00000000-0000-0000-0000-000000005a5e")
	if got != want {
		t.Fatalf("SystemPrincipalID() = %s, want %s (must match migration 0126 seed)", got, want)
	}
	if SystemPrincipalID() != got {
		t.Fatal("SystemPrincipalID() must be stable across calls")
	}
	if got == uuid.Nil {
		t.Fatal("SystemPrincipalID() must not be the nil UUID — the SplitLogger rejects a nil actor")
	}
}

func TestIsSystemPrincipal(t *testing.T) {
	t.Parallel()
	if !IsSystemPrincipal(SystemPrincipalID()) {
		t.Error("IsSystemPrincipal(SystemPrincipalID()) = false, want true")
	}
	if IsSystemPrincipal(uuid.Nil) {
		t.Error("IsSystemPrincipal(uuid.Nil) = true, want false")
	}
	if IsSystemPrincipal(uuid.New()) {
		t.Error("IsSystemPrincipal(<random>) = true, want false")
	}
}

// Gate 1 at the crypto layer: the stored sentinel is not a decodable PHC
// string, so VerifyPassword can NEVER return ok=true for it regardless of the
// candidate password. This is what makes MasterLogin fail closed even if the
// reader exclusion (gate 2) were ever bypassed.
func TestPasswordSentinelHash_FailsClosed(t *testing.T) {
	t.Parallel()
	for _, candidate := range []string{"", "SYSTEM-NO-LOGIN", "hunter2", "any-guess", PasswordSentinelHash} {
		ok, err := VerifyPassword(candidate, PasswordSentinelHash)
		if ok {
			t.Errorf("VerifyPassword(%q, sentinel) = ok=true; the sentinel must never authenticate", candidate)
		}
		if err == nil {
			t.Errorf("VerifyPassword(%q, sentinel) err = nil; expected a decode error on the non-PHC sentinel", candidate)
		}
	}
}

func TestSystemPrincipalEmail_NonDeliverable(t *testing.T) {
	t.Parallel()
	// RFC 2606 reserves .invalid so the address can never receive mail —
	// password-reset / re-invite flows have nothing to deliver to it.
	if !strings.HasSuffix(SystemPrincipalEmail, ".invalid") {
		t.Errorf("SystemPrincipalEmail = %q, want a reserved .invalid address", SystemPrincipalEmail)
	}
}
