package dunning_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing/dunning"
)

// fakeRepo is a minimal in-memory implementation of DunningRepository
// used only to assert the port surface compiles and the sentinel
// errors are reachable from callers. The Postgres adapter lands in
// internal/adapter/db/postgres/dunning (separate child issue C14 /
// SIN-62965).
type fakeRepo struct {
	rows map[uuid.UUID]*dunning.DunningState
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: make(map[uuid.UUID]*dunning.DunningState)}
}

func (f *fakeRepo) GetBySubscription(_ context.Context, subID uuid.UUID) (*dunning.DunningState, error) {
	if subID == uuid.Nil {
		return nil, dunning.ErrZeroSubscription
	}
	d, ok := f.rows[subID]
	if !ok {
		return nil, dunning.ErrNotFound
	}
	return d, nil
}

func (f *fakeRepo) Save(_ context.Context, d *dunning.DunningState, _ uuid.UUID) error {
	f.rows[d.SubscriptionID()] = d
	return nil
}

// fakeOverride implements CourtesyOverride and returns the canned
// override unless tenantID is uuid.Nil.
type fakeOverride struct {
	have dunning.Override
}

func (f *fakeOverride) ActiveFor(_ context.Context, tenantID uuid.UUID, _ time.Time) (dunning.Override, error) {
	if tenantID == uuid.Nil {
		return dunning.Override{}, dunning.ErrZeroTenant
	}
	if f.have.Reason == "" {
		return dunning.Override{}, dunning.ErrNoActiveOverride
	}
	return f.have, nil
}

func TestDunningRepository_PortContract(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo()

	if _, err := repo.GetBySubscription(ctx, uuid.Nil); !errors.Is(err, dunning.ErrZeroSubscription) {
		t.Errorf("uuid.Nil: got err %v, want ErrZeroSubscription", err)
	}

	if _, err := repo.GetBySubscription(ctx, uuid.New()); !errors.Is(err, dunning.ErrNotFound) {
		t.Errorf("unknown sub: got err %v, want ErrNotFound", err)
	}

	d, err := dunning.NewDunningState(uuid.New(), uuid.New(), time.Now())
	if err != nil {
		t.Fatalf("NewDunningState: %v", err)
	}
	if err := repo.Save(ctx, d, uuid.New()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := repo.GetBySubscription(ctx, d.SubscriptionID())
	if err != nil {
		t.Fatalf("GetBySubscription after save: %v", err)
	}
	if got.ID() != d.ID() {
		t.Errorf("round-trip lost identity")
	}
}

func TestCourtesyOverride_PortContract(t *testing.T) {
	ctx := context.Background()

	t.Run("rejects zero tenant", func(t *testing.T) {
		port := &fakeOverride{}
		if _, err := port.ActiveFor(ctx, uuid.Nil, time.Now()); !errors.Is(err, dunning.ErrZeroTenant) {
			t.Errorf("got err %v, want ErrZeroTenant", err)
		}
	})

	t.Run("returns ErrNoActiveOverride when none", func(t *testing.T) {
		port := &fakeOverride{}
		_, err := port.ActiveFor(ctx, uuid.New(), time.Now())
		if !errors.Is(err, dunning.ErrNoActiveOverride) {
			t.Errorf("got err %v, want ErrNoActiveOverride", err)
		}
	})

	t.Run("returns canned override", func(t *testing.T) {
		until := time.Now().Add(24 * time.Hour)
		port := &fakeOverride{have: dunning.Override{Until: until, Reason: "valid reason for grant"}}
		got, err := port.ActiveFor(ctx, uuid.New(), time.Now())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Until.Equal(until) {
			t.Errorf("until = %v, want %v", got.Until, until)
		}
		if got.Reason != "valid reason for grant" {
			t.Errorf("reason = %q", got.Reason)
		}
	})
}
