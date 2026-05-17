// Package usecase orchestrates the aiassist domain. Summarize is the
// single entry point: it resolves policy, looks up the cache, reserves
// tokens, calls the LLM, commits or releases, and persists the new
// summary row.
//
// The package imports only sibling domain code (internal/aiassist,
// internal/wallet) and stdlib. No pgx, no net/http, no vendor SDK.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/wallet"
)

// Clock is the time source. Defaulted to time.Now; tests inject a
// frozen clock so TTL / reservation timestamps are deterministic.
type Clock func() time.Time

// Service is the aiassist orchestrator. Construction is via NewService;
// zero values are not usable (the nil checks in NewService surface
// missing wiring early).
type Service struct {
	repo        aiassist.SummaryRepository
	walletSvc   aiassist.WalletClient
	llm         aiassist.LLMClient
	policy      aiassist.PolicyResolver
	rateLimiter aiassist.RateLimiter
	clock       Clock
	ttl         time.Duration
	// rateLimitBucket names the configured bucket the rate limiter
	// charges against. Empty means "rate limiting disabled" — the use
	// case skips the Allow call. The bucket is wired by cmd/server so
	// the use case stays infra-agnostic.
	rateLimitBucket string
	// consent + anonymizer + anonymizerVersion implement the SIN-62928
	// gate (Fase 3 decisão #8). All three must be wired for the gate
	// to run; if any is nil, the use case skips the gate and proceeds
	// as in W2C (rate limiter pattern). Production wiring in
	// cmd/server is responsible for setting all three so the gate is
	// always armed outside of unit tests.
	consent           aiassist.ConsentService
	anonymizer        aiassist.Anonymizer
	anonymizerVersion string
}

// Config carries Service construction-time dependencies. The fields are
// all required except RateLimiter (and RateLimitBucket): callers that
// don't wire a rate limiter skip the gate. TTL <= 0 falls back to
// aiassist.DefaultSummaryTTL; Clock nil falls back to time.Now.
//
// Consent / Anonymizer / AnonymizerVersion implement the SIN-62928
// consent gate. All three must be set together for the gate to run;
// any nil/blank skips the gate (test-only convenience). Production
// wiring sets all three.
type Config struct {
	Repo              aiassist.SummaryRepository
	Wallet            aiassist.WalletClient
	LLM               aiassist.LLMClient
	Policy            aiassist.PolicyResolver
	RateLimiter       aiassist.RateLimiter
	RateLimitBucket   string
	TTL               time.Duration
	Clock             Clock
	Consent           aiassist.ConsentService
	Anonymizer        aiassist.Anonymizer
	AnonymizerVersion string
}

// NewService validates cfg and returns a wired Service. Missing required
// fields surface as a typed error so cmd/server fails to start rather
// than panicking on the first call.
func NewService(cfg Config) (*Service, error) {
	if cfg.Repo == nil {
		return nil, errors.New("aiassist/usecase: Repo is nil")
	}
	if cfg.Wallet == nil {
		return nil, errors.New("aiassist/usecase: Wallet is nil")
	}
	if cfg.LLM == nil {
		return nil, errors.New("aiassist/usecase: LLM is nil")
	}
	if cfg.Policy == nil {
		return nil, errors.New("aiassist/usecase: Policy is nil")
	}
	if cfg.RateLimiter != nil && cfg.RateLimitBucket == "" {
		return nil, errors.New("aiassist/usecase: RateLimitBucket required when RateLimiter is set")
	}
	if (cfg.Consent != nil || cfg.Anonymizer != nil || cfg.AnonymizerVersion != "") &&
		(cfg.Consent == nil || cfg.Anonymizer == nil || cfg.AnonymizerVersion == "") {
		return nil, errors.New("aiassist/usecase: Consent, Anonymizer, and AnonymizerVersion must be wired together")
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = aiassist.DefaultSummaryTTL
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		repo:              cfg.Repo,
		walletSvc:         cfg.Wallet,
		llm:               cfg.LLM,
		policy:            cfg.Policy,
		rateLimiter:       cfg.RateLimiter,
		rateLimitBucket:   cfg.RateLimitBucket,
		clock:             clock,
		ttl:               ttl,
		consent:           cfg.Consent,
		anonymizer:        cfg.Anonymizer,
		anonymizerVersion: cfg.AnonymizerVersion,
	}, nil
}

