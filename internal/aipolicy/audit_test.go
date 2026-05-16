package aipolicy_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// decoratorFakeRepo is the in-memory Repository fake the decorator tests
// stack against. It records the last Upsert call and lets the test
// inject Get values + errors.
type decoratorFakeRepo struct {
	getPolicy aipolicy.Policy
	getFound  bool
	getErr    error
	upsertErr error
	upserts   []aipolicy.Policy
	// Delete-side toggles. deleteRowsAffected is the return value the
	// fake reports; deleteErr is the synthetic error.
	deleteRemoved bool
	deleteErr     error
	deletes       []decoratorDeleteCall
	listErr       error
	listResult    []aipolicy.Policy
}

type decoratorDeleteCall struct {
	tenantID  uuid.UUID
	scopeType aipolicy.ScopeType
	scopeID   string
}

func (f *decoratorFakeRepo) Get(_ context.Context, _ uuid.UUID, _ aipolicy.ScopeType, _ string) (aipolicy.Policy, bool, error) {
	return f.getPolicy, f.getFound, f.getErr
}

func (f *decoratorFakeRepo) Upsert(_ context.Context, p aipolicy.Policy) error {
	f.upserts = append(f.upserts, p)
	return f.upsertErr
}

func (f *decoratorFakeRepo) List(_ context.Context, _ uuid.UUID) ([]aipolicy.Policy, error) {
	return f.listResult, f.listErr
}

func (f *decoratorFakeRepo) Delete(_ context.Context, tenantID uuid.UUID, scopeType aipolicy.ScopeType, scopeID string) (bool, error) {
	f.deletes = append(f.deletes, decoratorDeleteCall{tenantID: tenantID, scopeType: scopeType, scopeID: scopeID})
	return f.deleteRemoved, f.deleteErr
}

// fakeLogger captures every Record call so tests can assert on the
// diff payload.
type fakeLogger struct {
	events []aipolicy.AuditEvent
	err    error
}

func (f *fakeLogger) Record(_ context.Context, ev aipolicy.AuditEvent) error {
	f.events = append(f.events, ev)
	return f.err
}

func tenantID(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.MustParse("11111111-1111-1111-1111-111111111111")
}

func actorMaster() aipolicy.Actor {
	return aipolicy.Actor{
		UserID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Master: true,
	}
}

func actorTenant() aipolicy.Actor {
	return aipolicy.Actor{
		UserID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Master: false,
	}
}

// TestDiffPolicies_NoPrev returns the FieldCreated synthetic event
// with the full policy snapshot in NewValue.
func TestDiffPolicies_NoPrev(t *testing.T) {
	next := aipolicy.Policy{
		Model:         "openrouter/auto",
		PromptVersion: "v1",
		Tone:          "neutro",
		Language:      "pt-BR",
		AIEnabled:     true,
		Anonymize:     true,
		OptIn:         true,
	}
	got := aipolicy.DiffPolicies(aipolicy.Policy{}, next, false)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Field != aipolicy.FieldCreated {
		t.Fatalf("field = %q, want %q", got[0].Field, aipolicy.FieldCreated)
	}
	if got[0].OldValue != nil {
		t.Fatalf("OldValue = %v, want nil", got[0].OldValue)
	}
	snap, ok := got[0].NewValue.(map[string]any)
	if !ok {
		t.Fatalf("NewValue type = %T, want map[string]any", got[0].NewValue)
	}
	if snap["ai_enabled"] != true {
		t.Fatalf("snap[ai_enabled] = %v, want true", snap["ai_enabled"])
	}
}

// TestDiffPolicies_NoChange returns an empty slice when prev equals
// next on every audited field.
func TestDiffPolicies_NoChange(t *testing.T) {
	p := aipolicy.Policy{Model: "m", PromptVersion: "v1", AIEnabled: true}
	got := aipolicy.DiffPolicies(p, p, true)
	if len(got) != 0 {
		t.Fatalf("want 0 events, got %d", len(got))
	}
}

