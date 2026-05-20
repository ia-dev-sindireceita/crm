//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// TestCustomDomainStore_MarkVerified_RejectsRotatedToken — SIN-63104.
//
// Drives the postgres adapter's compare-and-swap path against a real
// Postgres so the SQL predicate `verification_token = $2` is exercised
// directly (per the engineering bar: no DB mocking when the code
// actually touches storage). The test:
//
//  1. inserts a tenant_custom_domains row with verification_token = "T1"
//  2. calls MarkVerified with expectedToken = "T2"
//  3. asserts ErrTokenRotated
//  4. asserts verified_at stays NULL (the CAS UPDATE matched 0 rows)
//
// Pre-remediation code (no AND verification_token = $2 in the SQL)
// would have flipped verified_at and returned a populated Domain.
func TestCustomDomainStore_MarkVerified_RejectsRotatedToken(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id := uuid.New()
	tenant := uuid.New()
	const correctToken = "T1"
	const insertSQL = `
INSERT INTO tenant_custom_domains
  (id, tenant_id, host, verification_token, token_issued_at, created_at, updated_at)
VALUES
  ($1, $2, $3, $4, $5, $5, $5)`
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if _, err := h.pool.Exec(ctx, insertSQL, id, tenant, "shop.example.com", correctToken, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	store := pgstore.NewCustomDomainStore(h.pool)

	_, err := store.MarkVerified(ctx, id, "T2-rotated", now.Add(5*time.Second), false, nil)
	if !errors.Is(err, management.ErrTokenRotated) {
		t.Fatalf("err = %v, want ErrTokenRotated", err)
	}

	// Row must remain unverified — the CAS predicate must have prevented
	// the UPDATE from matching any row.
	var verifiedAt *time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT verified_at FROM tenant_custom_domains WHERE id = $1`, id,
	).Scan(&verifiedAt); err != nil {
		t.Fatalf("post-check query: %v", err)
	}
	if verifiedAt != nil {
		t.Fatalf("verified_at = %v, want nil (CAS must reject the write)", verifiedAt)
	}

	// Sanity: the correct token still works.
	got, err := store.MarkVerified(ctx, id, correctToken, now.Add(10*time.Second), true, nil)
	if err != nil {
		t.Fatalf("MarkVerified (correct token): %v", err)
	}
	if got.VerifiedAt == nil {
		t.Fatal("expected VerifiedAt set after successful CAS")
	}
	if !got.VerifiedWithDNSSEC {
		t.Fatal("expected VerifiedWithDNSSEC=true after successful CAS")
	}
}

// TestCustomDomainStore_MarkVerified_NotFoundOnMissingRow — SIN-63104.
//
// Round-trips the discriminator that distinguishes "row missing" from
// "token rotated": when the row does not exist, the adapter must
// return ErrStoreNotFound rather than ErrTokenRotated.
func TestCustomDomainStore_MarkVerified_NotFoundOnMissingRow(t *testing.T) {
	h := startHarness(t)
	h.truncate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store := pgstore.NewCustomDomainStore(h.pool)
	_, err := store.MarkVerified(ctx, uuid.New(), "any", time.Now().UTC(), false, nil)
	if !errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v, want ErrStoreNotFound", err)
	}
}