// SummarizeRequest is the boundary input to Summarize. The
// idempotency triple (tenant_id, conversation_id, request_id) is the
// per-call dedup key; replaying the same triple after success returns
// the cached summary without charging the wallet again.
type SummarizeRequest struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	RequestID      string
	// Prompt is the pre-anonymised text to summarise. The use case
	// charges the wallet for this text plus the policy's
	// MaxOutputTokens and forwards the same string to LLMClient.
	Prompt string
	Scope  aiassist.Scope
}

// SummarizeResponse is what callers receive on success. CacheHit is
// true when the use case satisfied the request from the AISummary
// table without an LLM round trip (and without a wallet debit).
type SummarizeResponse struct {
	Summary  *aiassist.Summary
	CacheHit bool
}

// reservationKeySuffix and friends keep the per-phase idempotency keys
// distinct on the ledger while sharing the boundary request_id. The
// suffixes are stable so a replay produces the same key.
const (
	reservationKeySuffix = ":reserve"
	commitKeySuffix      = ":commit"
	releaseKeySuffix     = ":release"
)

// Summarize is the AI-assist orchestrator. Steps (matching the
// SIN-62903 spec):
//
//  1. Validate the input.
//  2. Resolve policy; refuse if AI is disabled / opt-in is false.
//  3. Optionally consult the rate limiter; deny → ErrLLMUnavailable.
//  4. Look up the cache; valid hit → return without charging.
//  5. EstimateReservation → wallet.Reserve.
//     wallet.ErrInsufficientFunds → ErrInsufficientBalance.
//  6. LLMClient.Complete.
//     Error → wallet.Release → wrap as ErrLLMUnavailable.
//  7. Clamp actualTokens to the reservation, wallet.Commit,
//     persist Summary row.
//
// On any wallet error during Release/Commit, the use case surfaces
// the wallet error verbatim so ops can correlate ledger anomalies
// with the F37 reconciler.
func (s *Service) Summarize(ctx context.Context, req SummarizeRequest) (*SummarizeResponse, error) {
	if err := s.validate(req); err != nil {
		return nil, err
	}

	policy, err := s.policy.Resolve(ctx, req.TenantID, req.Scope)
	if err != nil {
		return nil, fmt.Errorf("aiassist/usecase: resolve policy: %w", err)
	}
	if !policy.AIEnabled || !policy.OptIn {
		return nil, ErrPolicyBlocked(policy)
	}

	if err := s.checkConsent(ctx, req, policy); err != nil {
		return nil, err
	}

	if err := s.checkRateLimit(ctx, req); err != nil {
		return nil, err
	}

	now := s.clock()

	cached, err := s.repo.GetLatestValid(ctx, req.TenantID, req.ConversationID, now)
	if err != nil && !errors.Is(err, aiassist.ErrCacheMiss) {
		return nil, fmt.Errorf("aiassist/usecase: cache lookup: %w", err)
	}
	if err == nil && cached.IsValid(now) {
		return &SummarizeResponse{Summary: cached, CacheHit: true}, nil
	}

	reservation, llmResp, err := s.reserveAndCall(ctx, req, policy)
	if err != nil {
		return nil, err
	}

	committed, err := s.commitAndPersist(ctx, req, policy, reservation, llmResp, now)
	if err != nil {
		return nil, err
	}
	return &SummarizeResponse{Summary: committed, CacheHit: false}, nil
}

// Invalidate marks every currently-valid summary on the conversation
// stale. The inbox pipeline calls this on new inbound messages so the
// next Summarize regenerates rather than serving a row that no longer
// reflects the conversation. Idempotent: calling on a conversation
// with no valid summaries returns nil.
func (s *Service) Invalidate(ctx context.Context, tenantID, conversationID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return aiassist.ErrZeroTenant
	}
	if conversationID == uuid.Nil {
		return aiassist.ErrZeroConversation
	}
	return s.repo.InvalidateForConversation(ctx, tenantID, conversationID, s.clock())
}

