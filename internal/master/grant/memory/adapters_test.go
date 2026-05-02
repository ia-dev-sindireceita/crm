package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/master/grant"
	"github.com/pericles-luz/crm/internal/master/grant/memory"
)

func TestRepo_SaveAndQuery(t *testing.T) {
	t.Parallel()
	r := memory.NewRepo()
	ctx := context.Background()
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	g := grant.Grant{
		ID:             "g1",
		MasterID:       "m1",
		SubscriptionID: "s1",
		Amount:         5_000_000,
		Status:         grant.StatusGranted,
		CreatedAt:      now,
	}
	if err := r.Save(ctx, g); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := r.Save(ctx, g); err == nil {
		t.Fatalf("duplicate save must error")
	}

	sum, err := r.SubscriptionWindowSum(ctx, "s1", now.Add(-90*24*time.Hour))
	if err != nil || sum != 5_000_000 {
		t.Errorf("sub sum: %d %v", sum, err)
	}
	sum, err = r.MasterWindowSum(ctx, "m1", now.Add(-365*24*time.Hour))
	if err != nil || sum != 5_000_000 {
		t.Errorf("master sum: %d %v", sum, err)
	}
	// Window exclusion: an old granted row outside the window is excluded.
	old := grant.Grant{
		ID:             "g0",
		MasterID:       "m1",
		SubscriptionID: "s1",
		Amount:         9_000_000,
		Status:         grant.StatusGranted,
		CreatedAt:      now.Add(-200 * 24 * time.Hour),
	}
	_ = r.Save(ctx, old)
	sum, _ = r.SubscriptionWindowSum(ctx, "s1", now.Add(-90*24*time.Hour))
	if sum != 5_000_000 {
		t.Errorf("90-day window must exclude old grant: got %d", sum)
	}
	sum, _ = r.MasterWindowSum(ctx, "m1", now.Add(-365*24*time.Hour))
	if sum != 14_000_000 {
		t.Errorf("365-day window must include old grant: got %d", sum)
	}

	// Pending and cancelled grants are excluded from cap math.
	pending := grant.Grant{
		ID:             "g2",
		MasterID:       "m1",
		SubscriptionID: "s1",
		Amount:         99,
		Status:         grant.StatusPendingApproval,
		CreatedAt:      now,
	}
	_ = r.Save(ctx, pending)
	sum, _ = r.SubscriptionWindowSum(ctx, "s1", now.Add(-90*24*time.Hour))
	if sum != 5_000_000 {
		t.Errorf("pending must not count: got %d", sum)
	}
}

func TestRepo_FindAndUpdateDecision(t *testing.T) {
	t.Parallel()
	r := memory.NewRepo()
	ctx := context.Background()
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	pending := grant.Grant{
		ID:             "p1",
		MasterID:       "m1",
		SubscriptionID: "s1",
		Amount:         11_000_000,
		Status:         grant.StatusPendingApproval,
		CreatedAt:      now,
	}
	_ = r.Save(ctx, pending)

	got, ok, err := r.FindByID(ctx, "p1")
	if err != nil || !ok || got.ID != "p1" {
		t.Fatalf("find: %+v %v %v", got, ok, err)
	}
	_, ok, _ = r.FindByID(ctx, "missing")
	if ok {
		t.Errorf("missing should not be found")
	}

	if err := r.UpdateDecision(ctx, "p1", grant.StatusApproved, "m2", now.Add(time.Hour)); err != nil {
		t.Fatalf("update decision: %v", err)
	}
	got, _, _ = r.FindByID(ctx, "p1")
	if got.Status != grant.StatusApproved || got.DecidedBy != "m2" {
		t.Errorf("after approve: %+v", got)
	}
	// Approved counts toward window sums (using DecidedAt as the effective time).
	sum, _ := r.MasterWindowSum(ctx, "m1", now.Add(-365*24*time.Hour))
	if sum != 11_000_000 {
		t.Errorf("approved should count: got %d", sum)
	}

	// Re-applying decision on a non-pending row must error.
	if err := r.UpdateDecision(ctx, "p1", grant.StatusApproved, "m3", now); err == nil {
		t.Errorf("update on non-pending should error")
	}
	if err := r.UpdateDecision(ctx, "missing", grant.StatusApproved, "m2", now); err == nil {
		t.Errorf("update on missing should error")
	}

	// Bogus target status must error.
	pending2 := pending
	pending2.ID = "p2"
	_ = r.Save(ctx, pending2)
	if err := r.UpdateDecision(ctx, "p2", grant.StatusGranted, "m2", now); err == nil {
		t.Errorf("invalid target status should error")
	}
}

func TestAuditLogger_AndAlerts(t *testing.T) {
	t.Parallel()
	a := memory.NewAuditLogger()
	_ = a.Log(context.Background(), grant.AuditEntry{Kind: grant.AuditGranted})
	_ = a.Log(context.Background(), grant.AuditEntry{Kind: grant.AuditAlertEmitted})
	if got := a.CountKind(grant.AuditGranted); got != 1 {
		t.Errorf("count: %d", got)
	}
	if got := len(a.Entries()); got != 2 {
		t.Errorf("entries: %d", got)
	}

	n := memory.NewAlertNotifier()
	if err := n.Notify(context.Background(), grant.Alert{}); err != nil {
		t.Errorf("notify: %v", err)
	}
	if got := len(n.Alerts()); got != 1 {
		t.Errorf("alerts: %d", got)
	}
	n.SetFailure(errors.New("boom"))
	if err := n.Notify(context.Background(), grant.Alert{}); err == nil {
		t.Errorf("expected failure")
	}
	n.SetFailure(nil)
	if err := n.Notify(context.Background(), grant.Alert{}); err != nil {
		t.Errorf("after clearing failure: %v", err)
	}
}

func TestFixedClockAndIDs(t *testing.T) {
	t.Parallel()
	c := memory.NewFixedClock(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC))
	got := c.Now()
	c.Advance(2 * time.Hour)
	if !c.Now().Equal(got.Add(2 * time.Hour)) {
		t.Errorf("advance broken")
	}
	c.Set(got)
	if !c.Now().Equal(got) {
		t.Errorf("set broken")
	}

	ids := memory.NewSequenceIDs("x")
	a, b := ids.NewID(), ids.NewID()
	if a == b {
		t.Errorf("ids not unique: %s %s", a, b)
	}
	if a != "x1" || b != "x2" {
		t.Errorf("ids: %s %s", a, b)
	}
}
