package wallet_alerter_test

import (
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

func TestDedup_FreshKey_NotSeen(t *testing.T) {
	t.Parallel()
	d := wallet_alerter.NewDedup(time.Hour, &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)})
	if d.Seen("t1", time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)) {
		t.Error("fresh key reported as seen")
	}
}

func TestDedup_RecordThenSeen(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	d := wallet_alerter.NewDedup(time.Hour, clk)
	ts := time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)
	d.Record("t1", ts)
	if !d.Seen("t1", ts) {
		t.Error("recorded key not seen")
	}
}

func TestDedup_DifferentOccurredAt_Independent(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	d := wallet_alerter.NewDedup(time.Hour, clk)
	ts1 := time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 5, 16, 19, 1, 0, 0, time.UTC)
	d.Record("t1", ts1)
	if d.Seen("t1", ts2) {
		t.Error("second occurred_at must be independent of the first")
	}
}

func TestDedup_DifferentTenants_Independent(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	d := wallet_alerter.NewDedup(time.Hour, clk)
	ts := time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)
	d.Record("t1", ts)
	if d.Seen("t2", ts) {
		t.Error("different tenant must be independent")
	}
}

func TestDedup_ExpiryViaClock(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	d := wallet_alerter.NewDedup(time.Hour, clk)
	ts := time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)
	d.Record("t1", ts)
	clk.Advance(time.Hour + time.Second)
	if d.Seen("t1", ts) {
		t.Error("entry must expire after TTL")
	}
	if got := d.Len(); got != 0 {
		t.Errorf("Len after expiry = %d, want 0 (lazy prune must collect expired entries)", got)
	}
}

func TestDedup_LazyPruneOnRecord(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC)}
	d := wallet_alerter.NewDedup(time.Hour, clk)
	d.Record("t1", time.Date(2026, 5, 16, 19, 0, 0, 0, time.UTC))
	clk.Advance(2 * time.Hour)
	d.Record("t2", time.Date(2026, 5, 16, 21, 0, 0, 0, time.UTC))
	if got := d.Len(); got != 1 {
		t.Errorf("Len after lazy prune = %d, want 1", got)
	}
}

func TestDedup_NilSafe(t *testing.T) {
	t.Parallel()
	var d *wallet_alerter.Dedup
	if d.Seen("x", time.Now()) {
		t.Error("nil dedup must report not-seen")
	}
	d.Record("x", time.Now()) // must not panic
	if got := d.Len(); got != 0 {
		t.Errorf("nil dedup Len = %d, want 0", got)
	}
}

func TestDedup_DefaultsCoerceTTL(t *testing.T) {
	t.Parallel()
	d := wallet_alerter.NewDedup(0, nil)
	ts := time.Now().UTC()
	d.Record("t1", ts)
	// With a non-positive TTL the cache should still dedup (the
	// constructor coerces to DefaultDedupTTL = 1h).
	if !d.Seen("t1", ts) {
		t.Error("zero TTL must be coerced to a positive default and still dedup")
	}
}