// validate enforces the boundary contract. The wallet has its own
// checks; we duplicate the tenant/empty-key checks here so the failure
// surfaces before any expensive call.
func (s *Service) validate(req SummarizeRequest) error {
	if req.TenantID == uuid.Nil {
		return aiassist.ErrZeroTenant
	}
	if req.ConversationID == uuid.Nil {
		return aiassist.ErrZeroConversation
	}
	if req.RequestID == "" {
		return aiassist.ErrEmptyRequestID
	}
	if req.Prompt == "" {
		return aiassist.ErrEmptyPrompt
	}
	return nil
}

// checkConsent runs the SIN-62928 consent gate. It executes before
// the rate limiter, cache lookup, wallet reservation, and LLM call so
// nothing is charged or fetched on behalf of an unconsented scope.
//
// The gate is fail-closed: an Anonymizer or ConsentService error
// aborts the call. A clean (consent==false) result is the explicit
// re-consent path: the function returns a *aiassist.ConsentRequired
// carrying the anonymized preview, so the web handler can render the
// confirmation modal and persist consent on accept.
//
// The gate is skipped when (a) consent/anonymizer/version are not all
// wired (test-only), or (b) the resolved policy has no PromptVersion —
// the latter is a tightly-scoped escape hatch for legacy tenants whose
// admin row pre-dates the consent migration; once they re-run the
// W4A config UI they receive a non-empty version and re-enter the
// gate automatically.
func (s *Service) checkConsent(ctx context.Context, req SummarizeRequest, policy aiassist.Policy) error {
	if s.consent == nil || s.anonymizer == nil || s.anonymizerVersion == "" {
		return nil
	}
	if policy.PromptVersion == "" {
		return nil
	}
	preview, err := s.anonymizer.Anonymize(ctx, req.Prompt)
	if err != nil {
		return fmt.Errorf("aiassist/usecase: anonymize for consent: %w", err)
	}
	scope := consentScopeFor(req.TenantID, req.Scope)
	has, err := s.consent.HasConsent(ctx, scope, s.anonymizerVersion, policy.PromptVersion)
	if err != nil {
		return fmt.Errorf("aiassist/usecase: consent check: %w", err)
	}
	if has {
		return nil
	}
	return &aiassist.ConsentRequired{
		Scope:             scope,
		Payload:           preview,
		AnonymizerVersion: s.anonymizerVersion,
		PromptVersion:     policy.PromptVersion,
	}
}

// consentScopeFor picks the most-specific consent scope for the
// request. The cascade mirrors the policy cascade (channel > team >
// tenant): a tenant operator acting on the WhatsApp channel records
// consent at the channel scope so other channels on the same tenant
// keep their own gate; a team-scoped action records at the team
// scope; a bare tenant action records at the tenant scope. The
// resulting triple maps directly onto migration 0101's UNIQUE
// (tenant_id, scope_kind, scope_id).
func consentScopeFor(tenantID uuid.UUID, scope aiassist.Scope) aipolicy.ConsentScope {
	switch {
	case scope.ChannelID != "":
		return aipolicy.ConsentScope{TenantID: tenantID, Kind: aipolicy.ScopeChannel, ID: scope.ChannelID}
	case scope.TeamID != "":
		return aipolicy.ConsentScope{TenantID: tenantID, Kind: aipolicy.ScopeTeam, ID: scope.TeamID}
	default:
		return aipolicy.ConsentScope{TenantID: tenantID, Kind: aipolicy.ScopeTenant, ID: tenantID.String()}
	}
}

// checkRateLimit calls the configured RateLimiter, if any. The key is
// (tenant_id, conversation_id) so the bucket is per-conversation —
// preventing a single noisy chat from monopolising the IA budget while
// leaving other conversations on the same tenant unaffected.
func (s *Service) checkRateLimit(ctx context.Context, req SummarizeRequest) error {
	if s.rateLimiter == nil {
		return nil
	}
	key := req.TenantID.String() + ":" + req.ConversationID.String()
	allowed, _, err := s.rateLimiter.Allow(ctx, s.rateLimitBucket, key)
	if err != nil {
		return fmt.Errorf("%w: %v", aiassist.ErrLLMUnavailable, err)
	}
	if !allowed {
		return aiassist.ErrLLMUnavailable
	}
	return nil
}

// idempotencyKey builds a per-phase ledger key from the boundary
// request_id. The triple (tenant, conversation, request_id) is the
// stable input; appending the phase suffix keeps Reserve / Commit /
// Release as distinct ledger rows while preserving idempotent retries
// on each phase.
func idempotencyKey(req SummarizeRequest, phaseSuffix string) string {
	return req.TenantID.String() + ":" + req.ConversationID.String() + ":" + req.RequestID + phaseSuffix
}

