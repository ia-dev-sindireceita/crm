package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

func TestMasterGrant_NewMasterGrant(t *testing.T) {
	tenant := uuid.New()
	actor := uuid.New()
	now := time.Now().UTC()

	t.Run("valid extra_tokens", func(t *testing.T) {
		g, err := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens, nil, "valid reason here", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if g.TenantID() != tenant {
			t.Errorf("TenantID = %v, want %v", g.TenantID(), tenant)
		}
		if g.Kind() != wallet.KindExtraTokens {
			t.Errorf("Kind = %v, want extra_tokens", g.Kind())
		}
		if len(g.ExternalID()) != 26 {
			t.Errorf("ExternalID len = %d, want 26 (ULID)", len(g.ExternalID()))
		}
		if g.ID() == uuid.Nil {
			t.Error("ID must not be zero")
		}
	})

	t.Run("zero tenant", func(t *testing.T) {
		_, err := wallet.NewMasterGrant(uuid.Nil, actor, wallet.KindExtraTokens, nil, "valid reason xx", now)
		if !isErr(err, wallet.ErrZeroTenant) {
			t.Errorf("want ErrZeroTenant, got %v", err)
		}
	})

	t.Run("zero actor", func(t *testing.T) {
		_, err := wallet.NewMasterGrant(tenant, uuid.Nil, wallet.KindExtraTokens, nil, "valid reason xx", now)
		if !isErr(err, wallet.ErrZeroActor) {
			t.Errorf("want ErrZeroActor, got %v", err)
		}
	})

	t.Run("invalid kind", func(t *testing.T) {
		_, err := wallet.NewMasterGrant(tenant, actor, wallet.MasterGrantKind("bogus"), nil, "valid reason xx", now)
		if !isErr(err, wallet.ErrInvalidGrantKind) {
			t.Errorf("want ErrInvalidGrantKind, got %v", err)
		}
	})

	t.Run("short reason", func(t *testing.T) {
		_, err := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens, nil, "short", now)
		if !isErr(err, wallet.ErrGrantReasonTooShort) {
			t.Errorf("want ErrGrantReasonTooShort, got %v", err)
		}
	})
}

func TestMasterGrant_Revoke(t *testing.T) {
	tenant := uuid.New()
	actor := uuid.New()
	revoker := uuid.New()
	now := time.Now().UTC()

	newGrant := func() *wallet.MasterGrant {
		g, err := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens, nil, "test grant reason", now)
		if err != nil {
			t.Fatalf("NewMasterGrant: %v", err)
		}
		return g
	}

	t.Run("happy path", func(t *testing.T) {
		g := newGrant()
		if err := g.Revoke(revoker, "revoke reason here", now); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if !g.IsRevoked() {
			t.Error("IsRevoked = false, want true")
		}
	})

	t.Run("revoke already revoked", func(t *testing.T) {
		g := newGrant()
		_ = g.Revoke(revoker, "revoke reason here", now)
		err := g.Revoke(revoker, "revoke again reason", now)
		if !isErr(err, wallet.ErrGrantAlreadyRevoked) {
			t.Errorf("want ErrGrantAlreadyRevoked, got %v", err)
		}
	})

	t.Run("revoke after consume", func(t *testing.T) {
		g := newGrant()
		_ = g.Consume("ledger-entry-id", now)
		err := g.Revoke(revoker, "revoke after consume", now)
		if !isErr(err, wallet.ErrGrantAlreadyConsumed) {
			t.Errorf("want ErrGrantAlreadyConsumed, got %v", err)
		}
	})

	t.Run("zero revoker", func(t *testing.T) {
		g := newGrant()
		err := g.Revoke(uuid.Nil, "revoke reason here", now)
		if !isErr(err, wallet.ErrZeroActor) {
			t.Errorf("want ErrZeroActor, got %v", err)
		}
	})

	t.Run("short revoke reason", func(t *testing.T) {
		g := newGrant()
		err := g.Revoke(revoker, "short", now)
		if !isErr(err, wallet.ErrGrantReasonTooShort) {
			t.Errorf("want ErrGrantReasonTooShort, got %v", err)
		}
	})
}

func TestMasterGrant_Consume(t *testing.T) {
	tenant := uuid.New()
	actor := uuid.New()
	revoker := uuid.New()
	now := time.Now().UTC()

	newGrant := func() *wallet.MasterGrant {
		g, err := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens, nil, "test grant reason", now)
		if err != nil {
			t.Fatalf("NewMasterGrant: %v", err)
		}
		return g
	}

	t.Run("happy path", func(t *testing.T) {
		g := newGrant()
		if err := g.Consume("ledger-entry-123", now); err != nil {
			t.Fatalf("Consume: %v", err)
		}
		if !g.IsConsumed() {
			t.Error("IsConsumed = false, want true")
		}
		if g.ConsumedRef() != "ledger-entry-123" {
			t.Errorf("ConsumedRef = %q, want ledger-entry-123", g.ConsumedRef())
		}
	})

	t.Run("consume already consumed", func(t *testing.T) {
		g := newGrant()
		_ = g.Consume("ref1", now)
		err := g.Consume("ref2", now)
		if !isErr(err, wallet.ErrGrantAlreadyConsumed) {
			t.Errorf("want ErrGrantAlreadyConsumed, got %v", err)
		}
	})

	t.Run("consume after revoke", func(t *testing.T) {
		g := newGrant()
		_ = g.Revoke(revoker, "revoke reason here", now)
		err := g.Consume("ref", now)
		if !isErr(err, wallet.ErrGrantAlreadyRevoked) {
			t.Errorf("want ErrGrantAlreadyRevoked, got %v", err)
		}
	})
}

func TestNewULID(t *testing.T) {
	ids := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := wallet.NewULID()
		if len(id) != 26 {
			t.Fatalf("ULID length = %d, want 26", len(id))
		}
		for _, ch := range id {
			if !isValidCrockford(ch) {
				t.Fatalf("ULID %q contains invalid char %q", id, ch)
			}
		}
		ids[id] = struct{}{}
	}
	if len(ids) != 1000 {
		t.Errorf("generated %d unique ULIDs out of 1000 — collision detected", len(ids))
	}
}

// isValidCrockford reports whether r is in the Crockford base-32 alphabet.
func isValidCrockford(r rune) bool {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, c := range alpha {
		if r == c {
			return true
		}
	}
	return false
}

func isErr(got, target error) bool {
	return errors.Is(got, target)
}
