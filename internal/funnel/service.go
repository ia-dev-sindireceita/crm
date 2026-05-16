package funnel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Service orchestrates funnel mutations. It depends only on the three
// ports declared in port.go; the concrete pgx adapter is wired in
// cmd/server.
type Service struct {
	stages      StageRepository
	transitions TransitionRepository
	publisher   EventPublisher
	now         func() time.Time
}

// Config bundles the dependencies required to construct a Service.
// All fields are required except Now, which defaults to time.Now in
// UTC when nil — tests inject a pinned clock to assert TransitionedAt.
type Config struct {
	Stages      StageRepository
	Transitions TransitionRepository
	Publisher   EventPublisher
	Now         func() time.Time
}

// NewService validates the configuration and returns a ready Service.
// Missing required dependencies produce a typed error so cmd/server
// fails fast on misconfiguration instead of nil-panicking on first call.
func NewService(cfg Config) (*Service, error) {
	if cfg.Stages == nil {
		return nil, errors.New("funnel: StageRepository is required")
	}
	if cfg.Transitions == nil {
		return nil, errors.New("funnel: TransitionRepository is required")
	}
	if cfg.Publisher == nil {
		return nil, errors.New("funnel: EventPublisher is required")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		stages:      cfg.Stages,
		transitions: cfg.Transitions,
		publisher:   cfg.Publisher,
		now:         now,
	}, nil
}

// MoveConversation moves the conversation to the stage identified by
// toStageKey under the given tenant scope. The four rules from
// SIN-62792:
//
//  1. Resolve toStage by (tenantID, toStageKey). Missing → ErrStageNotFound.
//  2. Resolve the current stage from the latest transition (nil = "no stage").
//  3. Idempotency: if currentStageID == toStage.ID, return nil without
//     creating a new transition row.
//  4. Otherwise persist a new Transition and publish a
//     funnel.conversation_moved event with the wire payload from
//     ConversationMovedEvent.
//
// Reason is free-form; trimming is left to callers so they can persist
// whatever the UI captured verbatim.
func (s *Service) MoveConversation(
	ctx context.Context,
	tenantID, conversationID uuid.UUID,
	toStageKey string,
	byUserID uuid.UUID,
	reason string,
) error {
	if tenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	if conversationID == uuid.Nil {
		return ErrInvalidConversation
	}
	if byUserID == uuid.Nil {
		return ErrInvalidActor
	}
	key := strings.TrimSpace(toStageKey)
	if key == "" {
		return ErrInvalidStageKey
	}

	toStage, err := s.stages.FindByKey(ctx, tenantID, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %q", ErrStageNotFound, key)
		}
		return fmt.Errorf("funnel: resolve stage: %w", err)
	}

	var fromStageID *uuid.UUID
	latest, err := s.transitions.LatestForConversation(ctx, tenantID, conversationID)
	switch {
	case err == nil:
		// Rule 3 — idempotency: same destination → no-op.
		if latest.ToStageID == toStage.ID {
			return nil
		}
		from := latest.ToStageID
		fromStageID = &from
	case errors.Is(err, ErrNotFound):
		// First entry into the funnel: from is nil.
	default:
		return fmt.Errorf("funnel: resolve current stage: %w", err)
	}

	t := &Transition{
		ID:                   uuid.New(),
		TenantID:             tenantID,
		ConversationID:       conversationID,
		FromStageID:          fromStageID,
		ToStageID:            toStage.ID,
		TransitionedByUserID: byUserID,
		TransitionedAt:       s.now(),
		Reason:               reason,
	}
	if err := s.transitions.Create(ctx, t); err != nil {
		return fmt.Errorf("funnel: persist transition: %w", err)
	}

	evt := ConversationMovedEvent{
		TenantID:             t.TenantID,
		ConversationID:       t.ConversationID,
		TransitionID:         t.ID,
		FromStageID:          t.FromStageID,
		ToStageID:            t.ToStageID,
		ToStageKey:           toStage.Key,
		TransitionedByUserID: t.TransitionedByUserID,
		TransitionedAt:       t.TransitionedAt,
		Reason:               t.Reason,
	}
	if err := s.publisher.Publish(ctx, EventNameConversationMoved, evt); err != nil {
		return fmt.Errorf("funnel: publish %s: %w", EventNameConversationMoved, err)
	}
	return nil
}
