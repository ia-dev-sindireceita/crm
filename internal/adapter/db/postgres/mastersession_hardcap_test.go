package postgres_test

// SIN-62418 storage-layer tests for the master 4h hard cap on
// Store.Touch. ADR 0073 §D3 fixes the cap at 4h from created_at; the
// implementation MUST refuse to extend past it (clamp expires_at) and
// MUST invalidate the row when a Touch lands at or past the ceiling.
//
// Reuses the shared TestMain / harness / freshDBWithMasterSession /
// seedMasterUser / frozenClock helpers from mastersession_test.go —
// same package so no plumbing duplication.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/mastersession"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

// Spec-driven boundary: at exactly created_at + 4h, Touch must treat
// the request as past the cap (>= semantics, matching iam.CheckActivity
// boundary doc-comment in internal/iam/timeouts.go).
func TestStore_TouchHardCap_AtBoundary_DeletesAndReturnsErr(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	user := seedMasterUser(t, db, "user@master.test")
	ctx := context.Background()

	created := time.Now().UTC().Truncate(time.Microsecond)
	base, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	creator := base.WithClock(frozenClock(created))
	sess, err := creator.Create(ctx, user, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	atCap := created.Add(mastersession.MasterHardTTL)
	toucher := base.WithClock(frozenClock(atCap))
	if err := toucher.Touch(ctx, sess.ID, 15*time.Minute); !errors.Is(err, mastermfa.ErrSessionHardCap) {
		t.Fatalf("Touch at boundary err = %v, want ErrSessionHardCap", err)
	}

	// Row MUST be deleted. Probe via Get (which translates pgx.ErrNoRows
	// to mastermfa.ErrSessionNotFound).
	if _, err := base.Get(ctx, sess.ID); !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Errorf("Get after hard-cap delete err = %v, want ErrSessionNotFound", err)
	}
}

// AC3: Touch at T+3h59m passes (clamped or extended), at T+4h01m
// returns hard-cap. Combined here so a single fixture exercises both
// sides of the boundary.
func TestStore_TouchHardCap_AC3_BeforeAndAfterCap(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	user := seedMasterUser(t, db, "user@master.test")
	ctx := context.Background()

	created := time.Now().UTC().Truncate(time.Microsecond)
	base, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	creator := base.WithClock(frozenClock(created))
	sess, err := creator.Create(ctx, user, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// At T+3h59m, Touch MUST succeed. With idleTTL=15m the proposed
	// expires_at = T+4h14m, which exceeds the hard cap; the adapter
	// clamps to T+4h instead of letting the row outlive the cap.
	near := created.Add(3*time.Hour + 59*time.Minute)
	if err := base.WithClock(frozenClock(near)).Touch(ctx, sess.ID, 15*time.Minute); err != nil {
		t.Fatalf("Touch at T+3h59m: %v (want nil)", err)
	}
	row, err := base.WithClock(frozenClock(near)).Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after T+3h59m Touch: %v", err)
	}
	wantCap := created.Add(mastersession.MasterHardTTL)
	if !row.ExpiresAt.Equal(wantCap) {
		t.Errorf("expires_at after near-cap Touch = %v, want clamped to %v", row.ExpiresAt, wantCap)
	}

	// At T+4h01m, Touch MUST return ErrSessionHardCap and delete the
	// row.
	past := created.Add(4*time.Hour + time.Minute)
	if err := base.WithClock(frozenClock(past)).Touch(ctx, sess.ID, 15*time.Minute); !errors.Is(err, mastermfa.ErrSessionHardCap) {
		t.Fatalf("Touch at T+4h01m err = %v, want ErrSessionHardCap", err)
	}
	if _, err := base.Get(ctx, sess.ID); !errors.Is(err, mastermfa.ErrSessionNotFound) {
		t.Errorf("Get after T+4h01m Touch err = %v, want ErrSessionNotFound", err)
	}
}

// Even when idleTTL is short enough that now+idleTTL < cap, the
// adapter must NOT extend past the cap. Test with a tiny idleTTL
// well inside the cap so we can be sure the clamp doesn't fire when
// it shouldn't.
func TestStore_TouchHardCap_BelowCap_NoClamp(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	user := seedMasterUser(t, db, "user@master.test")
	ctx := context.Background()

	created := time.Now().UTC().Truncate(time.Microsecond)
	base, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	creator := base.WithClock(frozenClock(created))
	sess, err := creator.Create(ctx, user, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// At T+1h with idleTTL=15m → expires_at = T+1h15m, well below cap.
	at := created.Add(time.Hour)
	if err := base.WithClock(frozenClock(at)).Touch(ctx, sess.ID, 15*time.Minute); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	row, err := base.WithClock(frozenClock(at)).Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := at.Add(15 * time.Minute)
	if !row.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v (no clamp expected this far below cap)", row.ExpiresAt, want)
	}
}

// MasterHardTTL is the documented 4h ADR value — pin it so a future
// reviewer who lowers it for "convenience" trips a build break.
func TestStore_MasterHardTTL_Is4h(t *testing.T) {
	t.Parallel()
	if mastersession.MasterHardTTL != 4*time.Hour {
		t.Fatalf("MasterHardTTL = %v, want 4h (ADR 0073 §D3)", mastersession.MasterHardTTL)
	}
}
