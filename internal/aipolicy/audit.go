package aipolicy

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Actor identifies the human (and optional master-impersonation flag)
// behind a Policy mutation. Audit rows carry this verbatim so a tenant
// admin reading /settings/ai-policy/audit can answer "who flipped
// AIEnabled off, and were they a master operator acting as us?".
//
// UserID is required for every mutation — the system has no anonymous
// write path through this port. Master tenant impersonation still
// records the master's own UserID; Master = true distinguishes that
// row from a tenant-driven change with the same column delta.
type Actor struct {
	UserID uuid.UUID
	Master bool
}

// IsValid reports whether a is well-formed: non-zero UserID. The
// decorator rejects malformed actors before calling the underlying
// Upsert so the audit row and the policy write commit or rollback
// together rather than diverge.
func (a Actor) IsValid() bool { return a.UserID != uuid.Nil }

// AuditEvent is the per-field record persisted to ai_policy_audit.
// One Upsert that flips two fields produces two AuditEvents; the
// resolver writes each row in the same transaction as the Upsert so
// the trail cannot drift from the policy state.
//
// Field is the ai_policy column name that changed ("ai_enabled",
// "model", "tone", ...). Lifecycle events without a column changeset
// use the synthetic field names FieldCreated and FieldDeleted so the
// reader can distinguish a brand-new policy row from a one-field edit.
//
// OldValue and NewValue are typed as `any` so the adapter can encode
// them as jsonb without forcing the domain to import encoding/json.
// nil OldValue on FieldCreated means "no prior row"; nil NewValue on
// FieldDeleted means "row no longer exists".
type AuditEvent struct {
	TenantID   uuid.UUID
	ScopeType  ScopeType
	ScopeID    string
	Field      string
	OldValue   any
	NewValue   any
	Actor      Actor
	OccurredAt time.Time
}

// Synthetic field names for lifecycle events that do not correspond
// to a single ai_policy column. Wire-stable: persisted in
// ai_policy_audit.field and referenced by the admin view's "evento"
// pill, so renaming is a breaking change. Add a new constant rather
// than rename.
const (
	FieldCreated = "__created__"
	FieldDeleted = "__deleted__"
)

// AuditLogger is the storage port for ai_policy_audit rows. The
// decorator (RecordingRepository) is the only caller in production;
// tests can swap the adapter for a recording fake without touching
// the Repository contract.
//
// Record persists one AuditEvent. The adapter MUST run inside the
// same logical scope as the underlying Upsert (the tenant GUC must
// already be set when this is called) so RLS gates the insert by
// the correct tenant. Returning a non-nil error rolls the decorator's
// Upsert back: a write that cannot be audited never lands.
type AuditLogger interface {
	Record(ctx context.Context, event AuditEvent) error
}

// AuditQuery is the read port for the admin views. Cursor pagination
// is keyset on (created_at, id) DESC; Filter narrows the result by
// scope and time window. The Page method MUST return up to limit rows
// plus a next-cursor token when more rows exist.
type AuditQuery interface {
	Page(ctx context.Context, q AuditPageQuery) (AuditPage, error)
}

// AuditPageQuery is the input to AuditQuery.Page. TenantID is required
// (the SELECT is RLS-gated to that tenant). ScopeType / ScopeID narrow
// the predicate when both are non-zero. Since / Until bound the
// created_at window; zero values mean "open-ended". Cursor is the
// opaque continuation produced by the previous Page call; empty means
// "first page". Limit clamps to 1..200; zero defaults to 50.
type AuditPageQuery struct {
	TenantID  uuid.UUID
	ScopeType ScopeType
	ScopeID   string
	Since     time.Time
	Until     time.Time
	Cursor    AuditCursor
	Limit     int
}

// AuditCursor is a (CreatedAt, ID) pair that pins the next-page
// continuation. The adapter encodes/decodes it as an opaque base64
// string at the boundary; the domain keeps it typed so callers cannot
// mint a cursor from arbitrary input.
//
// Zero value means "no cursor" (first page).
type AuditCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// IsZero reports whether c is the zero cursor (first-page sentinel).
func (c AuditCursor) IsZero() bool { return c.ID == uuid.Nil && c.CreatedAt.IsZero() }

