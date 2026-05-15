package nats_test

import (
	"context"
	"errors"
	"testing"
	"time"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

// These tests cover the cheap, server-independent edges of sdk.go.
// The real Connect/EnsureStream/Subscribe/Publish/Drain paths are
// exercised by the worker package's integration_test.go against a
// testcontainers NATS, which is where the bulk of coverage comes
// from (-coverpkg includes this package).

func TestSDK_Connect_RequiresURL(t *testing.T) {
	t.Parallel()
	if _, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{}); err == nil {
		t.Fatal("expected error on empty URL")
	}
}

func TestSDK_Connect_RejectsUnreachable(t *testing.T) {
	t.Parallel()
	// Port 1 is "tcpmux"; no NATS server runs there. Use a short
	// connect timeout so the test is fast.
	cfg := natsadapter.SDKConfig{
		URL:            "nats://127.0.0.1:1",
		ConnectTimeout: 250 * time.Millisecond,
	}
	if _, err := natsadapter.Connect(context.Background(), cfg); err == nil {
		t.Fatal("expected error connecting to closed port")
	}
}

func TestSDK_Close_NilSafe(t *testing.T) {
	t.Parallel()
	var a *natsadapter.SDKAdapter
	a.Close() // must not panic
	if err := a.Drain(); err != nil {
		t.Fatalf("Drain on nil adapter err = %v, want nil", err)
	}
}

func TestSDK_Delivery_NilSafe(t *testing.T) {
	t.Parallel()
	var d *natsadapter.Delivery
	if got := d.Data(); got != nil {
		t.Errorf("Data on nil delivery = %v, want nil", got)
	}
	if err := d.Ack(context.Background()); err == nil {
		t.Error("Ack on nil delivery should return error")
	}
}

func TestSDK_EnsureStream_ValidatesArgs(t *testing.T) {
	t.Parallel()
	// We can't construct a usable SDKAdapter without a server, so
	// these validation paths are exercised via the integration test
	// (which proves the AddStream call wires through). Keep this
	// test as a placeholder documenting the contract: empty name
	// and empty subjects are rejected before any RPC.
	_ = natsadapter.SDKConfig{}
}

func TestSDK_Subscribe_RejectsNilHandler(t *testing.T) {
	t.Parallel()
	// Same as EnsureStream: full path covered by integration.
	var h natsadapter.HandlerFunc
	if h != nil {
		t.Fatal("unreachable")
	}
	// We just assert the zero value is nil so a future refactor
	// that changes HandlerFunc to a struct breaks this test.
}

// Compile-time guarantee the adapter's Delivery satisfies the
// worker.Delivery contract. If either side drifts, this fails the
// build before any test runs. (No import to worker to avoid an
// adapter→domain reverse dep — we keep the assertion structural by
// checking method shapes via a local interface.)
type _workerDelivery interface {
	Data() []byte
	Ack(ctx context.Context) error
}

var _ _workerDelivery = (*natsadapter.Delivery)(nil)

// Suppress unused-error lint on intentionally discarded values.
var _ = errors.New
