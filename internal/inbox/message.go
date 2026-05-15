package inbox

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// MessageDirection labels the direction of a Message relative to the
// tenant: "in" was received from the contact, "out" was sent by the
// tenant. The CHECK constraint on migration 0088 matches these exact
// strings — keeping the constants here keeps adapter wiring in lock
// step with the schema.
type MessageDirection string

// MessageDirectionIn is a message received from the contact (e.g. an
// inbound WhatsApp text).
const MessageDirectionIn MessageDirection = "in"

// MessageDirectionOut is a message the tenant sent to the contact
// (e.g. an outbound WhatsApp reply).
const MessageDirectionOut MessageDirection = "out"

// MessageStatus is the lifecycle state of a message. Transitions are
// monotonic: pending → sent → delivered → read, with failed as a
// terminal off-ramp from any non-terminal state. The CHECK constraint
// on migration 0088 lists the same five values.
type MessageStatus string

const (
	// MessageStatusPending is the initial state of an outbound message
	// after the use-case persists it but before the carrier ACKs.
	MessageStatusPending MessageStatus = "pending"
	// MessageStatusSent means the carrier accepted the message for
	// delivery. Outbound only.
	MessageStatusSent MessageStatus = "sent"
	// MessageStatusDelivered means the carrier reports the message
	// reached the recipient device. Outbound only.
	MessageStatusDelivered MessageStatus = "delivered"
	// MessageStatusRead means the recipient opened/read the message.
	// Outbound only — inbound messages are inherently "delivered" from
	// our point of view and skip past this.
	MessageStatusRead MessageStatus = "read"
	// MessageStatusFailed is a terminal state for outbound messages the
	// carrier could not deliver (invalid number, blocked, expired
	// session). Inbound messages MUST NOT enter this state; the
	// receiver should reject them at the boundary.
	MessageStatusFailed MessageStatus = "failed"
)

// statusRank orders MessageStatus values along the monotonic progress
// axis. Equal-rank transitions are no-ops; higher rank moves forward.
// failed is special: it is reachable from any non-terminal state but
// cannot be left.
var statusRank = map[MessageStatus]int{
	MessageStatusPending:   0,
	MessageStatusSent:      1,
	MessageStatusDelivered: 2,
	MessageStatusRead:      3,
}

// validInboundStatuses lists the statuses an inbound message is allowed
// to be created with. Inbound messages are observed events — we never
// emit a "pending" inbound. The carrier already delivered the message
// to us, so it is at least "delivered" from the tenant's point of view.
var validInboundStatuses = map[MessageStatus]bool{
	MessageStatusDelivered: true,
	MessageStatusRead:      true,
}

// validOutboundStatuses lists every status an outbound message may
// legitimately carry.
var validOutboundStatuses = map[MessageStatus]bool{
	MessageStatusPending:   true,
	MessageStatusSent:      true,
	MessageStatusDelivered: true,
	MessageStatusRead:      true,
	MessageStatusFailed:    true,
}

// Message is an entity inside the Conversation aggregate. Identity is
// the UUID; equality is by ID. The aggregate root (Conversation) is
// responsible for invariants that cross multiple messages (e.g.
// LastMessageAt); Message owns the per-message state machine via
// AdvanceStatus.
//
// Media is the optional attachment summary for messages that carry a
// file payload (image, document, audio). It is nil for text-only
// messages. The fields are populated by the inbox repository when the
// underlying `message.media` jsonb column is non-null; the projector at
// internal/inbox/usecase/views.go (messageToView) reads from here to
// build the read-only MessageMediaView the HTMX bubble template renders.
// See [SIN-62805] F2-05d for the UI hide-flag wiring.
type Message struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	ConversationID    uuid.UUID
	Direction         MessageDirection
	Body              string
	Status            MessageStatus
	ChannelExternalID string
	SentByUserID      *uuid.UUID
	CreatedAt         time.Time
	Media             *MessageMedia
}

// MessageMedia is the closed metadata block the inbox read path uses to
// render attachments. Stored in the `message.media` jsonb column
// (migration 0092). Adapters fill it by parsing the jsonb document;
// the domain code only reads it.
//
// Hash is the content-addressed identifier the static-origin route
// (`GET /t/{tenant}/m/{hash}`) consumes. Format is the closed Format
// enum value (e.g. "png", "pdf"). ScanStatus is one of "pending",
// "clean", "infected" — matches the enum in internal/media/scanner.Status.
type MessageMedia struct {
	Hash       string
	Format     string
	ScanStatus string
}

// NewMessageInput is the constructor argument for NewMessage. Required
// fields are validated in NewMessage; nil/empty fields trigger a
// sentinel error.
type NewMessageInput struct {
	TenantID          uuid.UUID
	ConversationID    uuid.UUID
	Direction         MessageDirection
	Body              string
	Status            MessageStatus
	ChannelExternalID string
	SentByUserID      *uuid.UUID
}