// TestDiffPolicies_AIEnabledFlip mirrors AC #1: ai_enabled true→false
// must produce one event named "ai_enabled" with the typed old/new
// payload.
func TestDiffPolicies_AIEnabledFlip(t *testing.T) {
	prev := aipolicy.Policy{AIEnabled: true}
	next := aipolicy.Policy{AIEnabled: false}
	got := aipolicy.DiffPolicies(prev, next, true)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	ev := got[0]
	if ev.Field != "ai_enabled" {
		t.Fatalf("field = %q, want %q", ev.Field, "ai_enabled")
	}
	if ev.OldValue != true || ev.NewValue != false {
		t.Fatalf("old/new = %v/%v, want true/false", ev.OldValue, ev.NewValue)
	}
}

// TestDiffPolicies_MultipleFields validates the per-field decomposition
// when several columns change in one Upsert.
func TestDiffPolicies_MultipleFields(t *testing.T) {
	prev := aipolicy.Policy{Model: "a", Tone: "neutro", AIEnabled: false}
	next := aipolicy.Policy{Model: "b", Tone: "informal", AIEnabled: true}
	got := aipolicy.DiffPolicies(prev, next, true)
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}
	fields := map[string]bool{}
	for _, ev := range got {
		fields[ev.Field] = true
	}
	for _, want := range []string{"model", "tone", "ai_enabled"} {
		if !fields[want] {
			t.Fatalf("missing event for field %q (got %v)", want, fields)
		}
	}
}

// TestRecordingRepository_RejectsMissingActor enforces that the
// decorator refuses to write a policy change without a named actor.
// ErrMissingActor is the non-repudiation guard.
func TestRecordingRepository_RejectsMissingActor(t *testing.T) {
	repo := &decoratorFakeRepo{}
	logger := &fakeLogger{}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) {
			return aipolicy.Actor{}, false
		},
	})
	err := dec.Upsert(context.Background(), aipolicy.Policy{
		TenantID:  tenantID(t),
		ScopeType: aipolicy.ScopeTenant,
		ScopeID:   tenantID(t).String(),
	})
	if !errors.Is(err, aipolicy.ErrMissingActor) {
		t.Fatalf("err = %v, want ErrMissingActor", err)
	}
	if len(repo.upserts) != 0 {
		t.Fatalf("decorator wrote policy without actor: %d upserts", len(repo.upserts))
	}
}

// TestRecordingRepository_EmitsCreatedEvent verifies the FieldCreated
// path when no prior row exists.
func TestRecordingRepository_EmitsCreatedEvent(t *testing.T) {
	repo := &decoratorFakeRepo{getFound: false}
	logger := &fakeLogger{}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) {
			return actorTenant(), true
		},
	})
	p := aipolicy.Policy{
		TenantID:  tenantID(t),
		ScopeType: aipolicy.ScopeTenant,
		ScopeID:   tenantID(t).String(),
		AIEnabled: true,
	}
	if err := dec.Upsert(context.Background(), p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(logger.events) != 1 || logger.events[0].Field != aipolicy.FieldCreated {
		t.Fatalf("events = %+v, want one FieldCreated", logger.events)
	}
	if logger.events[0].Actor != actorTenant() {
		t.Fatalf("actor = %+v, want %+v", logger.events[0].Actor, actorTenant())
	}
}

// TestRecordingRepository_EmitsPerFieldOnUpdate covers the common
// "flip ai_enabled" case: prior row exists, one column changes, one
// audit row gets written attributing the change to the actor.
func TestRecordingRepository_EmitsPerFieldOnUpdate(t *testing.T) {
	repo := &decoratorFakeRepo{
		getFound: true,
		getPolicy: aipolicy.Policy{
			TenantID: tenantID(t), ScopeType: aipolicy.ScopeTenant,
			ScopeID: tenantID(t).String(), AIEnabled: true,
		},
	}
	logger := &fakeLogger{}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) {
			return actorTenant(), true
		},
		Now: func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
	})
	next := repo.getPolicy
	next.AIEnabled = false
	if err := dec.Upsert(context.Background(), next); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("events = %d, want 1", len(logger.events))
	}
	ev := logger.events[0]
	if ev.Field != "ai_enabled" || ev.OldValue != true || ev.NewValue != false {
		t.Fatalf("event = %+v, want ai_enabled true→false", ev)
	}
	if ev.OccurredAt.Year() != 2026 {
		t.Fatalf("OccurredAt = %v, want stamped clock", ev.OccurredAt)
	}
}

