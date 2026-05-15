package postgres_test

// SIN-62791 integration tests for the metadedup bridge in
// internal/adapter/store/postgres/metadedup. The bridge wraps the
// existing inbox Postgres store; we exercise it against the real
// inbound_message_dedup table to prove the duplicate sentinel is
// rewritten from inbox.ErrInboundAlreadyProcessed to
// metashared.ErrAlreadyProcessed without any other behaviour change.
//
// Lives in the shared postgres_test package (see inbox_adapter_test.go
// for the same rationale: a separate test binary races the ALTER ROLE
// bootstrap on the shared CI cluster).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
	"github.com/pericles-luz/crm/internal/adapter/store/postgres/metadedup"
	"github.com/pericles-luz/crm/internal/inbox"
)

func TestMetadedupBridge_Claim_DuplicateMapsToMetasharedErr(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	inner := newInboxStore(t, db)
	bridge, err := metadedup.New(inner)
	if err != nil {
		t.Fatalf("metadedup.New: %v", err)
	}
	ctx := context.Background()
	if err := bridge.Claim(ctx, "whatsapp", "wamid.bridge-1"); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	err = bridge.Claim(ctx, "whatsapp", "wamid.bridge-1")
	if !errors.Is(err, metashared.ErrAlreadyProcessed) {
		t.Fatalf("second Claim err = %v, want metashared.ErrAlreadyProcessed", err)
	}
	// Inner sentinel MUST NOT leak through the bridge — callers depend
	// on the metashared sentinel.
	if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
		t.Errorf("bridge leaked inbox.ErrInboundAlreadyProcessed instead of remapping")
	}
}

func TestMetadedupBridge_MarkProcessed_FlipsTimestamp(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	inner := newInboxStore(t, db)
	bridge, _ := metadedup.New(inner)
	ctx := context.Background()
	if err := bridge.Claim(ctx, "whatsapp", "wamid.bridge-2"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := bridge.MarkProcessed(ctx, "whatsapp", "wamid.bridge-2"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	var processed *time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT processed_at FROM inbound_message_dedup WHERE channel='whatsapp' AND channel_external_id='wamid.bridge-2'`).Scan(&processed); err != nil {
		t.Fatalf("verify processed_at: %v", err)
	}
	if processed == nil {
		t.Fatal("processed_at = nil after MarkProcessed")
	}
}

func TestMetadedupBridge_MarkProcessed_NotFound_PreservesInnerSentinel(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	inner := newInboxStore(t, db)
	bridge, _ := metadedup.New(inner)
	err := bridge.MarkProcessed(context.Background(), "whatsapp", "wamid.never-claimed")
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err = %v, want inbox.ErrNotFound", err)
	}
}

func TestMetadedupBridge_New_RejectsNil(t *testing.T) {
	t.Parallel()
	if _, err := metadedup.New(nil); err == nil {
		t.Fatal("metadedup.New(nil) err = nil, want non-nil")
	}
}
