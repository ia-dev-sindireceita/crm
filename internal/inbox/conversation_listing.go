package inbox

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// knownChannels enumerates the carriers the inbox list filter accepts.
// It mirrors the channels the receive/send adapters integrate today; an
// empty filter means "all channels". Values are the canonical lower-case
// spelling NewConversation stores on the write side, so the read filter
// matches verbatim once the use case has normalised the input.
var knownChannels = map[string]struct{}{
	"whatsapp":  {},
	"instagram": {},
	"messenger": {},
	"webchat":   {},
}

// ValidateListChannel reports nil when channel is empty (= no filter) or
// a known carrier; otherwise ErrInvalidChannel. The value is matched
// verbatim against the canonical lower-case spelling: callers MUST trim
// and lower-case at the boundary (the use case does) so a casing variant
// never silently slips past as "no filter".
func ValidateListChannel(channel string) error {
	if channel == "" {
		return nil
	}
	if _, ok := knownChannels[channel]; !ok {
		return ErrInvalidChannel
	}
	return nil
}

// SnippetMaxChars caps the last-message preview the inbox list renders.
// 140 runes ≈ one short line; truncation happens server-side so the
// browser never receives a full (potentially multi-KB) message body in
// the list payload — only the preview the operator scans.
const SnippetMaxChars = 140

// Snippet collapses internal whitespace and truncates body to
// SnippetMaxChars runes, appending a single "…" when truncation
// occurred. It counts runes (not bytes) so multi-byte UTF-8 (Portuguese
// accents, emoji) is never split mid-codepoint, and it folds any run of
// whitespace — newlines included — to a single space so a multi-line
// message renders as one preview line.
func Snippet(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	body = strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(body) <= SnippetMaxChars {
		return body
	}
	runes := []rune(body)
	return strings.TrimRight(string(runes[:SnippetMaxChars]), " ") + "…"
}

// UserLabelFromEmail derives a human label from a user's email. The users
// table has no display-name column (migration 0005), so the local-part
// (before "@") is the best available label. Inputs without a usable
// local-part fall back to the trimmed email so the UI always has something
// to render. This is pure presentation logic, kept in the domain alongside
// Snippet/ValidateListChannel; the postgres UserDirectory adapter calls it
// after fetching the email (SIN-64967).
func UserLabelFromEmail(email string) string {
	email = strings.TrimSpace(email)
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// ConversationFilter narrows ListConversationSummaries on the read side.
// Every field is optional; a zero value means "no filter" on that axis:
//
//	State          — "" returns both open and closed
//	Channel        — "" returns all carriers (validated against knownChannels)
//	AssignedUserID — uuid.Nil returns conversations regardless of assignee;
//	                 a non-nil id implements the "atribuídas a mim" filter
type ConversationFilter struct {
	State          ConversationState
	Channel        string
	AssignedUserID uuid.UUID
}

// ConversationListItem is the flat read-model projection backing the
// GET /inbox list pane (SIN-64967). It is intentionally NOT the
// Conversation aggregate: the read side must not leak the aggregate (or
// its mutators) to the web layer (forbidwebboundary / DDD-lite). The
// storage adapter computes LastMessageSnippet / LastMessageDirection in
// the same query that lists the conversations (no N+1); the
// assigned-atendente label and the awaiting-reply flag are derived above
// the port, in the use case.
type ConversationListItem struct {
	ID             uuid.UUID
	ContactID      uuid.UUID
	Channel        string
	State          ConversationState
	AssignedUserID *uuid.UUID
	LastMessageAt  time.Time
	CreatedAt      time.Time
	// ContactDisplayName is the primary row label: the contact's name
	// resolved by the adapter in the main query (tenant-scoped JOIN on
	// contact). It is never empty — the adapter falls back to the
	// channel identifier and then the contact id so the UI never has to
	// render a bare UUID (SIN-64967, CTO ratification of SIN-64966).
	ContactDisplayName string
	// LastMessageSnippet is the truncated body of the most recent message,
	// already passed through Snippet by the adapter. Empty when the
	// conversation has no messages yet.
	LastMessageSnippet string
	// LastMessageDirection is the direction of the most recent message
	// ("in"/"out"); empty when the conversation has no messages yet.
	LastMessageDirection MessageDirection
}