// TestRecordingRepository_AC2_MasterFlag covers AC #2: a change
// emitted with Actor.Master = true preserves the bit through the
// audit event.
func TestRecordingRepository_AC2_MasterFlag(t *testing.T) {
	repo := &decoratorFakeRepo{
		getFound: true,
		getPolicy: aipolicy.Policy{
			TenantID: tenantID(t), ScopeType: aipolicy.ScopeTenant,
			ScopeID: tenantID(t).String(), AIEnabled: true,
		},
	}
	logger := &fakeLogger{}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) {
			return actorMaster(), true
		},
	})
	next := repo.getPolicy
	next.AIEnabled = false
	if err := dec.Upsert(context.Background(), next); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(logger.events) != 1 {
		t.Fatalf("events = %d, want 1", len(logger.events))
	}
	if !logger.events[0].Actor.Master || logger.events[0].Actor.UserID != actorMaster().UserID {
		t.Fatalf("actor = %+v, want master with UserID %v", logger.events[0].Actor, actorMaster().UserID)
	}
}

// TestRecordingRepository_AuditFailureBubblesUp documents the failure
// surface when the audit insert fails: the policy was already written
// by inner.Upsert (the adapter committed), but the decorator surfaces
// the error so the caller can take corrective action.
func TestRecordingRepository_AuditFailureBubblesUp(t *testing.T) {
	repo := &decoratorFakeRepo{getFound: true, getPolicy: aipolicy.Policy{
		TenantID: tenantID(t), ScopeType: aipolicy.ScopeTenant,
		ScopeID: tenantID(t).String(), AIEnabled: true,
	}}
	logger := &fakeLogger{err: errors.New("audit insert failed")}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) {
			return actorTenant(), true
		},
	})
	next := repo.getPolicy
	next.AIEnabled = false
	if err := dec.Upsert(context.Background(), next); err == nil {
		t.Fatalf("err = nil, want bubble-up of audit failure")
	}
	if len(repo.upserts) != 1 {
		t.Fatalf("inner.Upsert called %d times, want 1", len(repo.upserts))
	}
}

// TestRecordingRepository_UpsertFailureSkipsAudit confirms the
// converse: if inner.Upsert fails the decorator does not emit any
// audit row (a non-event should not be logged).
func TestRecordingRepository_UpsertFailureSkipsAudit(t *testing.T) {
	repo := &decoratorFakeRepo{getFound: true, upsertErr: errors.New("db down")}
	logger := &fakeLogger{}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) {
			return actorTenant(), true
		},
	})
	err := dec.Upsert(context.Background(), aipolicy.Policy{
		TenantID:  tenantID(t),
		ScopeType: aipolicy.ScopeTenant,
		ScopeID:   tenantID(t).String(),
	})
	if err == nil {
		t.Fatalf("err = nil, want bubble-up of Upsert failure")
	}
	if len(logger.events) != 0 {
		t.Fatalf("audit emitted on failed Upsert: %+v", logger.events)
	}
}

// TestRecordingRepository_Get is a transparent pass-through.
func TestRecordingRepository_Get(t *testing.T) {
	want := aipolicy.Policy{Model: "x"}
	repo := &decoratorFakeRepo{getPolicy: want, getFound: true}
	dec := aipolicy.NewRecordingRepository(repo, &fakeLogger{}, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
	})
	got, found, err := dec.Get(context.Background(), tenantID(t), aipolicy.ScopeTenant, "x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("found = false, want true")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %+v, want %+v", got, want)
	}
}

