package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// TrainingChannel is the only conversation channel a reset may touch.
// It mirrors llmcustomer.ChannelName ("fakellm") but is duplicated here
// rather than imported: the use-case layer must not depend on a concrete
// channel adapter (hexagonal dependency direction), and the constant is
// the load-bearing blast-radius guard, so it lives next to the use case
// that enforces it. If the adapter's ChannelName ever changes, the
// guard test in reset_conversation_test.go fails loudly.
const TrainingChannel = "fakellm"

// ErrConversationNotResettable is returned by ResetConversation when the
// target conversation is not the fakellm training thread. It is the
// primary blast-radius control: a real customer conversation can never
// have its history deleted through this path. The web handler maps it to
// 404 (not 403) so the endpoint leaks no signal about which non-training
// conversations exist.
var ErrConversationNotResettable = errors.New("inbox: conversation is not resettable")

// ResetRepository is the narrow storage port ResetConversation needs:
// load the conversation (to read its channel for the guard) and delete
// its messages. Both *postgres/inbox.Store and the in-memory test repo
// satisfy it structurally — declaring the slice the use case actually
// uses (accept-narrow) keeps the dependency surface minimal.
type ResetRepository interface {
	GetConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*inbox.Conversation, error)
	DeleteMessagesByConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (int, error)
}

// ConversationResetter is the channel-adapter port that clears the
// in-memory conversational state a fake channel keeps alongside the DB
// (the llmcustomer adapter tracks per-tenant turn history + a
// "bootstrapped" flag under a mutex). Deleting message rows without
// resetting that state would desync the simulator — the next operator
// turn would replay the LLM against stale history. The llmcustomer
// adapter implements this; NoopConversationResetter covers every other
// channel (and deployments where the fake adapter is not wired).
type ConversationResetter interface {
	ResetConversation(ctx context.Context, tenantID, conversationID uuid.UUID) error
}

// NoopConversationResetter is the resetter wired when no channel keeps
// in-memory state to clear (the real-carrier wireup, or the disabled
// stub branch). It satisfies ConversationResetter with a no-op so the
// composition root never has to nil-guard the resetter.
type NoopConversationResetter struct{}

// ResetConversation satisfies ConversationResetter; it does nothing.
func (NoopConversationResetter) ResetConversation(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

// ResetConversation deletes every message of a fakellm training
// conversation and resets the channel adapter's in-memory state for it.
// It is the write side of SIN-65392 "apagar mensagens da conversa de
// treino".
//
// Security posture (least privilege + blast radius): the use case
// REJECTS — with ErrConversationNotResettable — any conversation whose
// channel is not the fakellm training channel, BEFORE deleting anything.
// Deleting customer history is therefore impossible by construction
// through this path, regardless of who calls it or what id they supply;
// the role gate on the route stays at the ordinary inbox-read level
// because the channel guard, not RBAC, is what confines the reach.
type ResetConversation struct {
	repo     ResetRepository
	resetter ConversationResetter
}

// NewResetConversation wires the use case. A nil repo is a programming
// error caught here. A nil resetter is tolerated and replaced with the
// no-op resetter so callers in deployments without a stateful channel
// adapter need not construct one.
func NewResetConversation(repo ResetRepository, resetter ConversationResetter) (*ResetConversation, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: reset repo must not be nil")
	}
	if resetter == nil {
		resetter = NoopConversationResetter{}
	}
	return &ResetConversation{repo: repo, resetter: resetter}, nil
}

// MustNewResetConversation is the panic-on-error variant for the
// composition root.
func MustNewResetConversation(repo ResetRepository, resetter ConversationResetter) *ResetConversation {
	u, err := NewResetConversation(repo, resetter)
	if err != nil {
		panic(err)
	}
	return u
}

// ResetConversationInput is the use-case argument.
type ResetConversationInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// ResetConversationResult reports the outcome. Deleted is the number of
// message rows removed (0 on an already-empty thread — the operation is
// idempotent).
type ResetConversationResult struct {
	Deleted int
}

// Execute runs the reset pipeline: load + guard, delete rows, reset
// adapter state.
func (u *ResetConversation) Execute(ctx context.Context, in ResetConversationInput) (ResetConversationResult, error) {
	if in.TenantID == uuid.Nil {
		return ResetConversationResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return ResetConversationResult{}, ErrNotFound
	}

	// Load first so the channel guard runs against the persisted truth,
	// not a client-supplied hint. An RLS-hidden / unknown id collapses to
	// ErrNotFound (IDOR guard) before any delete.
	conv, err := u.repo.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return ResetConversationResult{}, err
	}

	// Blast-radius guard: only the fakellm training thread is resettable.
	// Reject everything else as not-found so the endpoint cannot be used
	// to wipe — or even probe — real customer conversations.
	if conv.Channel != TrainingChannel {
		return ResetConversationResult{}, ErrConversationNotResettable
	}

	deleted, err := u.repo.DeleteMessagesByConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return ResetConversationResult{}, err
	}

	// Clear the channel adapter's in-memory state so the simulator starts
	// fresh. Done AFTER the DB delete: if the delete fails we never touch
	// adapter state, keeping the two sides convergent on the next attempt.
	if err := u.resetter.ResetConversation(ctx, in.TenantID, in.ConversationID); err != nil {
		return ResetConversationResult{}, err
	}

	return ResetConversationResult{Deleted: deleted}, nil
}