// reserveAndCall reserves tokens and runs the LLM. On any LLM failure
// it releases the reservation (defence in depth: the F37 reaper would
// catch a stranded reservation eventually, but releasing eagerly keeps
// the user-visible balance accurate). The returned reservation pointer
// is the surviving handle the caller hands to commitAndPersist.
func (s *Service) reserveAndCall(
	ctx context.Context,
	req SummarizeRequest,
	policy aiassist.Policy,
) (*wallet.Reservation, aiassist.LLMResponse, error) {
	estimate := aiassist.EstimateReservation(req.Prompt, policy.Model, policy.MaxOutputTokens)
	reserveKey := idempotencyKey(req, reservationKeySuffix)
	reservation, err := s.walletSvc.Reserve(ctx, req.TenantID, estimate, reserveKey)
	if err != nil {
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return nil, aiassist.LLMResponse{}, aiassist.ErrInsufficientBalance
		}
		return nil, aiassist.LLMResponse{}, fmt.Errorf("aiassist/usecase: wallet reserve: %w", err)
	}

	llmReq := aiassist.LLMRequest{
		Prompt:         req.Prompt,
		Model:          policy.Model,
		MaxTokens:      int(policy.MaxOutputTokens),
		IdempotencyKey: idempotencyKey(req, ""),
	}
	llmResp, err := s.llm.Complete(ctx, llmReq)
	if err != nil {
		releaseKey := idempotencyKey(req, releaseKeySuffix)
		if rerr := s.walletSvc.Release(ctx, reservation, releaseKey); rerr != nil {
			return nil, aiassist.LLMResponse{}, fmt.Errorf("aiassist/usecase: llm failed and release failed: %w (release: %v)", err, rerr)
		}
		return nil, aiassist.LLMResponse{}, fmt.Errorf("%w: %v", aiassist.ErrLLMUnavailable, err)
	}
	return reservation, llmResp, nil
}

// commitAndPersist clamps the actual token usage to the reservation
// ceiling, commits to the wallet, and persists the summary row. The
// clamp protects against a tokenizer-drift edge case (our /4 estimator
// vs. the upstream's BPE) and is paired with a defensive Save failure
// path that does not re-charge or refund the wallet — the F37 reconciler
// will pick up a "committed but no summary row" anomaly via the audit
// trail.
func (s *Service) commitAndPersist(
	ctx context.Context,
	req SummarizeRequest,
	policy aiassist.Policy,
	reservation *wallet.Reservation,
	llmResp aiassist.LLMResponse,
	now time.Time,
) (*aiassist.Summary, error) {
	actual := llmResp.TokensIn + llmResp.TokensOut
	if actual <= 0 {
		actual = 1
	}
	if actual > reservation.Amount {
		actual = reservation.Amount
	}

	commitKey := idempotencyKey(req, commitKeySuffix)
	if err := s.walletSvc.Commit(ctx, reservation, actual, commitKey); err != nil {
		return nil, fmt.Errorf("aiassist/usecase: wallet commit: %w", err)
	}

	summary, err := aiassist.NewSummary(
		req.TenantID,
		req.ConversationID,
		llmResp.Text,
		policy.Model,
		llmResp.TokensIn,
		llmResp.TokensOut,
		now,
		s.ttl,
	)
	if err != nil {
		return nil, fmt.Errorf("aiassist/usecase: build summary: %w", err)
	}
	if err := s.repo.Save(ctx, summary); err != nil {
		return nil, fmt.Errorf("aiassist/usecase: persist summary: %w", err)
	}
	return summary, nil
}

// ErrPolicyBlocked wraps ErrAIDisabled with the policy snapshot so
// callers / tests can distinguish "ai_enabled=false" from
// "opt_in=false" without leaking the boolean fields into the error
// message. The wrapped error keeps errors.Is(err, ErrAIDisabled) true.
func ErrPolicyBlocked(p aiassist.Policy) error {
	reason := "ai_enabled=false"
	if p.AIEnabled && !p.OptIn {
		reason = "opt_in=false"
	}
	return fmt.Errorf("%w: %s", aiassist.ErrAIDisabled, reason)
}