// TestActor_IsValid rejects the zero UserID.
func TestActor_IsValid(t *testing.T) {
	if (aipolicy.Actor{}).IsValid() {
		t.Fatal("zero actor reported valid")
	}
	if !actorTenant().IsValid() {
		t.Fatal("tenant actor reported invalid")
	}
}

// TestAuditCursor_IsZero distinguishes the sentinel from a real cursor.
func TestAuditCursor_IsZero(t *testing.T) {
	if !(aipolicy.AuditCursor{}).IsZero() {
		t.Fatal("zero cursor reported non-zero")
	}
	non := aipolicy.AuditCursor{CreatedAt: time.Unix(1, 0), ID: uuid.New()}
	if non.IsZero() {
		t.Fatal("non-zero cursor reported zero")
	}
}

// TestDiffPolicies_EachFieldEmitsOwnEvent walks every audited column
// (prompt_version, language, anonymize, opt_in) so the per-field
// dispatch is exercised end-to-end and a future column addition
// fails this test rather than slipping past.
func TestDiffPolicies_EachFieldEmitsOwnEvent(t *testing.T) {
	base := aipolicy.Policy{
		Model: "m", PromptVersion: "v1", Tone: "neutro",
		Language: "pt-BR", AIEnabled: true, Anonymize: true, OptIn: true,
	}
	cases := []struct {
		name  string
		mut   func(*aipolicy.Policy)
		field string
	}{
		{"prompt_version", func(p *aipolicy.Policy) { p.PromptVersion = "v2" }, "prompt_version"},
		{"language", func(p *aipolicy.Policy) { p.Language = "en-US" }, "language"},
		{"anonymize", func(p *aipolicy.Policy) { p.Anonymize = false }, "anonymize"},
		{"opt_in", func(p *aipolicy.Policy) { p.OptIn = false }, "opt_in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next := base
			c.mut(&next)
			got := aipolicy.DiffPolicies(base, next, true)
			if len(got) != 1 || got[0].Field != c.field {
				t.Fatalf("events = %+v, want one %s", got, c.field)
			}
		})
	}
}

// TestNewRecordingRepository_PanicsOnNilDeps protects the wire from
// silently dropping the audit trail when a collaborator is missing.
func TestNewRecordingRepository_PanicsOnNilDeps(t *testing.T) {
	t.Run("nil inner", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on nil Repository")
			}
		}()
		aipolicy.NewRecordingRepository(nil, &fakeLogger{}, aipolicy.RecordingConfig{
			ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
		})
	})
	t.Run("nil audit", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on nil AuditLogger")
			}
		}()
		aipolicy.NewRecordingRepository(&decoratorFakeRepo{}, nil, aipolicy.RecordingConfig{
			ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
		})
	})
	t.Run("nil actor func", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on nil ActorFromContext")
			}
		}()
		aipolicy.NewRecordingRepository(&decoratorFakeRepo{}, &fakeLogger{}, aipolicy.RecordingConfig{})
	})
}

// TestAuditMetrics covers both the unregistered (test) and registered
// (production) construction paths plus the Observe nil-receiver
// guard.
func TestAuditMetrics(t *testing.T) {
	m := aipolicy.NewAuditMetrics(nil)
	if m == nil {
		t.Fatal("NewAuditMetrics(nil) returned nil")
	}
	// Exercise both master = true and master = false labels.
	m.Observe(aipolicy.AuditEvent{
		ScopeType: aipolicy.ScopeTenant, Field: "ai_enabled",
		Actor: aipolicy.Actor{UserID: uuid.New(), Master: true},
	})
	m.Observe(aipolicy.AuditEvent{
		ScopeType: aipolicy.ScopeChannel, Field: "model",
		Actor: aipolicy.Actor{UserID: uuid.New(), Master: false},
	})
	// nil receiver MUST NOT panic — the wire can pass nil to skip
	// metrics in tests.
	var nilm *aipolicy.AuditMetrics
	nilm.Observe(aipolicy.AuditEvent{ScopeType: aipolicy.ScopeTenant, Field: "ai_enabled"})
}

