package wasession

import (
	"time"

	"github.com/google/uuid"
)

// EventKind discriminates the payload carried by an Event.
type EventKind string

const (
	// EventInbound carries an inbound message (Event.Inbound is set).
	EventInbound EventKind = "inbound"
	// EventStatus carries a session status change (Event.Status is set).
	EventStatus EventKind = "status"
	// EventQR carries a QR pairing code for the provisioning UI
	// (Event.QR is set).
	EventQR EventKind = "qr"
)

// Event is the single, tagged union the Manager fans out to its consumer.
// Exactly one of Inbound / Status / QR is non-nil, selected by Kind. A
// tagged struct (rather than an interface hierarchy) keeps the channel
// fan-out and the consumer switch simple, mirroring inbox.InboundEvent.
type Event struct {
	Kind     EventKind
	TenantID uuid.UUID
	Inbound  *InboundMessage
	Status   *StatusChange
	QR       *QRCode
}

// InboundMessage is a carrier-agnostic inbound message emitted by a session.
// A later phase (Fase 2) maps it into inbox.InboundEvent; Fase 1 only needs
// to surface it. ExternalID is the carrier message id used as the inbound
// dedup key. SenderE164 is the sender's phone in E.164 (no '+').
type InboundMessage struct {
	ExternalID string
	SenderE164 string
	SenderName string
	Body       string
	OccurredAt time.Time
	HasMedia   bool
	FromMe     bool
}

// StatusChange reports a session transitioning between lifecycle states.
type StatusChange struct {
	From   Status
	To     Status
	Reason string
}

// QRCode is a pairing code to render to the operator. Code is secret
// (Credential) so it cannot be logged; ExpiresAt is when WhatsApp will
// rotate it.
type QRCode struct {
	Code      Credential
	ExpiresAt time.Time
}

// newInboundEvent builds an EventInbound for tenantID.
func newInboundEvent(tenantID uuid.UUID, msg InboundMessage) Event {
	return Event{Kind: EventInbound, TenantID: tenantID, Inbound: &msg}
}

// newStatusEvent builds an EventStatus for tenantID.
func newStatusEvent(tenantID uuid.UUID, sc StatusChange) Event {
	return Event{Kind: EventStatus, TenantID: tenantID, Status: &sc}
}

// newQREvent builds an EventQR for tenantID.
func newQREvent(tenantID uuid.UUID, qr QRCode) Event {
	return Event{Kind: EventQR, TenantID: tenantID, QR: &qr}
}
