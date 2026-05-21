package consent

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// fakeRegistry is an in-memory ConsentRegistry suitable for unit
// tests. It is the in-process adapter referenced by quality-bar rule
// 5 — the production code path still goes through pgconsent + the
// real database in the integration suite; this fake exists only so
// the decorator can be exercised without a DB round-trip.
type fakeRegistry struct {
	rows        []ConsentRecord
	recordErr   error
	revokeErr   error
	lastRecord  ConsentRecord
	lastRevokeQ RevokeQuery
}

func (f *fakeRegistry) Record(ctx context.Context, rec ConsentRecord) (ConsentRecord, bool, error) {
	if f.recordErr != nil {
		return ConsentRecord{}, false, f.recordErr
	}
	f.lastRecord = rec
	for _, existing := range f.rows {
		if existing.TenantID == rec.TenantID &&
			existing.Subject == rec.Subject &&
			existing.Purpose == rec.Purpose &&
			existing.Version == rec.Version {
			return existing, false, nil
		}
	}
	persisted := rec
	persisted.ID = uuid.New()
	persisted.GrantedAt = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	persisted.Granted = true
	f.rows = append(f.rows, persisted)
	return persisted, true, nil
}

func (f *fakeRegistry) Latest(ctx context.Context, tenant uuid.UUID, subject Subject, purpose Purpose) (*ConsentRecord, error) {
	var latest *ConsentRecord
	for i := range f.rows {
		row := f.rows[i]
		if row.TenantID != tenant || row.Subject != subject || row.Purpose != purpose {
			continue
		}
		if latest == nil || row.GrantedAt.After(latest.GrantedAt) {
			r := row
			latest = &r
		}
	}
	return latest, nil
}

func (f *fakeRegistry) History(ctx context.Context, tenant uuid.UUID, subject Subject, purpose Purpose) ([]ConsentRecord, error) {
	out := make([]ConsentRecord, 0)
	for _, row := range f.rows {
		if row.TenantID == tenant && row.Subject == subject && row.Purpose == purpose {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeRegistry) Revoke(ctx context.Context, q RevokeQuery) (ConsentRecord, error) {
	if f.revokeErr != nil {
		return ConsentRecord{}, f.revokeErr
	}
	f.lastRevokeQ = q
	for i := range f.rows {
		row := &f.rows[i]
		if row.TenantID == q.TenantID && row.Subject == q.Subject && row.Purpose == q.Purpose && row.Granted {
			row.Granted = false
			ts := time.Date(2026, 5, 21, 13, 0, 0, 0, time.UTC)
			row.RevokedAt = &ts
			row.RevokeReason = q.Reason
			return *row, nil
		}
	}
	return ConsentRecord{}, ErrNoActiveGrant
}

// recordingSplitLogger captures the events the decorator emits so
// assertions can compare the audit trail without touching a DB.
type recordingSplitLogger struct {
	security []audit.SecurityAuditEvent
	data     []audit.DataAuditEvent
	writeErr error
}

func (r *recordingSplitLogger) WriteSecurity(ctx context.Context, ev audit.SecurityAuditEvent) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	r.security = append(r.security, ev)
	return nil
}

func (r *recordingSplitLogger) WriteData(ctx context.Context, ev audit.DataAuditEvent) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	r.data = append(r.data, ev)
	return nil
}

func fixedActorFromContext(actor uuid.UUID, present bool) func(context.Context) (uuid.UUID, bool) {
	return func(context.Context) (uuid.UUID, bool) {
		return actor, present
	}
}

func newTestRegistry(t *testing.T) (*RecordingRegistry, *fakeRegistry, *recordingSplitLogger, uuid.UUID) {
	t.Helper()
	inner := &fakeRegistry{}
	auditLog := &recordingSplitLogger{}
	actor := uuid.New()
	reg, err := NewRecordingRegistry(inner, auditLog, RecordingConfig{
		Now:              func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) },
		ActorFromContext: fixedActorFromContext(actor, true),
	})
	if err != nil {
		t.Fatalf("NewRecordingRegistry: %v", err)
	}
	return reg, inner, auditLog, actor
}

func goodRecord(tenant, subject uuid.UUID) ConsentRecord {
	return ConsentRecord{
		TenantID:  tenant,
		Subject:   Subject{Type: SubjectUser, ID: subject.String()},
		Purpose:   PurposeTermsOfService,
		Version:   "v2026-05",
		Granted:   true,
		IP:        netip.MustParseAddr("198.51.100.42"),
		UserAgent: "Mozilla/5.0",
	}
}

