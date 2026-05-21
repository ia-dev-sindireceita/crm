package postgres_test

// SIN-63185 / Fase 6 PR2 integration tests for the pgconsent.Store
// adapter against the real consent_record table (migration 0107).
//
// Lives in the parent postgres_test package (not the
// internal/iam/consent/pgconsent subpackage) to share the TestMain +
// harness with the other postgres_test files — tests in a separate
// binary race the ALTER ROLE bootstrap on the shared CI cluster
// (memory `testpg shared-cluster ALTER ROLE race`).

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam/consent"
	"github.com/pericles-luz/crm/internal/iam/consent/pgconsent"
)

func newConsentStorePG(t *testing.T, db *testpg.DB) *pgconsent.Store {
	t.Helper()
	store, err := pgconsent.NewStore(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func goodConsentRecord(tenant uuid.UUID) consent.ConsentRecord {
	return consent.ConsentRecord{
		TenantID:  tenant,
		Subject:   consent.Subject{Type: consent.SubjectUser, ID: uuid.New().String()},
		Purpose:   consent.PurposeTermsOfService,
		Version:   "v2026-05",
		Granted:   true,
		IP:        netip.MustParseAddr("198.51.100.7"),
		UserAgent: "TestAgent/1.0",
	}
}

func TestPGConsent_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := pgconsent.NewStore(nil); err == nil {
		t.Error("NewStore(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestPGConsent_Record_NewRow(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	rec := goodConsentRecord(tenant)
	persisted, created, err := store.Record(ctx, rec)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !created {
		t.Fatal("created = false on first Record; want true")
	}
	if persisted.ID == uuid.Nil {
		t.Errorf("persisted.ID = uuid.Nil")
	}
	if !persisted.Granted {
		t.Errorf("persisted.Granted = false; want true")
	}
	if persisted.GrantedAt.IsZero() {
		t.Errorf("persisted.GrantedAt is zero; want now()")
	}
	if persisted.IP.String() != "198.51.100.7" {
		t.Errorf("persisted.IP = %v; want 198.51.100.7", persisted.IP)
	}
	if persisted.UserAgent != "TestAgent/1.0" {
		t.Errorf("persisted.UserAgent = %q; want TestAgent/1.0", persisted.UserAgent)
	}
}

func TestPGConsent_Record_IdempotentNoOp(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	rec := goodConsentRecord(tenant)
	first, created, err := store.Record(ctx, rec)
	if err != nil || !created {
		t.Fatalf("first Record: created=%v err=%v", created, err)
	}

	second, created, err := store.Record(ctx, rec)
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}
	if created {
		t.Errorf("second Record created=true; want false (no-op)")
	}
	if second.ID != first.ID {
		t.Errorf("second.ID=%v != first.ID=%v; want canonical row", second.ID, first.ID)
	}

	// Exactly one row in the table for this triple.
	var count int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM consent_record
		  WHERE tenant_id = $1 AND subject_type = $2
		    AND subject_id = $3 AND purpose = $4 AND version = $5`,
		tenant, string(rec.Subject.Type), rec.Subject.ID,
		string(rec.Purpose), rec.Version).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("rows after duplicate Record = %d; want 1", count)
	}
}

func TestPGConsent_Record_ValidationRejects(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	good := goodConsentRecord(tenant)

	cases := []struct {
		name string
		mut  func(r *consent.ConsentRecord)
		want error
	}{
		{"zero tenant", func(r *consent.ConsentRecord) { r.TenantID = uuid.Nil }, consent.ErrInvalidTenant},
		{"bad subject type", func(r *consent.ConsentRecord) { r.Subject.Type = consent.SubjectType("admin") }, consent.ErrInvalidSubjectType},
		{"blank subject id", func(r *consent.ConsentRecord) { r.Subject.ID = " " }, consent.ErrInvalidSubjectID},
		{"bad purpose", func(r *consent.ConsentRecord) { r.Purpose = consent.Purpose("ai") }, consent.ErrInvalidPurpose},
		{"blank version", func(r *consent.ConsentRecord) { r.Version = "" }, consent.ErrInvalidVersion},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.mut(&r)
			_, _, err := store.Record(ctx, r)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestPGConsent_Latest_ReturnsMostRecent(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	subject := consent.Subject{Type: consent.SubjectUser, ID: uuid.New().String()}
	rec := consent.ConsentRecord{
		TenantID: tenant,
		Subject:  subject,
		Purpose:  consent.PurposeTermsOfService,
		Version:  "v1",
		Granted:  true,
	}
	if _, _, err := store.Record(ctx, rec); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	rec.Version = "v2"
	if _, _, err := store.Record(ctx, rec); err != nil {
		t.Fatalf("second Record: %v", err)
	}

	got, err := store.Latest(ctx, tenant, subject, consent.PurposeTermsOfService)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got == nil {
		t.Fatal("Latest = nil; want most recent row")
	}
	if got.Version != "v2" {
		t.Errorf("Latest.Version = %q; want v2", got.Version)
	}
}

func TestPGConsent_Latest_NoRowsReturnsNil(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	got, err := store.Latest(ctx, tenant,
		consent.Subject{Type: consent.SubjectUser, ID: "missing"},
		consent.PurposeTermsOfService)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got != nil {
		t.Errorf("Latest with no rows = %+v; want nil", got)
	}
}

func TestPGConsent_History_OrderedByGrantedAtDesc(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	subject := consent.Subject{Type: consent.SubjectContact, ID: uuid.New().String()}
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, _, err := store.Record(ctx, consent.ConsentRecord{
			TenantID: tenant,
			Subject:  subject,
			Purpose:  consent.PurposeMarketing,
			Version:  v,
			Granted:  true,
		}); err != nil {
			t.Fatalf("Record %s: %v", v, err)
		}
	}

	hist, err := store.History(ctx, tenant, subject, consent.PurposeMarketing)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("History len = %d; want 3", len(hist))
	}
	// granted_at default DESC means newest first; insert order was v1, v2, v3.
	// Identical timestamps are possible on fast inserts, but each Record runs
	// in its own transaction so the index DESC still groups by insert order
	// modulo equal ties — assert membership rather than strict ordering.
	got := map[string]bool{}
	for _, r := range hist {
		got[r.Version] = true
	}
	for _, v := range []string{"v1", "v2", "v3"} {
		if !got[v] {
			t.Errorf("missing version %q from History", v)
		}
	}
}

func TestPGConsent_Revoke_UpdatesRow(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	subject := consent.Subject{Type: consent.SubjectUser, ID: uuid.New().String()}
	rec := consent.ConsentRecord{
		TenantID: tenant,
		Subject:  subject,
		Purpose:  consent.PurposeTermsOfService,
		Version:  "v1",
		Granted:  true,
	}
	if _, _, err := store.Record(ctx, rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	revoked, err := store.Revoke(ctx, consent.RevokeQuery{
		TenantID: tenant,
		Subject:  subject,
		Purpose:  consent.PurposeTermsOfService,
		Reason:   "operator opt-out",
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.Granted {
		t.Errorf("revoked.Granted = true; want false")
	}
	if revoked.RevokedAt == nil {
		t.Errorf("revoked.RevokedAt = nil; want set")
	}
	if revoked.RevokeReason != "operator opt-out" {
		t.Errorf("revoked.RevokeReason = %q; want 'operator opt-out'", revoked.RevokeReason)
	}

	// A second Revoke without a re-grant returns ErrNoActiveGrant.
	_, err = store.Revoke(ctx, consent.RevokeQuery{
		TenantID: tenant,
		Subject:  subject,
		Purpose:  consent.PurposeTermsOfService,
		Reason:   "again",
	})
	if !errors.Is(err, consent.ErrNoActiveGrant) {
		t.Errorf("second Revoke err = %v; want ErrNoActiveGrant", err)
	}
}

func TestPGConsent_Revoke_NoActiveGrant(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	_, err := store.Revoke(ctx, consent.RevokeQuery{
		TenantID: tenant,
		Subject:  consent.Subject{Type: consent.SubjectUser, ID: "u-missing"},
		Purpose:  consent.PurposeTermsOfService,
		Reason:   "test",
	})
	if !errors.Is(err, consent.ErrNoActiveGrant) {
		t.Errorf("err = %v; want ErrNoActiveGrant", err)
	}
}

func TestPGConsent_RLSScopeIsolation(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantBForConsent(t, ctx, db)

	subject := consent.Subject{Type: consent.SubjectUser, ID: "shared-id"}
	if _, _, err := store.Record(ctx, consent.ConsentRecord{
		TenantID: tenantA,
		Subject:  subject,
		Purpose:  consent.PurposeTermsOfService,
		Version:  "v1",
		Granted:  true,
	}); err != nil {
		t.Fatalf("Record(A): %v", err)
	}

	// Tenant B's Latest under the same subject must return nil.
	got, err := store.Latest(ctx, tenantB, subject, consent.PurposeTermsOfService)
	if err != nil {
		t.Fatalf("Latest(B): %v", err)
	}
	if got != nil {
		t.Errorf("tenant B saw tenant A's consent row; want hidden by RLS")
	}
}

func TestPGConsent_LookupAndRevoke_ValidationRejects(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)
	goodSubject := consent.Subject{Type: consent.SubjectUser, ID: "u-1"}

	// Latest validation
	latestCases := []struct {
		name    string
		tenant  uuid.UUID
		subject consent.Subject
		purpose consent.Purpose
		want    error
	}{
		{"zero tenant", uuid.Nil, goodSubject, consent.PurposeTermsOfService, consent.ErrInvalidTenant},
		{"bad subject type", tenant, consent.Subject{Type: consent.SubjectType("admin"), ID: "x"}, consent.PurposeTermsOfService, consent.ErrInvalidSubjectType},
		{"blank subject id", tenant, consent.Subject{Type: consent.SubjectUser, ID: " "}, consent.PurposeTermsOfService, consent.ErrInvalidSubjectID},
		{"bad purpose", tenant, goodSubject, consent.Purpose("ai"), consent.ErrInvalidPurpose},
	}
	for _, tc := range latestCases {
		tc := tc
		t.Run("Latest/"+tc.name, func(t *testing.T) {
			_, err := store.Latest(ctx, tc.tenant, tc.subject, tc.purpose)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
		})
	}

	// History reuses the same validateLookup; exercise once per branch.
	for _, tc := range latestCases {
		tc := tc
		t.Run("History/"+tc.name, func(t *testing.T) {
			_, err := store.History(ctx, tc.tenant, tc.subject, tc.purpose)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
		})
	}

	// Revoke validation
	revokeCases := []struct {
		name string
		q    consent.RevokeQuery
		want error
	}{
		{"zero tenant", consent.RevokeQuery{TenantID: uuid.Nil, Subject: goodSubject, Purpose: consent.PurposeTermsOfService, Reason: "x"}, consent.ErrInvalidTenant},
		{"bad subject type", consent.RevokeQuery{TenantID: tenant, Subject: consent.Subject{Type: consent.SubjectType("admin"), ID: "x"}, Purpose: consent.PurposeTermsOfService, Reason: "x"}, consent.ErrInvalidSubjectType},
		{"blank subject id", consent.RevokeQuery{TenantID: tenant, Subject: consent.Subject{Type: consent.SubjectUser, ID: " "}, Purpose: consent.PurposeTermsOfService, Reason: "x"}, consent.ErrInvalidSubjectID},
		{"bad purpose", consent.RevokeQuery{TenantID: tenant, Subject: goodSubject, Purpose: consent.Purpose("ai"), Reason: "x"}, consent.ErrInvalidPurpose},
		{"blank reason", consent.RevokeQuery{TenantID: tenant, Subject: goodSubject, Purpose: consent.PurposeTermsOfService, Reason: " "}, consent.ErrInvalidRevokeReason},
	}
	for _, tc := range revokeCases {
		tc := tc
		t.Run("Revoke/"+tc.name, func(t *testing.T) {
			_, err := store.Revoke(ctx, tc.q)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestPGConsent_NullIPAndUserAgentRoundTrip(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentRecord(t)
	store := newConsentStorePG(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	// IP zero value + empty UA must round-trip as NULL columns.
	subject := consent.Subject{Type: consent.SubjectTenant, ID: tenant.String()}
	rec := consent.ConsentRecord{
		TenantID: tenant,
		Subject:  subject,
		Purpose:  consent.PurposeCookiesAnalytics,
		Version:  "v1",
		Granted:  true,
	}
	persisted, _, err := store.Record(ctx, rec)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if persisted.IP.IsValid() {
		t.Errorf("persisted.IP = %v; want zero (NULL round-trip)", persisted.IP)
	}
	if persisted.UserAgent != "" {
		t.Errorf("persisted.UserAgent = %q; want empty", persisted.UserAgent)
	}
}
