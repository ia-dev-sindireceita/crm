package wallet_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

func TestFindOrphanReservations_EmptyInput(t *testing.T) {
	t.Parallel()
	if got := wallet.FindOrphanReservations(nil, fixedTime, time.Minute); got != nil {
		t.Errorf("nil input → %v, want nil", got)
	}
	if got := wallet.FindOrphanReservations([]wallet.LedgerEntry{}, fixedTime, time.Minute); got != nil {
		t.Errorf("empty slice → %v, want nil", got)
	}
}

func TestFindOrphanReservations_RecentNotReleased(t *testing.T) {
	t.Parallel()
	walletID := uuid.New()
	tenantID := uuid.New()
	rid := uuid.New()
	now := fixedTime
	open := []wallet.LedgerEntry{
		{
			WalletID:    walletID,
			TenantID:    tenantID,
			Kind:        wallet.KindReserve,
			Amount:      -10,
			ExternalRef: rid.String(),
			OccurredAt:  now.Add(-30 * time.Second),
		},
	}
	got := wallet.FindOrphanReservations(open, now, time.Minute)
	if len(got) != 0 {
		t.Errorf("recent reservation marked orphan: %+v", got)
	}
}

func TestFindOrphanReservations_OldReleased(t *testing.T) {
	t.Parallel()
	walletID := uuid.New()
	tenantID := uuid.New()
	rid := uuid.New()
	now := fixedTime
	old := now.Add(-10 * time.Minute)
	open := []wallet.LedgerEntry{
		{
			WalletID:       walletID,
			TenantID:       tenantID,
			Kind:           wallet.KindReserve,
			Amount:         -25,
			IdempotencyKey: "reserve-op-1",
			ExternalRef:    rid.String(),
			OccurredAt:     old,
		},
	}
	got := wallet.FindOrphanReservations(open, now, time.Minute)
	if len(got) != 1 {
		t.Fatalf("orphan list = %d entries, want 1", len(got))
	}
	if got[0].ReservationID != rid {
		t.Errorf("ReservationID = %s, want %s", got[0].ReservationID, rid)
	}
	if got[0].Amount != 25 {
		t.Errorf("Amount = %d, want 25 (magnitude)", got[0].Amount)
	}
	if got[0].IdempotencyKey != "reserve-op-1" {
		t.Errorf("IdempotencyKey = %q, want %q", got[0].IdempotencyKey, "reserve-op-1")
	}
}

func TestFindOrphanReservations_ExactAgeBoundary(t *testing.T) {
	t.Parallel()
	// Cut-off is now - maxAge. An entry exactly at the boundary
	// (sub == maxAge) IS released (we use < for "still fresh").
	walletID := uuid.New()
	rid := uuid.New()
	now := fixedTime
	open := []wallet.LedgerEntry{{
		WalletID:    walletID,
		Kind:        wallet.KindReserve,
		Amount:      -1,
		ExternalRef: rid.String(),
		OccurredAt:  now.Add(-time.Minute),
	}}
	got := wallet.FindOrphanReservations(open, now, time.Minute)
	if len(got) != 1 {
		t.Errorf("exact-age boundary not released: %+v", got)
	}
}

func TestFindOrphanReservations_ZeroMaxAgeReleasesEverything(t *testing.T) {
	t.Parallel()
	rid1, rid2 := uuid.New(), uuid.New()
	open := []wallet.LedgerEntry{
		{Kind: wallet.KindReserve, ExternalRef: rid1.String(), Amount: -1, OccurredAt: fixedTime.Add(time.Second)},
		{Kind: wallet.KindReserve, ExternalRef: rid2.String(), Amount: -2, OccurredAt: fixedTime},
	}
	got := wallet.FindOrphanReservations(open, fixedTime.Add(time.Hour), 0)
	if len(got) != 2 {
		t.Errorf("maxAge=0 should release all open reservations, got %d", len(got))
	}
}

func TestFindOrphanReservations_FutureOccurredAt(t *testing.T) {
	t.Parallel()
	// Clock skew or test fixtures may produce a reserve row with
	// occurred_at in the future relative to "now". Such a row is
	// never an orphan (now.Sub(future) is negative → < maxAge).
	rid := uuid.New()
	open := []wallet.LedgerEntry{{
		Kind: wallet.KindReserve, ExternalRef: rid.String(), Amount: -1,
		OccurredAt: fixedTime.Add(time.Hour),
	}}
	if got := wallet.FindOrphanReservations(open, fixedTime, time.Minute); len(got) != 0 {
		t.Errorf("future-OccurredAt entry marked orphan: %+v", got)
	}
}

func TestFindOrphanReservations_IgnoresNonReserveKinds(t *testing.T) {
	t.Parallel()
	rid := uuid.New()
	open := []wallet.LedgerEntry{
		// Adapter bug: a commit row leaked into the open list.
		{Kind: wallet.KindCommit, ExternalRef: rid.String(), Amount: -1, OccurredAt: fixedTime.Add(-time.Hour)},
		// And a grant.
		{Kind: wallet.KindGrant, ExternalRef: rid.String(), Amount: 5, OccurredAt: fixedTime.Add(-time.Hour)},
	}
	if got := wallet.FindOrphanReservations(open, fixedTime, time.Minute); got != nil {
		t.Errorf("non-reserve kinds released as orphans: %+v", got)
	}
}

func TestFindOrphanReservations_MalformedExternalRef(t *testing.T) {
	t.Parallel()
	open := []wallet.LedgerEntry{{
		Kind: wallet.KindReserve, ExternalRef: "not-a-uuid", Amount: -1,
		OccurredAt: fixedTime.Add(-time.Hour),
	}}
	if got := wallet.FindOrphanReservations(open, fixedTime, time.Minute); got != nil {
		t.Errorf("malformed ExternalRef released: %+v", got)
	}
}

func TestFindOrphanReservations_MixedOldAndNew(t *testing.T) {
	t.Parallel()
	walletID := uuid.New()
	tenantID := uuid.New()
	old1, old2, fresh := uuid.New(), uuid.New(), uuid.New()
	now := fixedTime
	open := []wallet.LedgerEntry{
		{WalletID: walletID, TenantID: tenantID, Kind: wallet.KindReserve, Amount: -1, ExternalRef: old1.String(), OccurredAt: now.Add(-10 * time.Minute)},
		{WalletID: walletID, TenantID: tenantID, Kind: wallet.KindReserve, Amount: -2, ExternalRef: fresh.String(), OccurredAt: now.Add(-10 * time.Second)},
		{WalletID: walletID, TenantID: tenantID, Kind: wallet.KindReserve, Amount: -3, ExternalRef: old2.String(), OccurredAt: now.Add(-9 * time.Minute)},
	}
	got := wallet.FindOrphanReservations(open, now, time.Minute)
	if len(got) != 2 {
		t.Fatalf("orphan list = %d, want 2", len(got))
	}
	gotIDs := map[uuid.UUID]bool{got[0].ReservationID: true, got[1].ReservationID: true}
	if !gotIDs[old1] || !gotIDs[old2] {
		t.Errorf("orphan ids = %+v, want %s + %s", gotIDs, old1, old2)
	}
	if gotIDs[fresh] {
		t.Errorf("fresh reservation marked as orphan: %s", fresh)
	}
}
