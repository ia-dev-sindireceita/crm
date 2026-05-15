package metashared_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
)

// memDeduper is the in-memory reference implementation of
// metashared.Deduper used to lock the interface contract under test.
// Production wiring is the Postgres bridge in
// internal/adapter/store/postgres/metadedup — its integration test
// covers the SQL path against a real Postgres cluster (Quality bar
// rule 5: no mocked DB for code that touches storage). The fake here
// is a documented in-memory adapter that mirrors the production
// semantics (UNIQUE (channel, channel_external_id) → ErrAlreadyProcessed).
type memDeduper struct {
	mu        sync.Mutex
	claimed   map[string]bool // key -> processed_at != nil
	processed map[string]bool
}

func newMemDeduper() *memDeduper {
	return &memDeduper{
		claimed:   map[string]bool{},
		processed: map[string]bool{},
	}
}

func key(channel, externalID string) string { return channel + "|" + externalID }

func (m *memDeduper) Claim(_ context.Context, channel, channelExternalID string) error {
	if channel == "" || channelExternalID == "" {
		return errors.New("memDeduper: empty channel or external id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(channel, channelExternalID)
	if m.claimed[k] {
		return metashared.ErrAlreadyProcessed
	}
	m.claimed[k] = true
	return nil
}

func (m *memDeduper) MarkProcessed(_ context.Context, channel, channelExternalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(channel, channelExternalID)
	if !m.claimed[k] {
		return errors.New("memDeduper: MarkProcessed without prior Claim")
	}
	m.processed[k] = true
	return nil
}

// Compile-time check: memDeduper implements the port. If the interface
// shifts, the build breaks here instead of at every call site.
var _ metashared.Deduper = (*memDeduper)(nil)

func TestDeduper_ClaimFirstWins(t *testing.T) {
	t.Parallel()
	d := newMemDeduper()
	if err := d.Claim(context.Background(), "whatsapp", "wamid.A"); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	err := d.Claim(context.Background(), "whatsapp", "wamid.A")
	if !errors.Is(err, metashared.ErrAlreadyProcessed) {
		t.Fatalf("second Claim: %v, want ErrAlreadyProcessed", err)
	}
}

func TestDeduper_DifferentChannelsAreIndependent(t *testing.T) {
	t.Parallel()
	d := newMemDeduper()
	if err := d.Claim(context.Background(), "whatsapp", "shared-id"); err != nil {
		t.Fatalf("Claim whatsapp: %v", err)
	}
	if err := d.Claim(context.Background(), "instagram", "shared-id"); err != nil {
		t.Fatalf("Claim instagram (different channel): %v", err)
	}
}

func TestDeduper_MarkProcessedRequiresPriorClaim(t *testing.T) {
	t.Parallel()
	d := newMemDeduper()
	if err := d.MarkProcessed(context.Background(), "whatsapp", "wamid.X"); err == nil {
		t.Fatal("MarkProcessed without Claim should fail")
	}
	if err := d.Claim(context.Background(), "whatsapp", "wamid.X"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := d.MarkProcessed(context.Background(), "whatsapp", "wamid.X"); err != nil {
		t.Fatalf("MarkProcessed after Claim: %v", err)
	}
}

func TestDeduper_RejectsEmptyKeys(t *testing.T) {
	t.Parallel()
	d := newMemDeduper()
	if err := d.Claim(context.Background(), "", "x"); err == nil {
		t.Fatal("empty channel should fail")
	}
	if err := d.Claim(context.Background(), "whatsapp", ""); err == nil {
		t.Fatal("empty external id should fail")
	}
}

// TestDeduper_ConcurrentClaim asserts that two parallel Claim calls
// for the same key result in exactly one success — the production
// contract is enforced by the Postgres UNIQUE; the in-memory fake
// enforces it under a mutex so callers can rely on the interface
// invariant in unit tests.
func TestDeduper_ConcurrentClaim(t *testing.T) {
	t.Parallel()
	d := newMemDeduper()
	const N = 50
	var wg sync.WaitGroup
	var success, dup int
	var muCount sync.Mutex
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := d.Claim(context.Background(), "whatsapp", "wamid.concurrent")
			muCount.Lock()
			defer muCount.Unlock()
			if err == nil {
				success++
				return
			}
			if errors.Is(err, metashared.ErrAlreadyProcessed) {
				dup++
				return
			}
			t.Errorf("unexpected err: %v", err)
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("success = %d, want 1", success)
	}
	if dup != N-1 {
		t.Fatalf("dup = %d, want %d", dup, N-1)
	}
}