func TestNewRecordingRegistry_RejectsNilDependencies(t *testing.T) {
	t.Parallel()
	auditLog := &recordingSplitLogger{}
	inner := &fakeRegistry{}
	actor := fixedActorFromContext(uuid.New(), true)

	if _, err := NewRecordingRegistry(nil, auditLog, RecordingConfig{ActorFromContext: actor}); !errors.Is(err, ErrNilStore) {
		t.Errorf("nil inner: %v; want ErrNilStore", err)
	}
	if _, err := NewRecordingRegistry(inner, nil, RecordingConfig{ActorFromContext: actor}); !errors.Is(err, ErrNilAuditLogger) {
		t.Errorf("nil audit: %v; want ErrNilAuditLogger", err)
	}
	if _, err := NewRecordingRegistry(inner, auditLog, RecordingConfig{ActorFromContext: nil}); err == nil {
		t.Errorf("nil actor func: got nil; want error")
	}
}

func TestRecord_EmitsAuditOnCreate(t *testing.T) {
	t.Parallel()
	reg, _, audit, actor := newTestRegistry(t)
	tenant := uuid.New()
	rec := goodRecord(tenant, uuid.New())

	got, created, err := reg.Record(context.Background(), rec)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !created {
		t.Fatal("created = false; want true on first Record")
	}
	if got.ID == uuid.Nil {
		t.Errorf("persisted ID = uuid.Nil")
	}
	if len(audit.data) != 1 {
		t.Fatalf("audit.data len = %d; want 1", len(audit.data))
	}
	ev := audit.data[0]
	if ev.Event != "consent_grant" {
		t.Errorf("event = %q; want consent_grant", ev.Event)
	}
	if ev.ActorUserID != actor {
		t.Errorf("actor = %v; want %v", ev.ActorUserID, actor)
	}
	if ev.TenantID != tenant {
		t.Errorf("tenant = %v; want %v", ev.TenantID, tenant)
	}
	if ev.Target["consent_id"] != got.ID.String() {
		t.Errorf("target.consent_id = %v; want %v", ev.Target["consent_id"], got.ID.String())
	}
	if ev.Target["ip"] != "198.51.100.42" {
		t.Errorf("target.ip = %v; want 198.51.100.42", ev.Target["ip"])
	}
	if ev.Target["user_agent"] != "Mozilla/5.0" {
		t.Errorf("target.user_agent = %v; want Mozilla/5.0", ev.Target["user_agent"])
	}
}

func TestRecord_IdempotentNoOpSkipsAudit(t *testing.T) {
	t.Parallel()
	reg, _, audit, _ := newTestRegistry(t)
	rec := goodRecord(uuid.New(), uuid.New())

	if _, created, err := reg.Record(context.Background(), rec); err != nil || !created {
		t.Fatalf("first Record: created=%v err=%v", created, err)
	}
	if _, created, err := reg.Record(context.Background(), rec); err != nil {
		t.Fatalf("second Record: %v", err)
	} else if created {
		t.Errorf("second Record created=true; want false")
	}
	if len(audit.data) != 1 {
		t.Errorf("audit emissions = %d; want 1 (no-op must not re-audit)", len(audit.data))
	}
}

