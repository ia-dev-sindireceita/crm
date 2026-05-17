package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/rules"
)

// Config bundles the dependencies [NewEngine] requires. Every field
// except Logger, Now, and Metrics is mandatory; passing a nil value
// produces [ErrInvalidConfig] so the composition root fails fast at
// boot.
type Config struct {
	Resolver     RuleResolver
	Applications ApplicationsRepo
	Mover        StageMover

	// Logger captures structured per-event lines. Nil falls back to
	// slog.Default().
	Logger *slog.Logger

	// Now is the clock the engine reads when stamping the
	// [Application.AppliedAt] column and computing the latency
	// metric's elapsed-seconds. Nil falls back to time.Now().UTC().
	Now func() time.Time

	// Metrics is the Prometheus bundle. Nil disables metric emission
	// (the engine still runs); useful in unit tests where metrics are
	// out of scope.
	Metrics *Metrics
}

// Engine is the use-case core: resolve effective rules for an inbound
// message, evaluate triggers, dispatch the action via [StageMover],
// and record the application row for idempotency. The engine holds no
// mutable state across calls beyond its injected ports.
type Engine struct {
	resolver     RuleResolver
	applications ApplicationsRepo
	mover        StageMover
	logger       *slog.Logger
	now          func() time.Time
	metrics      *Metrics
}

// NewEngine validates the config and returns a ready Engine.
func NewEngine(cfg Config) (*Engine, error) {
	if cfg.Resolver == nil {
		return nil, fmt.Errorf("%w: Resolver is required", ErrInvalidConfig)
	}
	if cfg.Applications == nil {
		return nil, fmt.Errorf("%w: Applications is required", ErrInvalidConfig)
	}
	if cfg.Mover == nil {
		return nil, fmt.Errorf("%w: Mover is required", ErrInvalidConfig)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Engine{
		resolver:     cfg.Resolver,
		applications: cfg.Applications,
		mover:        cfg.Mover,
		logger:       logger,
		now:          now,
		metrics:      cfg.Metrics,
	}, nil
}

// Handle runs the full pipeline for one decoded inbound message:
//
//  1. Increment the per-tenant/channel evaluated counter.
//  2. Resolve the effective rule set via [RuleResolver]. team_id is
//     passed as uuid.Nil because the inbox aggregate does not yet carry
//     team assignment; channel + tenant scopes are the active reach
//     until conversations grow a team_id column.
//  3. For each resolved rule, evaluate the trigger. Non-matching rules
//     are skipped silently — they show up as the gap between the
//     evaluated and matched metrics.
//  4. For each match, short-circuit through [ApplicationsRepo.IsApplied]
//     so a redelivered NATS message skips the action entirely.
//  5. Dispatch the action via [StageMover.MoveConversation]. A failure
//     here propagates so JetStream redelivers; the action is idempotent
//     (funnel.Service.MoveConversation no-ops when already on stage)
//     so retries are safe.
//  6. Record the application row. UNIQUE conflict ([ErrAlreadyApplied])
//     is treated as a no-op success.
//
// The function never panics; every error path returns a wrapped error
// so the worker handler decides whether to Ack or Nak.
func (e *Engine) Handle(ctx context.Context, msg InboundMessage) error {
	start := e.now()
	defer func() {
		e.metrics.observeLatency(time.Since(start).Seconds())
	}()
	if err := e.validate(msg); err != nil {
		return err
	}
	e.metrics.observeEvaluated(msg.TenantID, msg.Channel)

	resolved, err := e.resolver.Resolve(ctx, rules.ResolveInput{
		TenantID: msg.TenantID,
		Channel:  msg.Channel,
		TeamID:   uuid.Nil,
	})
	if err != nil {
		return fmt.Errorf("engine: resolve rules: %w", err)
	}
	if len(resolved) == 0 {
		return nil
	}

	for _, r := range resolved {
		if err := e.tryRule(ctx, msg, r.Rule); err != nil {
			return err
		}
	}
	return nil
}

// tryRule evaluates one rule against the message and, on a match, runs
// the idempotency check + action dispatch + record. Errors short-circuit
// the rest of the rule loop because they are either transient (DB
// hiccup) or actionable (missing stage), and the next delivery will
// retry.
func (e *Engine) tryRule(ctx context.Context, msg InboundMessage, rule rules.Rule) error {
	if !matchTrigger(rule, msg) {
		return nil
	}
	e.metrics.observeMatched(rule.ID)

	applied, err := e.applications.IsApplied(ctx, msg.TenantID, rule.ID, msg.MessageID)
	if err != nil {
		return fmt.Errorf("engine: is-applied check: %w", err)
	}
	if applied {
		e.logger.InfoContext(ctx, "funnel/engine: skipped — already applied",
			"tenant_id", msg.TenantID,
			"rule_id", rule.ID,
			"message_id", msg.MessageID,
		)
		return nil
	}

	stage, ok := stageKey(rule)
	if !ok {
		// Action config is malformed — log and skip. We do NOT record
		// an application row because the action never dispatched; if
		// the operator fixes the rule, the next inbound on this
		// message_id would still skip (UNIQUE), but messages following
		// the fix start firing correctly.
		e.logger.WarnContext(ctx, "funnel/engine: rule action_config missing stage_key",
			"tenant_id", msg.TenantID,
			"rule_id", rule.ID,
			"action_type", rule.ActionType,
		)
		return nil
	}

	if err := e.mover.MoveConversation(ctx, msg.TenantID, msg.ConversationID, stage, SystemActor(), MoveReason); err != nil {
		return fmt.Errorf("engine: move conversation: %w", err)
	}

	record := Application{
		TenantID:       msg.TenantID,
		RuleID:         rule.ID,
		MessageID:      msg.MessageID,
		ConversationID: msg.ConversationID,
		ActionType:     string(rule.ActionType),
		AppliedAt:      e.now(),
	}
	if err := e.applications.Record(ctx, record); err != nil {
		if errors.Is(err, ErrAlreadyApplied) {
			// A concurrent delivery raced us and won — action still
			// applied at least once (we just did it again, idempotent).
			return nil
		}
		return fmt.Errorf("engine: record application: %w", err)
	}

	e.metrics.observeApplied(string(rule.ActionType))
	e.logger.InfoContext(ctx, "funnel/engine: applied",
		"tenant_id", msg.TenantID,
		"rule_id", rule.ID,
		"message_id", msg.MessageID,
		"conversation_id", msg.ConversationID,
		"action_type", rule.ActionType,
		"stage_key", stage,
	)
	return nil
}

// validate is a belt-and-braces check on the decoded message — the
// [DecodeInboundMessage] path already rejects bad payloads, but a
// caller that builds InboundMessage by hand (tests, future direct
// invokers) gets the same protection.
func (e *Engine) validate(msg InboundMessage) error {
	if msg.TenantID == uuid.Nil {
		return ErrInvalidEvent
	}
	if msg.ConversationID == uuid.Nil {
		return ErrInvalidEvent
	}
	if msg.MessageID == uuid.Nil {
		return ErrInvalidEvent
	}
	if msg.Channel == "" {
		return ErrInvalidEvent
	}
	return nil
}
