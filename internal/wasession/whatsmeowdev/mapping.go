package whatsmeowdev

import (
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/pericles-luz/crm/internal/wasession"
)

// mapConnEvent maps a whatsmeow connection-lifecycle event to a wasession
// status. ok is false for events that are not status changes (e.g. messages,
// receipts). LoggedOut / StreamReplaced / TemporaryBan / ClientOutdated all
// map to Banned because none of them recover by simply reconnecting — the
// operator must re-pair (ADR 0107 D3 lifecycle).
func mapConnEvent(evt any) (to wasession.Status, reason string, ok bool) {
	switch evt.(type) {
	case *events.Connected:
		return wasession.StatusConnected, "connected", true
	case *events.Disconnected:
		return wasession.StatusDisconnected, "disconnected", true
	case *events.LoggedOut:
		return wasession.StatusBanned, "logged out", true
	case *events.StreamReplaced:
		return wasession.StatusBanned, "stream replaced", true
	case *events.TemporaryBan:
		return wasession.StatusBanned, "temporary ban", true
	case *events.ClientOutdated:
		return wasession.StatusBanned, "client outdated", true
	default:
		return "", "", false
	}
}

// messageToInbound translates a whatsmeow inbound message event into the
// carrier-agnostic wasession.InboundMessage.
func messageToInbound(evt *events.Message) wasession.InboundMessage {
	body, hasText := textOf(evt.Message)
	return wasession.InboundMessage{
		ExternalID: evt.Info.ID,
		SenderE164: jidToE164(evt.Info.Sender),
		SenderName: evt.Info.PushName,
		Body:       body,
		OccurredAt: evt.Info.Timestamp,
		HasMedia:   !hasText && hasMedia(evt.Message),
		FromMe:     evt.Info.IsFromMe,
	}
}

// textOf extracts the plain-text body of a message. ok is false when the
// message carries no text (e.g. a media-only message).
func textOf(m *waE2E.Message) (string, bool) {
	if m == nil {
		return "", false
	}
	if c := m.GetConversation(); c != "" {
		return c, true
	}
	if e := m.GetExtendedTextMessage(); e != nil {
		if t := e.GetText(); t != "" {
			return t, true
		}
	}
	return "", false
}

// hasMedia reports whether the message carries a media attachment Fase 2 will
// need to mark for scanning.
func hasMedia(m *waE2E.Message) bool {
	if m == nil {
		return false
	}
	return m.GetImageMessage() != nil ||
		m.GetVideoMessage() != nil ||
		m.GetAudioMessage() != nil ||
		m.GetDocumentMessage() != nil ||
		m.GetStickerMessage() != nil
}

// jidToE164 returns the phone-number user part of a JID in E.164 form without
// the leading '+'. whatsmeow stores the user part as bare digits.
func jidToE164(jid types.JID) string { return jid.User }

// e164ToJID parses an E.164 phone number (with or without a leading '+') into
// a WhatsApp user JID. It rejects empty or non-numeric input so a malformed
// recipient never silently becomes a wrong JID.
func e164ToJID(e164 string) (types.JID, error) {
	digits := strings.TrimPrefix(strings.TrimSpace(e164), "+")
	if digits == "" {
		return types.JID{}, fmt.Errorf("whatsmeowdev: empty recipient phone")
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return types.JID{}, fmt.Errorf("whatsmeowdev: recipient phone is not numeric: %q", e164)
		}
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}
