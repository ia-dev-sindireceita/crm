package aiassist

import "errors"

// ErrInsufficientBalance is returned by the Summarize use case when the
// wallet does not have enough available tokens to satisfy the estimated
// reservation. Callers map this to the "insufficient credits" UI; the
// underlying wallet sentinel (wallet.ErrInsufficientFunds) is wrapped
// so callers can errors.Is(err, aiassist.ErrInsufficientBalance) without
// importing the wallet package.
var ErrInsufficientBalance = errors.New("aiassist: insufficient token balance")

// ErrAIDisabled is returned when the resolved policy has ai_enabled=false
// or opt_in=false for the requesting scope. The use case refuses to
// reserve tokens or hit the LLM before this check passes (LGPD posture,
// ADR-0041 / decisão #8).
var ErrAIDisabled = errors.New("aiassist: AI disabled by policy")

// ErrCacheMiss is returned by SummaryRepository.GetLatestValid when no
// non-invalidated, non-expired summary exists for (tenant, conversation).
// Adapters MUST translate "no rows" to this sentinel so the use case can
// match with errors.Is without importing pgx.
var ErrCacheMiss = errors.New("aiassist: cache miss")

// ErrZeroTenant is returned by the use case when uuid.Nil is passed as
// tenantID. Domain code never trusts uuid.Nil as a sentinel.
var ErrZeroTenant = errors.New("aiassist: tenant id must not be uuid.Nil")

// ErrZeroConversation is returned when uuid.Nil is passed as
// conversationID. The summary aggregate is rooted at a conversation, so
// uuid.Nil is meaningless and we reject it at the boundary.
var ErrZeroConversation = errors.New("aiassist: conversation id must not be uuid.Nil")

// ErrEmptyRequestID is returned when the caller omits the request_id
// component of the idempotency key. The wallet's ledger requires a
// non-empty idempotency key on every debit row; rejecting earlier keeps
// the failure mode actionable (we can name the missing field).
var ErrEmptyRequestID = errors.New("aiassist: request id must not be empty")

// ErrEmptyPrompt is returned when the caller omits the prompt input.
// We refuse to reserve tokens or hit the LLM with an empty prompt — it
// is always a programmer error to summarise nothing.
var ErrEmptyPrompt = errors.New("aiassist: prompt must not be empty")

// ErrLLMUnavailable wraps any error returned by the LLMClient port so
// callers can branch on a single sentinel ("AI temporarily unavailable")
// rather than reasoning about adapter-specific errors. The original
// error is preserved via errors.Unwrap.
var ErrLLMUnavailable = errors.New("aiassist: LLM unavailable")
