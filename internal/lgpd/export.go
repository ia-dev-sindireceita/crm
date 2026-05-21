package lgpd

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ExportContact is the contact row included in a data-subject export.
// Only fields the LGPD considers personal data are exported; internal
// surrogate keys (uuid PKs) are included to make the export
// referentially intact for the data subject's lawyer.
type ExportContact struct {
	ID          uuid.UUID `json:"id" csv:"contact_id"`
	TenantID    uuid.UUID `json:"tenant_id" csv:"tenant_id"`
	DisplayName string    `json:"display_name" csv:"display_name"`
	CreatedAt   time.Time `json:"created_at" csv:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" csv:"updated_at"`
}

// ExportIdentity is one row of contact_channel_identity scoped to the
// exported contact. Phone numbers / e-mails / handles count as personal
// data under LGPD art. 5 II — they are exported verbatim.
type ExportIdentity struct {
	ID         uuid.UUID `json:"id" csv:"identity_id"`
	Channel    string    `json:"channel" csv:"channel"`
	ExternalID string    `json:"external_id" csv:"external_id"`
	CreatedAt  time.Time `json:"created_at" csv:"created_at"`
}

// ExportConversation summarises one of the contact's conversation
// threads. The full message body lives in ExportMessage; this row
// gives the data subject the conversation envelope.
type ExportConversation struct {
	ID            uuid.UUID  `json:"id" csv:"conversation_id"`
	Channel       string     `json:"channel" csv:"channel"`
	State         string     `json:"state" csv:"state"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty" csv:"last_message_at"`
	CreatedAt     time.Time  `json:"created_at" csv:"created_at"`
}

// ExportMessage is one message turn from the contact's history.
// `Media` carries only the metadata jsonb stored on the message row —
// the binary asset itself is not embedded (AC #1 "anexos (metadata)").
type ExportMessage struct {
	ID                uuid.UUID `json:"id" csv:"message_id"`
	ConversationID    uuid.UUID `json:"conversation_id" csv:"conversation_id"`
	Direction         string    `json:"direction" csv:"direction"`
	Body              string    `json:"body" csv:"body"`
	Status            string    `json:"status" csv:"status"`
	ChannelExternalID *string   `json:"channel_external_id,omitempty" csv:"channel_external_id"`
	Media             *string   `json:"media,omitempty" csv:"media"`
	CreatedAt         time.Time `json:"created_at" csv:"created_at"`
}

// ExportBillingEvent is a billing-relevant audit_log_security row for
// the contact's tenant. The data subject does not get cross-tenant
// rows. `Target` is the raw jsonb payload (kept as a string so the
// CSV row can include it verbatim).
type ExportBillingEvent struct {
	ID         uuid.UUID `json:"id" csv:"event_id"`
	EventType  string    `json:"event_type" csv:"event_type"`
	Target     string    `json:"target" csv:"target"`
	OccurredAt time.Time `json:"occurred_at" csv:"occurred_at"`
}

// ExportConsent is one row of ai_policy_consent for the contact's
// tenant — proof of the tenant operator's documented consent to
// process the data subject's payload through the IA pipeline.
type ExportConsent struct {
	ID                uuid.UUID `json:"id" csv:"consent_id"`
	ScopeKind         string    `json:"scope_kind" csv:"scope_kind"`
	ScopeID           string    `json:"scope_id" csv:"scope_id"`
	AnonymizerVersion string    `json:"anonymizer_version" csv:"anonymizer_version"`
	PromptVersion     string    `json:"prompt_version" csv:"prompt_version"`
	AcceptedAt        time.Time `json:"accepted_at" csv:"accepted_at"`
}

// ExportBundle is the full per-contact export payload. The handler
// writes it to disk twice: once as data.json (canonical) and once as
// data.csv (one section per slice for spreadsheet-friendly review).
type ExportBundle struct {
	GeneratedAt   time.Time            `json:"generated_at"`
	Contact       ExportContact        `json:"contact"`
	Identities    []ExportIdentity     `json:"identities"`
	Conversations []ExportConversation `json:"conversations"`
	Messages      []ExportMessage      `json:"messages"`
	BillingEvents []ExportBillingEvent `json:"billing_events"`
	Consents      []ExportConsent      `json:"consents"`
}

// ExportRepository is the read port the export handler depends on.
// All methods MUST scope by tenant + contact id, never returning rows
// belonging to another tenant even if the caller mis-types contact_id.
// Implementations live in internal/adapter/db/postgres.
type ExportRepository interface {
	GetContact(ctx context.Context, tenantID, contactID uuid.UUID) (ExportContact, error)
	ListIdentities(ctx context.Context, tenantID, contactID uuid.UUID) ([]ExportIdentity, error)
	ListConversations(ctx context.Context, tenantID, contactID uuid.UUID) ([]ExportConversation, error)
	ListMessages(ctx context.Context, tenantID, contactID uuid.UUID) ([]ExportMessage, error)
	ListBillingEvents(ctx context.Context, tenantID, contactID uuid.UUID) ([]ExportBillingEvent, error)
	ListConsents(ctx context.Context, tenantID uuid.UUID) ([]ExportConsent, error)
}

// PurgeRepository is the write port the retention worker depends on.
// All methods MUST refuse to touch fiscal/billing tables — those are
// retained for RetentionPolicy.FiscalYears and only purged when the
// worker decides that the request's retention_until has elapsed.
type PurgeRepository interface {
	// PurgeContact anonymises the contact row, deletes
	// contact_channel_identity rows, and drops non-fiscal messages /
	// conversations. Fiscal/billing audit rows (audit_log_security)
	// are preserved. Returns an error if any constituent step fails
	// — callers MUST treat the deletion as not complete in that case.
	PurgeContact(ctx context.Context, tenantID, contactID uuid.UUID) error
}
