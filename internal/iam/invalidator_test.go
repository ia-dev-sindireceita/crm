package iam

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestMemoryInvalidator_PreservesCurrent simulates the ADR 0073 D3
// "login of the same user in two browsers" scenario: Browser A is in
// session sA, Browser B logs in (session sB), then InvalidateAllExceptCurrent
// runs with currentSessionID = sB. sA must be removed; sB must survive.
// This is exactly the assertion acceptance criteria #3 demands at the
// Redis layer; here we run it against the in-memory port to keep the
// domain test free of Redis.
func TestMemoryInvalidator_PreservesCurrent(t *testing.T) {
	user := uuid.New()
	sA := uuid.New()
	sB := uuid.New()
	other := uuid.New() // a different user — must not be touched

	inv := NewMemoryInvalidator()
	inv.Add(user, sA)
	inv.Add(user, sB)
	inv.Add(other, sA) // same session id, different user — must survive

	deleted, err := inv.InvalidateAllExceptCurrent(context.Background(), user, sB)
	if err != nil {
		t.Fatalf("InvalidateAllExceptCurrent: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if inv.Has(user, sA) {
		t.Fatalf("session sA on the invalidated user should have been removed")
	}
	if !inv.Has(user, sB) {
		t.Fatalf("session sB is the current session — it must be preserved")
	}
	if !inv.Has(other, sA) {
		t.Fatalf("the other user's sessions must be untouched")
	}
}

func TestMemoryInvalidator_UnknownUserIsNoOp(t *testing.T) {
	inv := NewMemoryInvalidator()
	deleted, err := inv.InvalidateAllExceptCurrent(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
}

func TestMemoryInvalidator_TwoFactorRotationFlow(t *testing.T) {
	// ADR 0073 D3: 2FA verify success rotates the session id. The new
	// id is created (Add), then the old pre-MFA id is invalidated. After
	// the flow only the post-MFA id remains.
	user := uuid.New()
	preMFA := uuid.New()
	postMFA := uuid.New()

	inv := NewMemoryInvalidator()
	inv.Add(user, preMFA)
	inv.Add(user, postMFA)

	deleted, err := inv.InvalidateAllExceptCurrent(context.Background(), user, postMFA)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if inv.Has(user, preMFA) {
		t.Fatalf("pre-MFA session must be invalidated")
	}
	if !inv.Has(user, postMFA) {
		t.Fatalf("post-MFA session must survive")
	}
}

// TestSessionInvalidator_PortShape pins the port type so an accidental
// signature drift breaks the test, not just downstream wiring.
func TestSessionInvalidator_PortShape(t *testing.T) {
	var _ SessionInvalidator = (*MemoryInvalidator)(nil)
}