func TestRecord_ValidationRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(r *ConsentRecord)
		want error
	}{
		{"zero tenant", func(r *ConsentRecord) { r.TenantID = uuid.Nil }, ErrInvalidTenant},
		{"bad subject type", func(r *ConsentRecord) { r.Subject.Type = SubjectType("admin") }, ErrInvalidSubjectType},
		{"blank subject id", func(r *ConsentRecord) { r.Subject.ID = " " }, ErrInvalidSubjectID},
		{"bad purpose", func(r *ConsentRecord) { r.Purpose = Purpose("ai") }, ErrInvalidPurpose},
		{"blank version", func(r *ConsentRecord) { r.Version = "" }, ErrInvalidVersion},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, _, _, _ := newTestRegistry(t)
			rec := goodRecord(uuid.New(), uuid.New())
			tc.mut(&rec)
			_, _, err := reg.Record(context.Background(), rec)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestRecord_MissingActor(t *testing.T) {
	t.Parallel()
	inner := &fakeRegistry{}
	auditLog := &recordingSplitLogger{}
	reg, err := NewRecordingRegistry(inner, auditLog, RecordingConfig{
		ActorFromContext: fixedActorFromContext(uuid.Nil, false),
	})
	if err != nil {
		t.Fatalf("NewRecordingRegistry: %v", err)
	}
	rec := goodRecord(uuid.New(), uuid.New())
	if _, _, err := reg.Record(context.Background(), rec); err == nil {
		t.Errorf("Record with missing actor: nil; want error")
	}
}

func TestRecord_AuditFailureBubbles(t *testing.T) {
	t.Parallel()
	inner := &fakeRegistry{}
	auditLog := &recordingSplitLogger{writeErr: errors.New("boom")}
	reg, err := NewRecordingRegistry(inner, auditLog, RecordingConfig{
		ActorFromContext: fixedActorFromContext(uuid.New(), true),
	})
	if err != nil {
		t.Fatalf("NewRecordingRegistry: %v", err)
	}
	rec := goodRecord(uuid.New(), uuid.New())
	_, _, err = reg.Record(context.Background(), rec)
	if err == nil {
		t.Errorf("Record with audit failure: nil; want error")
	}
}

func TestRevoke_EmitsAudit(t *testing.T) {
	t.Parallel()
	reg, _, audit, actor := newTestRegistry(t)
	tenant := uuid.New()
	subject := Subject{Type: SubjectUser, ID: uuid.New().String()}
	rec := goodRecord(tenant, uuid.MustParse(subject.ID))
	rec.Subject = subject
	if _, _, err := reg.Record(context.Background(), rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	revoked, err := reg.Revoke(context.Background(), RevokeQuery{
		TenantID:  tenant,
		Subject:   subject,
		Purpose:   PurposeTermsOfService,
		Reason:    "user request",
		IP:        netip.MustParseAddr("198.51.100.43"),
		UserAgent: "Mozilla/6.0",
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
	if len(audit.data) != 2 {
		t.Fatalf("audit.data len = %d; want 2 (grant + revoke)", len(audit.data))
	}
	ev := audit.data[1]
	if ev.Event != "consent_revoke" {
		t.Errorf("event = %q; want consent_revoke", ev.Event)
	}
	if ev.ActorUserID != actor {
		t.Errorf("actor = %v; want %v", ev.ActorUserID, actor)
	}
	if ev.Target["reason"] != "user request" {
		t.Errorf("target.reason = %v; want 'user request'", ev.Target["reason"])
	}
}

func TestRevoke_NoActiveGrant(t *testing.T) {
	t.Parallel()
	reg, _, audit, _ := newTestRegistry(t)
	_, err := reg.Revoke(context.Background(), RevokeQuery{
		TenantID: uuid.New(),
		Subject:  Subject{Type: SubjectUser, ID: "u-1"},
		Purpose:  PurposeTermsOfService,
		Reason:   "test",
	})
	if !errors.Is(err, ErrNoActiveGrant) {
		t.Errorf("err = %v; want ErrNoActiveGrant", err)
	}
	if len(audit.data) != 0 {
		t.Errorf("audit emissions on missing grant = %d; want 0", len(audit.data))
	}
}

func TestRevoke_ValidationRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		mut  func(q *RevokeQuery)
		want error
	}{
		{"zero tenant", func(q *RevokeQuery) { q.TenantID = uuid.Nil }, ErrInvalidTenant},
		{"bad subject type", func(q *RevokeQuery) { q.Subject.Type = SubjectType("admin") }, ErrInvalidSubjectType},
		{"blank subject id", func(q *RevokeQuery) { q.Subject.ID = "" }, ErrInvalidSubjectID},
		{"bad purpose", func(q *RevokeQuery) { q.Purpose = Purpose("ai") }, ErrInvalidPurpose},
		{"blank reason", func(q *RevokeQuery) { q.Reason = " " }, ErrInvalidRevokeReason},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, _, _, _ := newTestRegistry(t)
			q := RevokeQuery{
				TenantID: uuid.New(),
				Subject:  Subject{Type: SubjectUser, ID: "u-1"},
				Purpose:  PurposeTermsOfService,
				Reason:   "ok",
			}
			tc.mut(&q)
			_, err := reg.Revoke(context.Background(), q)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestLatestAndHistory_DelegateToInner(t *testing.T) {
	t.Parallel()
	reg, _, _, _ := newTestRegistry(t)
	tenant := uuid.New()
	subject := Subject{Type: SubjectContact, ID: "c-1"}
	rec := ConsentRecord{
		TenantID: tenant,
		Subject:  subject,
		Purpose:  PurposeMarketing,
		Version:  "v1",
		Granted:  true,
	}
	if _, _, err := reg.Record(context.Background(), rec); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := reg.Latest(context.Background(), tenant, subject, PurposeMarketing)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got == nil {
		t.Fatal("Latest = nil; want row")
	}
	if got.Purpose != PurposeMarketing {
		t.Errorf("Latest.Purpose = %q; want marketing", got.Purpose)
	}

	hist, err := reg.History(context.Background(), tenant, subject, PurposeMarketing)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 1 {
		t.Errorf("History len = %d; want 1", len(hist))
	}
}

func TestLatest_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	reg, _, _, _ := newTestRegistry(t)
	got, err := reg.Latest(context.Background(), uuid.New(), Subject{Type: SubjectUser, ID: "missing"}, PurposeTermsOfService)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got != nil {
		t.Errorf("Latest with no rows = %+v; want nil", got)
	}
}