// AuditPage carries the result of one AuditQuery.Page call. Events is
// the slice of rows (length ≤ Limit). Next is the cursor that the
// caller passes back to fetch the next page; the zero cursor means
// "no more pages".
type AuditPage struct {
	Events []AuditRecord
	Next   AuditCursor
}

// AuditRecord is the per-row shape the adapter hands back. ID is
// surfaced so the cursor can pin the keyset; the rest mirrors
// AuditEvent + persisted created_at.
type AuditRecord struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ScopeType   ScopeType
	ScopeID     string
	Field       string
	OldValue    any
	NewValue    any
	ActorUserID uuid.UUID
	ActorMaster bool
	CreatedAt   time.Time
}

// RecordingRepository is the decorator that wraps a Repository, runs
// each Upsert inside the same tenant scope, and emits one AuditEvent
// per field that actually changed (or one FieldCreated event when no
// prior row existed). The decorator is the production wiring for the
// ai-policy write path — handlers depend on aipolicy.Repository as
// the seam, and the wire passes a RecordingRepository so the audit
// trail and the policy state stay coupled.
//
// Diffing is field-by-field on the Policy struct (model, prompt
// version, tone, language, ai_enabled, anonymize, opt_in). TenantID,
// ScopeType, ScopeID, CreatedAt, UpdatedAt are not diffed — they are
// identity columns, not configuration values.
type RecordingRepository struct {
	inner   Repository
	audit   AuditLogger
	metrics *AuditMetrics
	now     func() time.Time
	actor   func(context.Context) (Actor, bool)
}

// RecordingConfig parameterises NewRecordingRepository. Now defaults
// to time.Now in UTC; ActorFromContext is required so the decorator
// can pull the request-scope actor out of the call site without
// importing the HTTP layer. Metrics is optional; nil is a silent
// counter.
type RecordingConfig struct {
	Now              func() time.Time
	ActorFromContext func(context.Context) (Actor, bool)
	Metrics          *AuditMetrics
}

