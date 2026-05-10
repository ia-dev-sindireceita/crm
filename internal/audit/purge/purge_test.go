package purge_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/audit/purge"
)

// fakeStore is an in-memory Store that records the time it was called
// with and returns a canned Result/error. It is NOT a database mock:
// the real Store contract (the postgres-bound DELETE) is exercised in
// the integration tests under internal/adapter/db/postgres/audit_purge_test.go.
type fakeStore struct {
	got    time.Time
	calls  int
	result purge.Result
	err    error
}

func (s *fakeStore) PurgeExpired(_ context.Context, now time.Time) (purge.Result, error) {
	s.got = now
	s.calls++
	return s.result, s.err
}

func TestNew_RejectsNilStore(t *testing.T) {
	t.Parallel()
	if _, err := purge.New(nil, nil); !errors.Is(err, purge.ErrNilStore) {
		t.Fatalf("err=%v, want ErrNilStore", err)
	}
}

func TestNew_DefaultsClockToWallClock(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	sw, err := purge.New(store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := time.Now().UTC().Add(-time.Second)
	if _, err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("calls=%d, want 1", store.calls)
	}
	if !store.got.After(before) {
		t.Fatalf("clock=%v, want after %v", store.got, before)
	}
}

func TestSweep_PassesPinnedClockThroughToStore(t *testing.T) {
	t.Parallel()
	pinned := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{result: purge.Result{DeletedRows: 7, TenantsSwept: 3}}
	sw, err := purge.New(store, func() time.Time { return pinned })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := sw.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !store.got.Equal(pinned) {
		t.Fatalf("clock=%v, want %v", store.got, pinned)
	}
	if got.DeletedRows != 7 || got.TenantsSwept != 3 {
		t.Fatalf("result=%+v, want {DeletedRows:7 TenantsSwept:3}", got)
	}
}

func TestSweep_PropagatesStoreError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	sw, err := purge.New(&fakeStore{err: sentinel}, func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sw.Sweep(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err=%v, want sentinel", err)
	}
}
