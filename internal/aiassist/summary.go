package aiassist

import (
	"time"

	"github.com/google/uuid"
)

// DefaultSummaryTTL is the default cache lifetime applied to a freshly
// generated AISummary. 24h matches the AC in SIN-62903: a summary stays
// usable for a working day so the operator does not pay tokens for the
// same conversation twice within a shift. Explicit invalidation (new
// inbound message — see Invalidate) trumps TTL.
const DefaultSummaryTTL = 24 * time.Hour

// Summary is the cached LLM-generated synopsis of a conversation.
//
// The aggregate is identified by (tenant_id, conversation_id); a single
// conversation can have multiple historical Summary rows (one per
// generation), but at most one is valid at a time — the others have
// either expired (expires_at <= now) or been explicitly invalidated
// (invalidated_at IS NOT NULL).
//
// Fields are exported so the adapter can hydrate the struct from a row
// scan without reflection. The domain helpers (IsValid, Invalidate)
// enforce the lifecycle.
type Summary struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	Text           string
	Model          string
	TokensIn       int64
	TokensOut      int64
	GeneratedAt    time.Time
	// ExpiresAt is the TTL boundary. A zero value means "no TTL" (an
	// opt-in for tenants that explicitly disable expiry). When non-zero,
	// the resolver treats GeneratedAt + TTL semantics consistently: the
	// row is valid up to and including ExpiresAt; once now > ExpiresAt,
	// the cache misses and a regeneration runs.
	ExpiresAt time.Time
	// InvalidatedAt is the explicit-invalidation marker. The inbox
	// pipeline calls SummaryService.Invalidate when a new message lands
	// on the conversation, which records the wall-clock at which the
	// summary stopped being authoritative. A non-zero value renders the
	// row invalid regardless of TTL.
	InvalidatedAt time.Time
}

// NewSummary constructs a Summary with TTL = DefaultSummaryTTL relative
// to generatedAt. ttl <= 0 disables expiration (ExpiresAt stays zero).
//
// Validation:
//   - tenantID != uuid.Nil
//   - conversationID != uuid.Nil
//   - text != ""
//   - tokensIn >= 0, tokensOut >= 0
//   - model != ""
//
// On any violation the function returns nil + a typed error so the use
// case can fail closed before persisting the row.
func NewSummary(
	tenantID, conversationID uuid.UUID,
	text, model string,
	tokensIn, tokensOut int64,
	generatedAt time.Time,
	ttl time.Duration,
) (*Summary, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if conversationID == uuid.Nil {
		return nil, ErrZeroConversation
	}
	if text == "" {
		return nil, ErrEmptyPrompt
	}
	if model == "" {
		return nil, ErrEmptyPrompt
	}
	if tokensIn < 0 || tokensOut < 0 {
		return nil, ErrEmptyPrompt
	}
	s := &Summary{
		ID:             uuid.New(),
		TenantID:       tenantID,
		ConversationID: conversationID,
		Text:           text,
		Model:          model,
		TokensIn:       tokensIn,
		TokensOut:      tokensOut,
		GeneratedAt:    generatedAt,
	}
	if ttl > 0 {
		s.ExpiresAt = generatedAt.Add(ttl)
	}
	return s, nil
}

// IsValid reports whether the summary is still the authoritative cache
// entry at the supplied wall-clock instant. The check is the OR of:
//
//   - explicit invalidation (InvalidatedAt non-zero), and
//   - TTL expiry (ExpiresAt non-zero AND now > ExpiresAt).
//
// Either failure mode flips the row to "miss"; callers regenerate by
// calling the LLM again. A summary with no TTL (ExpiresAt zero) and no
// explicit invalidation is always valid — that is the contract for
// tenants that opt out of expiration.
func (s *Summary) IsValid(now time.Time) bool {
	if s == nil {
		return false
	}
	if !s.InvalidatedAt.IsZero() {
		return false
	}
	if !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt) {
		return false
	}
	return true
}

// Invalidate records that the summary is no longer authoritative. The
// inbox pipeline calls this when a new inbound message lands on the
// conversation; the next Summarize call will miss the cache and
// regenerate. Calling Invalidate twice is a no-op: the first invalidation
// wins so the audit trail reflects when the staleness started.
func (s *Summary) Invalidate(now time.Time) {
	if s == nil {
		return
	}
	if !s.InvalidatedAt.IsZero() {
		return
	}
	s.InvalidatedAt = now
}
