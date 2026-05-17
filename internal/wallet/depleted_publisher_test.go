package wallet_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

func TestNoOpBalanceDepletedPublisher_PublishBalanceDepleted_ReturnsNil(t *testing.T) {
	t.Parallel()
	p := wallet.NoOpBalanceDepletedPublisher{}
	evt := wallet.BalanceDepletedEvent{
		TenantID:         uuid.New(),
		PolicyScope:      "tenant:default",
		LastChargeTokens: 42,
		OccurredAt:       time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := p.PublishBalanceDepleted(context.Background(), evt); err != nil {
		t.Fatalf("NoOp publisher returned error: %v", err)
	}
}

func TestNoOpBalanceDepletedPublisher_SatisfiesPort(t *testing.T) {
	t.Parallel()
	var _ wallet.BalanceDepletedPublisher = wallet.NoOpBalanceDepletedPublisher{}
}
