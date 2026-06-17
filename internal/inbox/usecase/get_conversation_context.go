package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/inbox"
)

// GetConversationContext is the read-side use case that gathers
// everything the inbox conversation side-panel needs in one call:
// contact display data, the conversation's channel, its current funnel
// stage, and the assignment state (SIN-64969, backend half of
// SIN-64959).
//
// It is hexagonal by construction: the reader interfaces below are
// declared IN this package (small, accept-broad / return-narrow) and
// satisfied structurally by the existing postgres adapters, so the
// use case never imports a storage driver and the web boundary can
// consume the projection without importing a domain root
// (forbidwebboundary).
//
// Every sub-read is filtered by TenantID and degrades gracefully: a
// missing contact (the conversation predates the contacts table) or a
// conversation that has never moved on the funnel resolves to
// zero-values for that field rather than failing the whole read — the
// panel must render partially. A nil optional reader (a deployment
// that has not wired contacts/funnel storage) is treated the same way.
type GetConversationContext struct {
	conversations conversationReader
	contacts      contactReader
	transitions   funnelTransitionReader
	stages        funnelStageReader
}

// conversationReader is the narrow read port for the conversation
// aggregate. Satisfied by inbox.Repository (the postgres inbox Store).
// This is the only required reader — without it there is no context.
type conversationReader interface {
	GetConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*inbox.Conversation, error)
}

// contactReader is the narrow read port for contact display data.
// Satisfied by contacts.Repository (`internal/contacts/port.go`
// FindByID).
type contactReader interface {
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*contacts.Contact, error)
}

// funnelTransitionReader yields the conversation's most-recent funnel
// transition. Satisfied by funnel.TransitionRepository
// (LatestForConversation).
type funnelTransitionReader interface {
	LatestForConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*funnel.Transition, error)
}

// funnelStageReader resolves a stage by id. The latest transition
// carries the destination stage id (to_stage_id), not its key, so the
// stage name lookup is by id. Satisfied by the postgres funnel Store's
// FindByID (added alongside this use case; not part of the
// funnel.StageRepository port).
type funnelStageReader interface {
	FindByID(ctx context.Context, tenantID, stageID uuid.UUID) (*funnel.Stage, error)
}

// ContactIdentityView is the read-only projection of a single contact
// channel identity (e.g. WhatsApp phone) for the side panel.
type ContactIdentityView struct {
	Channel    string
	ExternalID string
}

// ConversationContextView is the read-only projection backing the inbox
// conversation side panel. Fields left at their zero value mean "not
// available" — the template renders the present fields and omits the
// rest (graceful degradation).
type ConversationContextView struct {
	ConversationID uuid.UUID
	Channel        string

	// Contact display data. ContactDisplayName is empty and
	// ContactIdentities is nil when the contact row is missing.
	ContactID          uuid.UUID
	ContactDisplayName string
	ContactIdentities  []ContactIdentityView

	// Funnel stage. Both empty when the conversation has never moved
	// (no transition) or funnel storage is not wired.
	FunnelStageKey  string
	FunnelStageName string

	// Assignment. Assigned is true iff AssignedUserID is non-nil. No
	// user display-name read port exists (iam is auth-only), so only
	// the id is surfaced; name resolution is left for a follow-up.
	Assigned       bool
	AssignedUserID *uuid.UUID
}

// GetConversationContextInput is the use-case argument. TenantID scopes
// every sub-read; ConversationID selects the conversation.
type GetConversationContextInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// GetConversationContextResult wraps the projected view.
type GetConversationContextResult struct {
	Context ConversationContextView
}

