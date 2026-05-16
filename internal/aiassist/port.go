package aiassist

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

// SummaryRepository is the persistence port for the Summary aggregate.
//
// All methods are tenant-scoped so the adapter can run inside
// WithTenant and let RLS gate the read/write.
//
// Implementations MUST translate:
//
//   - "no rows" on GetLatestValid → ErrCacheMiss
//   - "no rows" on InvalidateForConversation → no error (idempotent)
//   - unique violations (we have none in the current schema; a future
//     migration that adds them MUST surface as a typed sentinel here)
//
// so domain code can match with errors.Is instead of importing pgx.
type SummaryRepository interface {
	// GetLatestValid returns the most recent non-invalidated,
	// non-expired summary for (tenantID, conversationID), or
	// ErrCacheMiss when no row qualifies at the supplied wall-clock.
	// The "valid at now" semantics live on the adapter so the SQL
	// can leverage the partial index (invalidated_at IS NULL AND
	// expires_at > now()).
	GetLatestValid(
		ctx context.Context,
		tenantID, conversationID uuid.UUID,
		now time.Time,
	) (*Summary, error)

	// Save persists a freshly generated summary row. The adapter
	// inserts a new row (multiple historical rows per conversation
	// are expected — see the migration comment) rather than upsert,
	// so audit history is preserved.
	Save(ctx context.Context, s *Summary) error

	// InvalidateForConversation marks every currently-valid summary
	// for (tenantID, conversationID) as invalidated_at = now. The
	// inbox pipeline calls this when a new inbound message lands on
	// the conversation. Idempotent: invalidating an already-stale
	// conversation returns nil.
	InvalidateForConversation(
		ctx context.Context,
		tenantID, conversationID uuid.UUID,
		now time.Time,
	) error
}

// WalletClient is the subset of the wallet use-case service the
// aiassist orchestrator consumes. Declaring the interface here (rather
// than importing wallet/usecase.Service directly) lets the unit tests
// mock the wallet without spinning up Postgres and keeps the domain
// surface focused on the three calls the use case needs.
//
// *wallet/usecase.Service satisfies this interface by structural
// match. The interface accepts the concrete wallet.Reservation pointer
// so callers do not have to translate to a duplicate domain type.
type WalletClient interface {
	Reserve(
		ctx context.Context,
		tenantID uuid.UUID,
		amount int64,
		idempotencyKey string,
	) (*wallet.Reservation, error)

	Commit(
		ctx context.Context,
		r *wallet.Reservation,
		actualAmount int64,
		idempotencyKey string,
	) error

	Release(
		ctx context.Context,
		r *wallet.Reservation,
		idempotencyKey string,
	) error
}

// Scope identifies which policy row applies to the request. The
// aipolicy resolver (SIN-62351 W2A) walks channel → team → tenant and
// returns the most specific match. The aiassist use case carries the
// triple opaquely; it does not interpret scope_id beyond passing it
// to the resolver.
type Scope struct {
	TeamID    string
	ChannelID string
}

// Policy is the slice of the ai_policy row the use case actually
// needs. Defining a port-side type keeps aiassist decoupled from
// internal/aipolicy's struct layout — the resolver's adapter maps
// columns to fields, so future schema additions stay invisible to
// aiassist unless they affect the orchestration contract.
type Policy struct {
	// AIEnabled gates whether the use case may proceed at all. False
	// is a hard stop — no reservation, no LLM call.
	AIEnabled bool
	// OptIn gates whether the tenant has explicitly opted into IA
	// processing. ADR-0041 requires this in addition to AIEnabled so
	// a master-toggled feature flag cannot bypass tenant consent.
	OptIn bool
	// Anonymize tells the cmd/server wiring whether to apply the PII
	// anonymizer before forwarding the prompt to the LLM adapter.
	// aiassist reads but does not act on this field directly — the
	// anonymizer (SIN-62350 W3B) sits between the use case and the
	// LLM adapter; the use case just forwards the policy decision.
	Anonymize bool
	// Model is the effective model id forwarded to LLMClient.Complete.
	// Empty means "let the adapter choose its default".
	Model string
	// MaxOutputTokens caps the completion length. The use case uses
	// it to (a) compute the reservation upper bound and (b) populate
	// LLMRequest.MaxTokens. A non-positive value is treated as the
	// adapter default; the wallet reservation then covers only the
	// estimated prompt tokens, which is the documented "unlimited
	// output, capped budget" mode.
	MaxOutputTokens int64
}

// PolicyResolver is the port the use case calls to pick the effective
// IA policy for (tenant, scope). The aipolicy adapter (SIN-62351)
// implements this by cascading channel → team → tenant. The interface
// is read-only and side-effect-free.
type PolicyResolver interface {
	Resolve(ctx context.Context, tenantID uuid.UUID, scope Scope) (Policy, error)
}

// RateLimiter is the optional gate the use case calls before doing any
// expensive work. Implementations live in internal/ai/port (SIN-62238).
// Defence in depth: even though the wallet absorbs spend, a runaway
// bot can still flood the LLM upstream and burn rate-limit budget at
// OpenRouter. The use case treats a denied allow as a hard refusal
// (ErrLLMUnavailable wrapped) rather than blocking.
//
// We accept the interface as a function-shaped type here so the use
// case can be unit-tested without importing internal/ai/port from the
// domain (the port lives in a sibling domain package, so the import is
// allowed but unnecessary friction in the unit tests).
type RateLimiter interface {
	Allow(ctx context.Context, bucket, key string) (allowed bool, retryAfter time.Duration, err error)
}
