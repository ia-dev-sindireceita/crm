package wallet_alerter_test

// End-to-end coverage for the SIN-62934 producer → consumer pipeline.
// The existing TestIntegration_PublishedEvent_PostsToSlack in
// integration_test.go publishes a hand-crafted JSON body to assert the
// CONSUMER decodes correctly. This test pairs the real producer adapter
// (internal/adapter/messaging/nats.WalletDepletedPublisher) with the
// real consumer worker, proving the wire contract holds in both
// directions: a domain event published by the producer is decoded by
// the consumer and reaches Slack with the expected formatted body.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	slacknotify "github.com/pericles-luz/crm/internal/adapter/notify/slack"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

func TestIntegration_ProducerToConsumer_RoundTrip(t *testing.T) {
	url := runEmbeddedNATS(t)
	sdk := connectSDK(t, url)

	slackURL, snapshot := startSlackMock(t)
	notifier := slacknotify.New(slackURL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan error, 1)
	go func() {
		runDone <- wallet_alerter.Run(ctx, &natsAdapterShim{a: sdk}, wallet_alerter.RunConfig{
			Notifier: notifier,
			Logger:   silentLogger(),
			AckWait:  500 * time.Millisecond,
		})
	}()

	waitForStream(t, sdk, wallet_alerter.StreamName)

	producer, err := natsadapter.NewWalletDepletedPublisher(sdk)
	if err != nil {
		t.Fatalf("NewWalletDepletedPublisher: %v", err)
	}

	tid := uuid.MustParse("12345678-1234-1234-1234-123456789012")
	occurred := time.Date(2026, 5, 16, 19, 42, 0, 0, time.UTC)
	if err := producer.PublishBalanceDepleted(context.Background(), wallet.BalanceDepletedEvent{
		TenantID:         tid,
		PolicyScope:      "tenant:default",
		LastChargeTokens: 4242,
		OccurredAt:       occurred,
	}); err != nil {
		t.Fatalf("PublishBalanceDepleted: %v", err)
	}

	waitForPOSTs(t, snapshot, 1, 3*time.Second)
	got := snapshot()
	if len(got) != 1 {
		t.Fatalf("Slack POST count = %d, want 1", len(got))
	}
	const wantPrefix = ":warning: Wallet zerada em tenant `12345678-1234-1234-1234-123456789012`"
	if len(got[0].Text) < len(wantPrefix) || got[0].Text[:len(wantPrefix)] != wantPrefix {
		t.Errorf("Slack body unexpected:\n got: %s\nwantPrefix: %s", got[0].Text, wantPrefix)
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}
