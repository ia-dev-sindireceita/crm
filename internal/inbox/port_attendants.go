package inbox

import (
	"context"

	"github.com/google/uuid"
)

// AssignableAttendant is the read projection of a tenant user eligible to
// lead a conversation: the user id plus a human display label for the
// inbox "assign to…" dropdown. DisplayName is derived adapter-side from
// the user's email (the users table has no display-name column — see
// UserLabelFromEmail) so the UI never renders a bare UUID.
type AssignableAttendant struct {
	UserID      uuid.UUID
	DisplayName string
}

// AssignableAttendantRepository lists the tenant users that may receive a
// conversation and answers the point question "is this user assignable?".
//
// "Assignable" = a user under the tenant scope whose role is one of the
// inbox roles tenant_atendente or tenant_gerente (mirrors the
// iam.ActionTenantInboxRead matrix). The Postgres adapter MUST run both
// methods under WithTenant so RLS hides users from other tenants — the
// tenant-isolation guarantee the AssignConversation use case leans on for
// its deny-by-default role/tenant gate.
//
// The port is intentionally small (accept-broad / return-narrow): the
// dropdown needs ListAssignable, the assign use case needs only
// IsAssignable.
type AssignableAttendantRepository interface {
	// ListAssignable returns the tenant's assignable attendants ordered by
	// display label, for the inbox assignment dropdown. Returns an empty
	// slice (nil error) when the tenant has no eligible user.
	ListAssignable(ctx context.Context, tenantID uuid.UUID) ([]AssignableAttendant, error)

	// IsAssignable reports whether userID is an assignable attendant under
	// tenantID. A user from another tenant (RLS-hidden), an unknown id, or a
	// user whose role is not an inbox role all return false, nil — the
	// caller maps false to ErrUserNotAssignable. Returns a non-nil error
	// only on an infrastructure failure.
	IsAssignable(ctx context.Context, tenantID, userID uuid.UUID) (bool, error)
}

// ConversationLeadStore persists the denormalised current-lead column
// (conversation.assigned_user_id) that the inbox list read-model
// (ConversationReadModel.ListConversationSummaries) filters and projects
// on. The append-only assignment_history ledger remains the audit source
// of truth; this port keeps the conversation row's cached lead coherent
// with the latest ledger row so the list pane's assignee filter and badge
// reflect manual (re)assignments. The Postgres adapter MUST run under
// WithTenant so the UPDATE cannot touch another tenant's row.
type ConversationLeadStore interface {
	// SetConversationLead sets conversation.assigned_user_id to userID for
	// (tenantID, conversationID). Returns ErrNotFound when no conversation
	// matches the tenant scope (RLS-hidden rows from other tenants collapse
	// to the same sentinel, preserving the no-cross-tenant-existence
	// posture).
	SetConversationLead(ctx context.Context, tenantID, conversationID, userID uuid.UUID) error
}