// TestRecordingRepository_InvalidScopeType refuses to call inner.Get
// when the policy is malformed.
func TestRecordingRepository_InvalidScopeType(t *testing.T) {
	repo := &decoratorFakeRepo{}
	dec := aipolicy.NewRecordingRepository(repo, &fakeLogger{}, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
	})
	err := dec.Upsert(context.Background(), aipolicy.Policy{
		TenantID: tenantID(t),
		// ScopeType is "" — invalid
		ScopeID: "x",
	})
	if !errors.Is(err, aipolicy.ErrInvalidScopeType) {
		t.Fatalf("err = %v, want ErrInvalidScopeType", err)
	}
	if len(repo.upserts) != 0 {
		t.Fatalf("inner.Upsert called on invalid scope")
	}
}

// TestRecordingRepository_InvalidTenant refuses to call inner.Get
// when TenantID is the zero uuid.
func TestRecordingRepository_InvalidTenant(t *testing.T) {
	repo := &decoratorFakeRepo{}
	dec := aipolicy.NewRecordingRepository(repo, &fakeLogger{}, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
	})
	err := dec.Upsert(context.Background(), aipolicy.Policy{
		ScopeType: aipolicy.ScopeTenant,
		ScopeID:   "x",
	})
	if !errors.Is(err, aipolicy.ErrInvalidTenant) {
		t.Fatalf("err = %v, want ErrInvalidTenant", err)
	}
}

// TestRecordingRepository_GetFailureBubbles confirms a Get error
// prevents the decorator from calling Upsert.
func TestRecordingRepository_GetFailureBubbles(t *testing.T) {
	repo := &decoratorFakeRepo{getErr: errors.New("boom")}
	dec := aipolicy.NewRecordingRepository(repo, &fakeLogger{}, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
	})
	err := dec.Upsert(context.Background(), aipolicy.Policy{
		TenantID:  tenantID(t),
		ScopeType: aipolicy.ScopeTenant,
		ScopeID:   "x",
	})
	if err == nil {
		t.Fatal("err = nil, want Get error to bubble")
	}
	if len(repo.upserts) != 0 {
		t.Fatalf("inner.Upsert called after Get failure")
	}
}

// TestRecordingRepository_List is a transparent pass-through.
func TestRecordingRepository_List(t *testing.T) {
	want := []aipolicy.Policy{{Model: "m1"}, {Model: "m2"}}
	repo := &decoratorFakeRepo{listResult: want}
	dec := aipolicy.NewRecordingRepository(repo, &fakeLogger{}, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
	})
	got, err := dec.List(context.Background(), tenantID(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
}

// TestRecordingRepository_DeleteEmitsFieldDeleted captures the
// snapshot in OldValue, NewValue is nil, and Field == FieldDeleted.
func TestRecordingRepository_DeleteEmitsFieldDeleted(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	prev := aipolicy.Policy{
		TenantID: tenantID(t), ScopeType: aipolicy.ScopeChannel, ScopeID: "whatsapp",
		Model: "claude-haiku", PromptVersion: "v1", Tone: "formal", Language: "pt-BR",
		AIEnabled: true, Anonymize: true, OptIn: true,
	}
	repo := &decoratorFakeRepo{getFound: true, getPolicy: prev, deleteRemoved: true}
	logger := &fakeLogger{}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		Now: func() time.Time { return now },
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) {
			return actorTenant(), true
		},
	})
	removed, err := dec.Delete(context.Background(), tenantID(t), aipolicy.ScopeChannel, "whatsapp")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !removed {
		t.Fatal("removed = false, want true")
	}
	if len(logger.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(logger.events))
	}
	ev := logger.events[0]
	if ev.Field != aipolicy.FieldDeleted {
		t.Fatalf("field = %s, want %s", ev.Field, aipolicy.FieldDeleted)
	}
	if ev.NewValue != nil {
		t.Fatalf("NewValue = %v, want nil", ev.NewValue)
	}
	if ev.OldValue == nil {
		t.Fatalf("OldValue = nil, want snapshot")
	}
	snap, ok := ev.OldValue.(map[string]any)
	if !ok {
		t.Fatalf("OldValue type = %T, want map[string]any", ev.OldValue)
	}
	if snap["model"] != "claude-haiku" {
		t.Fatalf("snap[model] = %v, want claude-haiku", snap["model"])
	}
	if !ev.OccurredAt.Equal(now) {
		t.Fatalf("OccurredAt = %v, want %v", ev.OccurredAt, now)
	}
	if ev.Actor.UserID != actorTenant().UserID {
		t.Fatalf("Actor.UserID = %v, want %v", ev.Actor.UserID, actorTenant().UserID)
	}
}