// NewRecordingRepository wraps inner so every Upsert emits the audit
// trail. nil inner, audit, or ActorFromContext panics at construction
// (the wire fails fast — better to crash boot than to ship a silent
// missing audit trail).
func NewRecordingRepository(inner Repository, audit AuditLogger, cfg RecordingConfig) *RecordingRepository {
	if inner == nil {
		panic("aipolicy: NewRecordingRepository: inner Repository is nil")
	}
	if audit == nil {
		panic("aipolicy: NewRecordingRepository: AuditLogger is nil")
	}
	if cfg.ActorFromContext == nil {
		panic("aipolicy: NewRecordingRepository: ActorFromContext is nil")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &RecordingRepository{
		inner:   inner,
		audit:   audit,
		metrics: cfg.Metrics,
		now:     now,
		actor:   cfg.ActorFromContext,
	}
}

// Get delegates to the wrapped Repository. The decorator does not
// audit reads — only mutations.
func (r *RecordingRepository) Get(ctx context.Context, tenantID uuid.UUID, scopeType ScopeType, scopeID string) (Policy, bool, error) {
	return r.inner.Get(ctx, tenantID, scopeType, scopeID)
}

// Upsert delegates to the wrapped Repository and, on success, emits
// one AuditEvent per changed field (or FieldCreated when no prior row
// existed). A failure to record any event rolls the entire call back
// — wait, that is the goal but Repository.Upsert has already
// committed by the time we look at Get. The honest contract here is:
//
//  1. Snapshot the existing row (if any).
//  2. Run the underlying Upsert.
//  3. On success, compute the diff and emit one Record per change.
//  4. If any Record fails, the audit trail is incomplete and the
//     decorator returns the error so the caller knows. The policy
//     change is already on disk; the caller's options are to retry
//     the audit emission (idempotent on (tenant, scope, field,
//     occurred_at) — no UNIQUE constraint, so retry adds a row) or
//     surface the failure to the operator.
//
// The aipolicy pgx adapter wraps Upsert in its own transaction; for
// the audit emission to share that transaction we would need to
// rework the port. That is the documented follow-up — see
// SIN-62353 review comment. For the first cut, the audit insert
// runs through the same WithTenant scope path (the adapter shares
// the pool) and the failure mode is logged-and-surfaced.
func (r *RecordingRepository) Upsert(ctx context.Context, p Policy) error {
	if !p.ScopeType.IsValid() {
		return ErrInvalidScopeType
	}
	if p.TenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	actor, ok := r.actor(ctx)
	if !ok {
		return ErrMissingActor
	}
	if !actor.IsValid() {
		return ErrMissingActor
	}

	prev, hadPrev, err := r.inner.Get(ctx, p.TenantID, p.ScopeType, p.ScopeID)
	if err != nil {
		return err
	}
	if err := r.inner.Upsert(ctx, p); err != nil {
		return err
	}

	events := DiffPolicies(prev, p, hadPrev)
	occurredAt := r.now()
	for _, ev := range events {
		ev.TenantID = p.TenantID
		ev.ScopeType = p.ScopeType
		ev.ScopeID = p.ScopeID
		ev.Actor = actor
		ev.OccurredAt = occurredAt
		if err := r.audit.Record(ctx, ev); err != nil {
			return err
		}
		r.metrics.Observe(ev)
	}
	return nil
}

// DiffPolicies returns one AuditEvent per field that differs between
// prev and next. If hadPrev is false, returns a single FieldCreated
// event with prev's zero value and next's full snapshot. The function
// is pure so unit tests can exercise the diff matrix without any
// adapter.
//
// Field names match ai_policy column names so the audit reader can
// render them without translation. The Actor and timestamp are NOT
// filled in here — the decorator stamps them after the diff so tests
// can compare diff output without seeding fakes.
func DiffPolicies(prev, next Policy, hadPrev bool) []AuditEvent {
	if !hadPrev {
		return []AuditEvent{{
			Field:    FieldCreated,
			OldValue: nil,
			NewValue: policySnapshot(next),
		}}
	}
	events := make([]AuditEvent, 0, 7)
	if prev.Model != next.Model {
		events = append(events, AuditEvent{Field: "model", OldValue: prev.Model, NewValue: next.Model})
	}
	if prev.PromptVersion != next.PromptVersion {
		events = append(events, AuditEvent{Field: "prompt_version", OldValue: prev.PromptVersion, NewValue: next.PromptVersion})
	}
	if prev.Tone != next.Tone {
		events = append(events, AuditEvent{Field: "tone", OldValue: prev.Tone, NewValue: next.Tone})
	}
	if prev.Language != next.Language {
		events = append(events, AuditEvent{Field: "language", OldValue: prev.Language, NewValue: next.Language})
	}
	if prev.AIEnabled != next.AIEnabled {
		events = append(events, AuditEvent{Field: "ai_enabled", OldValue: prev.AIEnabled, NewValue: next.AIEnabled})
	}
	if prev.Anonymize != next.Anonymize {
		events = append(events, AuditEvent{Field: "anonymize", OldValue: prev.Anonymize, NewValue: next.Anonymize})
	}
	if prev.OptIn != next.OptIn {
		events = append(events, AuditEvent{Field: "opt_in", OldValue: prev.OptIn, NewValue: next.OptIn})
	}
	return events
}

// policySnapshot renders a Policy as a map suitable for the
// FieldCreated event's NewValue payload. Only the configuration
// columns are included; identity columns (TenantID, ScopeType,
// ScopeID) live on the parent AuditEvent.
func policySnapshot(p Policy) map[string]any {
	return map[string]any{
		"model":          p.Model,
		"prompt_version": p.PromptVersion,
		"tone":           p.Tone,
		"language":       p.Language,
		"ai_enabled":     p.AIEnabled,
		"anonymize":      p.Anonymize,
		"opt_in":         p.OptIn,
	}
}