// NewGetConversationContext wires the use case. conversations is
// required; contacts, transitions, and stages are optional (nil readers
// degrade to zero-valued fields) so deployments that have not wired
// contacts/funnel storage still render the channel + assignment panel.
// Returns an error when conversations is nil.
func NewGetConversationContext(
	conversations conversationReader,
	contacts contactReader,
	transitions funnelTransitionReader,
	stages funnelStageReader,
) (*GetConversationContext, error) {
	if conversations == nil {
		return nil, errors.New("inbox/usecase: conversations reader must not be nil")
	}
	return &GetConversationContext{
		conversations: conversations,
		contacts:      contacts,
		transitions:   transitions,
		stages:        stages,
	}, nil
}

// MustNewGetConversationContext is the panic-on-error variant for the
// composition root.
func MustNewGetConversationContext(
	conversations conversationReader,
	contacts contactReader,
	transitions funnelTransitionReader,
	stages funnelStageReader,
) *GetConversationContext {
	u, err := NewGetConversationContext(conversations, contacts, transitions, stages)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the read pipeline. ErrInvalidTenant when TenantID is
// nil; ErrNotFound when the conversation is missing under the tenant
// scope (the caller maps that to 404). Sub-reads (contact, funnel)
// degrade to zero-values on their domain ErrNotFound; any other sub-read
// error is propagated so a genuine storage failure is not masked.
func (u *GetConversationContext) Execute(ctx context.Context, in GetConversationContextInput) (GetConversationContextResult, error) {
	if in.TenantID == uuid.Nil {
		return GetConversationContextResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return GetConversationContextResult{}, inbox.ErrNotFound
	}

	conv, err := u.conversations.GetConversation(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return GetConversationContextResult{}, err
	}

	view := ConversationContextView{
		ConversationID: conv.ID,
		Channel:        conv.Channel,
		ContactID:      conv.ContactID,
		Assigned:       conv.AssignedUserID != nil,
		AssignedUserID: conv.AssignedUserID,
	}

	if err := u.fillContact(ctx, in.TenantID, conv.ContactID, &view); err != nil {
		return GetConversationContextResult{}, err
	}
	if err := u.fillFunnel(ctx, in.TenantID, in.ConversationID, &view); err != nil {
		return GetConversationContextResult{}, err
	}

	return GetConversationContextResult{Context: view}, nil
}

// fillContact resolves the contact display name + identities, degrading
// to zero-values on contacts.ErrNotFound or a nil reader.
func (u *GetConversationContext) fillContact(ctx context.Context, tenantID, contactID uuid.UUID, view *ConversationContextView) error {
	if u.contacts == nil {
		return nil
	}
	c, err := u.contacts.FindByID(ctx, tenantID, contactID)
	switch {
	case err == nil:
		view.ContactDisplayName = c.DisplayName
		ids := c.Identities()
		if len(ids) > 0 {
			view.ContactIdentities = make([]ContactIdentityView, 0, len(ids))
			for _, id := range ids {
				view.ContactIdentities = append(view.ContactIdentities, ContactIdentityView{
					Channel:    id.Channel,
					ExternalID: id.ExternalID,
				})
			}
		}
		return nil
	case errors.Is(err, contacts.ErrNotFound):
		return nil
	default:
		return err
	}
}

// fillFunnel resolves the current stage key + name from the latest
// transition's destination stage, degrading to zero-values when the
// conversation has never moved (funnel.ErrNotFound) or funnel storage
// is not wired (nil readers).
func (u *GetConversationContext) fillFunnel(ctx context.Context, tenantID, conversationID uuid.UUID, view *ConversationContextView) error {
	if u.transitions == nil {
		return nil
	}
	tr, err := u.transitions.LatestForConversation(ctx, tenantID, conversationID)
	switch {
	case err == nil:
		// fall through to stage resolution below
	case errors.Is(err, funnel.ErrNotFound):
		return nil
	default:
		return err
	}
	if u.stages == nil {
		return nil
	}
	st, err := u.stages.FindByID(ctx, tenantID, tr.ToStageID)
	switch {
	case err == nil:
		view.FunnelStageKey = st.Key
		view.FunnelStageName = st.Label
		return nil
	case errors.Is(err, funnel.ErrNotFound):
		return nil
	default:
		return err
	}
}
