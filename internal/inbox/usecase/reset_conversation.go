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

// AssignmentClearer is the narrow storage port that drops a
// conversation back to Unassigned (SIN-65472). Wiping a training
// thread's messages must also surrender its leader: a stale assignee
// chip on an empty conversation is misleading, and the cleared state
// has to be visible through the LIVE read path
// (Store.ListConversationSummaries reads conversation.assigned_user_id),
// not just the legacy listing. The Postgres *inbox.Store satisfies this
// via ClearConversationLead; NoopAssignmentClearer covers deployments
// that do not wire it.
type AssignmentClearer interface {
	ClearAssignment(ctx context.Context, tenantID, conversationID uuid.UUID) error
}

// NoopAssignmentClearer is the clearer wired when no assignment store is
// available. It satisfies AssignmentClearer with a no-op so the
// composition root and tests need not always inject a concrete adapter.
type NoopAssignmentClearer struct{}

// ClearAssignment satisfies AssignmentClearer; it does nothing.
func (NoopAssignmentClearer) ClearAssignment(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

// SummaryInvalidator is the narrow storage port that invalidates the
// AI summary cached for a conversation (SIN-65472). Deleting every
// message MUST invalidate the stored ai_summary rows: a summary
// describes a history that no longer exists, so no stale summary may
// survive a wipe. Suggestions are not separately persisted — they are
// regenerated from the summary — so invalidating the summary covers
// them too. The Postgres aiassist store satisfies this via
// InvalidateForConversation (sets invalidated_at); NoopSummaryInvalidator
// covers deployments without aiassist wired.
type SummaryInvalidator interface {
	InvalidateSummaries(ctx context.Context, tenantID, conversationID uuid.UUID) error
}

// NoopSummaryInvalidator is the invalidator wired when no summary store
// is available. It satisfies SummaryInvalidator with a no-op so the
// composition root and tests need not always inject a concrete adapter.
type NoopSummaryInvalidator struct{}

// InvalidateSummaries satisfies SummaryInvalidator; it does nothing.
func (NoopSummaryInvalidator) InvalidateSummaries(context.Context, uuid.UUID, uuid.UUID) error {
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
	repo       ResetRepository
	resetter   ConversationResetter
	assignment AssignmentClearer
	summaries  SummaryInvalidator
}

// ResetOption configures the optional collaborators of a
// ResetConversation. They are options (not positional constructor args)
// so the existing two-argument call sites — the wire and the unit
// tests — keep compiling while deployments that wire the assignment
// store and the aiassist summary store opt in explicitly.
type ResetOption func(*ResetConversation)

// WithAssignmentClearer wires the port that drops the conversation back
// to Unassigned after its messages are deleted (SIN-65472). A nil
// clearer is ignored, leaving the no-op default in place.
func WithAssignmentClearer(c AssignmentClearer) ResetOption {
	return func(u *ResetConversation) {
		if c != nil {
			u.assignment = c
		}
	}
}

// WithSummaryInvalidator wires the port that invalidates the cached AI
// summary after a wipe (SIN-65472). A nil invalidator is ignored,
// leaving the no-op default in place.
func WithSummaryInvalidator(s SummaryInvalidator) ResetOption {
	return func(u *ResetConversation) {
		if s != nil {
			u.summaries = s
		}
	}
}

// NewResetConversation wires the use case. A nil repo is a programming
// error caught here. A nil resetter is tolerated and replaced with the
// no-op resetter so callers in deployments without a stateful channel
// adapter need not construct one. The assignment clearer and summary
// invalidator default to no-ops (tolerating a deployment that wires
// neither aiassist nor the assignment store) and are opted in via
// WithAssignmentClearer / WithSummaryInvalidator.
func NewResetConversation(repo ResetRepository, resetter ConversationResetter, opts ...ResetOption) (*ResetConversation, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: reset repo must not be nil")
	}
	if resetter == nil {
		resetter = NoopConversationResetter{}
	}
	u := &ResetConversation{
		repo:       repo,
		resetter:   resetter,
		assignment: NoopAssignmentClearer{},
		summaries:  NoopSummaryInvalidator{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(u)
		}
	}
	return u, nil
}

// MustNewResetConversation is the panic-on-error variant for the
// composition root.
func MustNewResetConversation(repo ResetRepository, resetter ConversationResetter, opts ...ResetOption) *ResetConversation {
	u, err := NewResetConversation(repo, resetter, opts...)
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

// Execute runs the reset pipeline: load + guard, delete rows, clear the
// assignment, invalidate the cached summary, reset adapter state.
//
// Ordering rationale (SIN-65472): guard → delete rows → clear
// assignment → invalidate summary → resetter. Every step after the
// guard is idempotent, so on a partial failure the caller can retry and
// the steps converge (deleting again removes 0 rows, clearing an
// already-Unassigned conversation is a no-op write, invalidating an
// already-invalid summary is a no-op). We return on the first error
// rather than pressing on so a transient storage fault does not leave
// the operation reporting success with a stale assignee or summary
// still live; the retry finishes the remaining steps.
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

	// Drop the conversation back to Unassigned. A training thread whose
	// history was just wiped should not keep its assignee chip — the
	// cleared state is read back through the live ListConversationSummaries
	// path (conversation.assigned_user_id). Done AFTER the delete so a
	// failed delete never surrenders the leader.
	if err := u.assignment.ClearAssignment(ctx, in.TenantID, in.ConversationID); err != nil {
		return ResetConversationResult{}, err
	}

	// Invalidate the cached AI summary: it describes a history that no
	// longer exists. Cached suggestions are regenerated from the summary
	// and are not separately persisted, so this also voids them.
	if err := u.summaries.InvalidateSummaries(ctx, in.TenantID, in.ConversationID); err != nil {
		return ResetConversationResult{}, err
	}

	// Clear the channel adapter's in-memory state so the simulator starts
	// fresh. Done LAST: if any DB-side step fails we never touch adapter
	// state, keeping the two sides convergent on the next attempt.
	if err := u.resetter.ResetConversation(ctx, in.TenantID, in.ConversationID); err != nil {
		return ResetConversationResult{}, err
	}

	return ResetConversationResult{Deleted: deleted}, nil
}