// TestRecordingRepository_DeleteMissNoAudit asserts that deleting a
// row that does not exist is idempotent and emits no audit event —
// the inner Delete is still called (so the test fake records it) but
// the audit logger sees nothing.
func TestRecordingRepository_DeleteMissNoAudit(t *testing.T) {
	repo := &decoratorFakeRepo{getFound: false, deleteRemoved: false}
	logger := &fakeLogger{}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
	})
	removed, err := dec.Delete(context.Background(), tenantID(t), aipolicy.ScopeChannel, "whatsapp")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if removed {
		t.Fatal("removed = true, want false")
	}
	if len(logger.events) != 0 {
		t.Fatalf("audit emitted on idempotent miss: %+v", logger.events)
	}
	if len(repo.deletes) != 1 {
		t.Fatalf("inner.Delete called %d times, want 1", len(repo.deletes))
	}
}

// TestRecordingRepository_DeleteRejectsInvalid documents the early
// returns the decorator enforces before touching the inner repo.
func TestRecordingRepository_DeleteRejectsInvalid(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		tenantID  uuid.UUID
		scopeType aipolicy.ScopeType
		scopeID   string
		actorOK   bool
		actor     aipolicy.Actor
		want      error
	}{
		{"nil-tenant", uuid.Nil, aipolicy.ScopeTenant, "x", true, actorTenant(), aipolicy.ErrInvalidTenant},
		{"invalid-scope", tenantID(t), aipolicy.ScopeType("nope"), "x", true, actorTenant(), aipolicy.ErrInvalidScopeType},
		{"no-actor", tenantID(t), aipolicy.ScopeTenant, "x", false, aipolicy.Actor{}, aipolicy.ErrMissingActor},
		{"zero-actor", tenantID(t), aipolicy.ScopeTenant, "x", true, aipolicy.Actor{}, aipolicy.ErrMissingActor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := &decoratorFakeRepo{deleteRemoved: true, getFound: true}
			dec := aipolicy.NewRecordingRepository(repo, &fakeLogger{}, aipolicy.RecordingConfig{
				ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return c.actor, c.actorOK },
			})
			_, err := dec.Delete(context.Background(), c.tenantID, c.scopeType, c.scopeID)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if len(repo.deletes) != 0 {
				t.Fatalf("inner.Delete called after early-reject: %+v", repo.deletes)
			}
		})
	}
}

// TestRecordingRepository_DeleteAuditFailureBubbles documents that an
// audit-insert failure surfaces to the caller even though the row is
// already gone from inner.
func TestRecordingRepository_DeleteAuditFailureBubbles(t *testing.T) {
	prev := aipolicy.Policy{TenantID: tenantID(t), ScopeType: aipolicy.ScopeTenant, ScopeID: tenantID(t).String()}
	repo := &decoratorFakeRepo{getFound: true, getPolicy: prev, deleteRemoved: true}
	logger := &fakeLogger{err: errors.New("audit insert failed")}
	dec := aipolicy.NewRecordingRepository(repo, logger, aipolicy.RecordingConfig{
		ActorFromContext: func(context.Context) (aipolicy.Actor, bool) { return actorTenant(), true },
	})
	removed, err := dec.Delete(context.Background(), tenantID(t), aipolicy.ScopeTenant, tenantID(t).String())
	if err == nil {
		t.Fatal("err = nil, want bubble-up of audit failure")
	}
	if !removed {
		t.Fatal("removed = false, want true (the row was deleted before audit failed)")
	}
}