// NewMessage constructs a fresh Message ready to be persisted. ID and
// CreatedAt are populated via now(); callers cannot supply them, which
// keeps the constructor a single source of truth for both fields.
func NewMessage(in NewMessageInput) (*Message, error) {
	if in.TenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return nil, ErrInvalidContact
	}
	if in.Direction != MessageDirectionIn && in.Direction != MessageDirectionOut {
		return nil, ErrInvalidDirection
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return nil, ErrInvalidBody
	}
	status := in.Status
	if status == "" {
		if in.Direction == MessageDirectionOut {
			status = MessageStatusPending
		} else {
			status = MessageStatusDelivered
		}
	}
	if in.Direction == MessageDirectionIn && !validInboundStatuses[status] {
		return nil, ErrInvalidStatus
	}
	if in.Direction == MessageDirectionOut && !validOutboundStatuses[status] {
		return nil, ErrInvalidStatus
	}
	m := &Message{
		ID:                uuid.New(),
		TenantID:          in.TenantID,
		ConversationID:    in.ConversationID,
		Direction:         in.Direction,
		Body:              body,
		Status:            status,
		ChannelExternalID: strings.TrimSpace(in.ChannelExternalID),
		SentByUserID:      in.SentByUserID,
		CreatedAt:         now(),
	}
	return m, nil
}

// HydrateMessage rebuilds a Message from stored fields without running
// the constructor's invariants. Adapter code uses it to materialise
// rows. Domain code MUST use NewMessage. Media is left nil — adapters
// that project the `message.media` jsonb call AttachMedia separately so
// existing call sites keep their nine-argument shape.
func HydrateMessage(id, tenantID, conversationID uuid.UUID, direction MessageDirection,
	body string, status MessageStatus, channelExternalID string, sentByUserID *uuid.UUID,
	createdAt time.Time) *Message {
	return &Message{
		ID:                id,
		TenantID:          tenantID,
		ConversationID:    conversationID,
		Direction:         direction,
		Body:              body,
		Status:            status,
		ChannelExternalID: channelExternalID,
		SentByUserID:      sentByUserID,
		CreatedAt:         createdAt,
	}
}

// AttachMedia sets m.Media with the projected `message.media` document.
// hash/format/scanStatus may be empty when the underlying jsonb omits
// the field; the projector at internal/inbox/usecase/views.go re-applies
// the "Sem expor a key infectada" rule (Hash is dropped on infected /
// pending verdicts) so any sensitive fallback handling lives there.
//
// Passing scanStatus == "" leaves m.Media nil so text-only messages
// stay free of an empty media block. Returns m for fluent chaining.
func (m *Message) AttachMedia(hash, format, scanStatus string) *Message {
	if scanStatus == "" && hash == "" && format == "" {
		return m
	}
	m.Media = &MessageMedia{Hash: hash, Format: format, ScanStatus: scanStatus}
	return m
}

// AdvanceStatus moves the message forward in its lifecycle. The
// transitions are:
//
//	pending → sent → delivered → read
//	any non-failed → failed
//
// Equal-status transitions are no-ops and return nil. Backward
// transitions (e.g. read → delivered) return ErrStatusRegression so
// out-of-order carrier ACKs do not corrupt the stored state. Unknown
// next statuses return ErrInvalidStatus.
//
// The method is direction-aware. Inbound messages may not transition
// to a sent/pending/failed state — those statuses are only meaningful
// for outbound flows.
func (m *Message) AdvanceStatus(next MessageStatus) error {
	if next == "" {
		return ErrInvalidStatus
	}
	if next == MessageStatusFailed {
		if m.Direction == MessageDirectionIn {
			return ErrInvalidStatus
		}
		if m.Status == MessageStatusFailed {
			return nil
		}
		m.Status = MessageStatusFailed
		return nil
	}
	curRank, curOK := statusRank[m.Status]
	nextRank, nextOK := statusRank[next]
	if !nextOK {
		return ErrInvalidStatus
	}
	if m.Status == MessageStatusFailed {
		// Failed is terminal. Refuse to leave it via the monotonic
		// path; callers must explicitly create a new send.
		return ErrStatusRegression
	}
	if !curOK {
		return ErrInvalidStatus
	}
	if m.Direction == MessageDirectionIn && (next == MessageStatusPending || next == MessageStatusSent) {
		return ErrInvalidStatus
	}
	if nextRank < curRank {
		return ErrStatusRegression
	}
	m.Status = next
	return nil
}

// AttachChannelExternalID sets the carrier-assigned message id (e.g.
// Meta's wamid) on an outbound message. The constructor leaves the
// field empty because we only learn it after the carrier ACK. Calling
// this on an already-attached id with a different value returns
// ErrInvalidStatus to surface the bug; calling it with the same id is
// a no-op.
func (m *Message) AttachChannelExternalID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return ErrInvalidStatus
	}
	if m.ChannelExternalID != "" && m.ChannelExternalID != id {
		return ErrInvalidStatus
	}
	m.ChannelExternalID = id
	return nil
}
