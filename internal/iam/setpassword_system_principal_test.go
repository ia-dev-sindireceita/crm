package iam

// SIN-66305 gate 2 — SetPassword must refuse the reserved system principal
// at the domain boundary, before hashing or touching the store, so it can
// never be made loginnable (is_master amplification). White-box test reusing
// the fakePolicy / fakePasswordWriter helpers from setpassword_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/password"
)

func TestSetPassword_RefusesSystemPrincipal(t *testing.T) {
	t.Parallel()
	writer := newFakeWriter()
	svc := &Service{
		PasswordPolicy: &fakePolicy{}, // would accept any password
		PasswordHasher: password.Default(),
		PasswordWriter: writer,
	}

	err := svc.SetPassword(context.Background(), uuid.New(), SystemPrincipalID(),
		"a-perfectly-valid-password", password.PolicyContext{})
	if !errors.Is(err, ErrSystemPrincipalProtected) {
		t.Fatalf("SetPassword(system principal) err = %v, want ErrSystemPrincipalProtected", err)
	}
	if writer.calls != 0 {
		t.Errorf("PasswordWriter called %d times; the guard must short-circuit before any store write", writer.calls)
	}

	// Control: a normal user id is NOT refused by the guard (it proceeds to
	// hash + write). Proves the guard is keyed on the reserved UUID, not a
	// blanket refusal.
	if err := svc.SetPassword(context.Background(), uuid.New(), uuid.New(),
		"a-perfectly-valid-password", password.PolicyContext{}); err != nil {
		t.Fatalf("SetPassword(normal user) err = %v, want nil", err)
	}
	if writer.calls != 1 {
		t.Errorf("PasswordWriter calls = %d, want 1 for the normal-user control", writer.calls)
	}
}
