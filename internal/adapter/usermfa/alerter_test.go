package usermfa

import (
	"context"
	"testing"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

func TestNoopAlerterReturnsNil(t *testing.T) {
	t.Parallel()
	a := NoopAlerter{}
	if err := a.AlertRecoveryUsed(context.Background(), mfa.RecoveryUsedDetails{}); err != nil {
		t.Fatalf("AlertRecoveryUsed: %v", err)
	}
	if err := a.AlertRecoveryRegenerated(context.Background(), mfa.RecoveryRegeneratedDetails{}); err != nil {
		t.Fatalf("AlertRecoveryRegenerated: %v", err)
	}
}
