package inbox_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestNewMessage_Validates(t *testing.T) {
	tenant := uuid.New()
	conv := uuid.New()
	tests := []struct {
		name    string
		in      inbox.NewMessageInput
		wantErr error
	}{
		{"zero tenant", inbox.NewMessageInput{
			TenantID: uuid.Nil, ConversationID: conv,
			Direction: inbox.MessageDirectionIn, Body: "hi",
		}, inbox.ErrInvalidTenant},
		{"zero conversation", inbox.NewMessageInput{
			TenantID: tenant, ConversationID: uuid.Nil,
			Direction: inbox.MessageDirectionIn, Body: "hi",
		}, inbox.ErrInvalidContact},
		{"empty direction", inbox.NewMessageInput{
			TenantID: tenant, ConversationID: conv,
			Direction: "", Body: "hi",
		}, inbox.ErrInvalidDirection},
		{"bogus direction", inbox.NewMessageInput{
			TenantID: tenant, ConversationID: conv,
			Direction: "sideways", Body: "hi",
		}, inbox.ErrInvalidDirection},
		{"empty body", inbox.NewMessageInput{
			TenantID: tenant, ConversationID: conv,
			Direction: inbox.MessageDirectionIn, Body: "   ",
		}, inbox.ErrInvalidBody},
		{"inbound with pending status", inbox.NewMessageInput{
			TenantID: tenant, ConversationID: conv,
			Direction: inbox.MessageDirectionIn, Body: "hi", Status: inbox.MessageStatusPending,
		}, inbox.ErrInvalidStatus},
		{"outbound with bogus status", inbox.NewMessageInput{
			TenantID: tenant, ConversationID: conv,
			Direction: inbox.MessageDirectionOut, Body: "hi", Status: "warped",
		}, inbox.ErrInvalidStatus},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inbox.NewMessage(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewMessage_DefaultStatus(t *testing.T) {
	tenant := uuid.New()
	conv := uuid.New()
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: conv,
		Direction: inbox.MessageDirectionIn, Body: "hi",
	})
	if err != nil {
		t.Fatalf("inbound NewMessage: %v", err)
	}
	if m.Status != inbox.MessageStatusDelivered {
		t.Errorf("inbound default Status = %q, want delivered", m.Status)
	}
	m2, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID: tenant, ConversationID: conv,
		Direction: inbox.MessageDirectionOut, Body: "hi",
	})
	if err != nil {
		t.Fatalf("outbound NewMessage: %v", err)
	}
	if m2.Status != inbox.MessageStatusPending {
		t.Errorf("outbound default Status = %q, want pending", m2.Status)
	}
}

func TestNewMessage_TrimsBodyAndExternalID(t *testing.T) {
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:          uuid.New(),
		ConversationID:    uuid.New(),
		Direction:         inbox.MessageDirectionOut,
		Body:              "  hello  ",
		ChannelExternalID: "  wamid.abc  ",
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if m.Body != "hello" {
		t.Errorf("Body = %q, want hello", m.Body)
	}
	if m.ChannelExternalID != "wamid.abc" {
		t.Errorf("ChannelExternalID = %q, want wamid.abc", m.ChannelExternalID)
	}
	if m.ID == uuid.Nil {
		t.Error("ID = uuid.Nil, want generated")
	}
	if m.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want now()")
	}
}

func TestNewMessage_PreservesSentByUser(t *testing.T) {
	uid := uuid.New()
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Direction:      inbox.MessageDirectionOut,
		Body:           "hi",
		SentByUserID:   &uid,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if m.SentByUserID == nil || *m.SentByUserID != uid {
		t.Errorf("SentByUserID = %v, want %v", m.SentByUserID, uid)
	}
}

// TestMessage_AdvanceStatus_AllTransitions covers AC #2: the state
// machine accepts every valid transition and rejects every invalid one.
func TestMessage_AdvanceStatus_AllTransitions(t *testing.T) {
	tests := []struct {
		name      string
		direction inbox.MessageDirection
		start     inbox.MessageStatus
		next      inbox.MessageStatus
		wantErr   error
		wantState inbox.MessageStatus
	}{
		// Outbound monotonic happy path.
		{"out: pending → sent", inbox.MessageDirectionOut, inbox.MessageStatusPending, inbox.MessageStatusSent, nil, inbox.MessageStatusSent},
		{"out: sent → delivered", inbox.MessageDirectionOut, inbox.MessageStatusSent, inbox.MessageStatusDelivered, nil, inbox.MessageStatusDelivered},
		{"out: delivered → read", inbox.MessageDirectionOut, inbox.MessageStatusDelivered, inbox.MessageStatusRead, nil, inbox.MessageStatusRead},
		// Equal-rank transitions are no-ops.
		{"out: sent → sent", inbox.MessageDirectionOut, inbox.MessageStatusSent, inbox.MessageStatusSent, nil, inbox.MessageStatusSent},
		// Skipping is allowed (carrier may collapse 'sent' and only emit 'delivered').
		{"out: pending → delivered", inbox.MessageDirectionOut, inbox.MessageStatusPending, inbox.MessageStatusDelivered, nil, inbox.MessageStatusDelivered},
		{"out: pending → read", inbox.MessageDirectionOut, inbox.MessageStatusPending, inbox.MessageStatusRead, nil, inbox.MessageStatusRead},
		// Failed is reachable from any non-failed outbound state.
		{"out: pending → failed", inbox.MessageDirectionOut, inbox.MessageStatusPending, inbox.MessageStatusFailed, nil, inbox.MessageStatusFailed},
		{"out: sent → failed", inbox.MessageDirectionOut, inbox.MessageStatusSent, inbox.MessageStatusFailed, nil, inbox.MessageStatusFailed},
		// Regressions rejected.
		{"out: delivered → sent", inbox.MessageDirectionOut, inbox.MessageStatusDelivered, inbox.MessageStatusSent, inbox.ErrStatusRegression, inbox.MessageStatusDelivered},
		{"out: read → delivered", inbox.MessageDirectionOut, inbox.MessageStatusRead, inbox.MessageStatusDelivered, inbox.ErrStatusRegression, inbox.MessageStatusRead},
		{"out: sent → pending", inbox.MessageDirectionOut, inbox.MessageStatusSent, inbox.MessageStatusPending, inbox.ErrStatusRegression, inbox.MessageStatusSent},
		// Failed is terminal.
		{"out: failed → delivered", inbox.MessageDirectionOut, inbox.MessageStatusFailed, inbox.MessageStatusDelivered, inbox.ErrStatusRegression, inbox.MessageStatusFailed},
		// Empty / unknown next rejected.
		{"out: pending → ''", inbox.MessageDirectionOut, inbox.MessageStatusPending, "", inbox.ErrInvalidStatus, inbox.MessageStatusPending},
		{"out: pending → bogus", inbox.MessageDirectionOut, inbox.MessageStatusPending, "warped", inbox.ErrInvalidStatus, inbox.MessageStatusPending},
		// Inbound state machine.
		{"in: delivered → read", inbox.MessageDirectionIn, inbox.MessageStatusDelivered, inbox.MessageStatusRead, nil, inbox.MessageStatusRead},
		{"in: delivered → pending", inbox.MessageDirectionIn, inbox.MessageStatusDelivered, inbox.MessageStatusPending, inbox.ErrInvalidStatus, inbox.MessageStatusDelivered},
		{"in: delivered → sent", inbox.MessageDirectionIn, inbox.MessageStatusDelivered, inbox.MessageStatusSent, inbox.ErrInvalidStatus, inbox.MessageStatusDelivered},
		{"in: delivered → failed", inbox.MessageDirectionIn, inbox.MessageStatusDelivered, inbox.MessageStatusFailed, inbox.ErrInvalidStatus, inbox.MessageStatusDelivered},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := inbox.HydrateMessage(
				uuid.New(), uuid.New(), uuid.New(),
				tc.direction, "hi", tc.start, "", nil,
				time.Now().UTC(),
			)
			err := m.AdvanceStatus(tc.next)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AdvanceStatus(%q) err = %v, want %v", tc.next, err, tc.wantErr)
			}
			if m.Status != tc.wantState {
				t.Errorf("Status = %q, want %q", m.Status, tc.wantState)
			}
		})
	}
}

func TestMessage_AdvanceStatus_FailedIdempotent(t *testing.T) {
	m := inbox.HydrateMessage(uuid.New(), uuid.New(), uuid.New(),
		inbox.MessageDirectionOut, "hi", inbox.MessageStatusFailed, "", nil, time.Now().UTC())
	if err := m.AdvanceStatus(inbox.MessageStatusFailed); err != nil {
		t.Errorf("failed → failed err = %v, want nil", err)
	}
	if m.Status != inbox.MessageStatusFailed {
		t.Errorf("Status = %q, want failed", m.Status)
	}
}

func TestMessage_AttachChannelExternalID(t *testing.T) {
	m := inbox.HydrateMessage(uuid.New(), uuid.New(), uuid.New(),
		inbox.MessageDirectionOut, "hi", inbox.MessageStatusPending, "", nil, time.Now().UTC())
	if err := m.AttachChannelExternalID(" wamid.abc "); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if m.ChannelExternalID != "wamid.abc" {
		t.Errorf("ChannelExternalID = %q, want wamid.abc", m.ChannelExternalID)
	}
	// Re-attaching the same id is a no-op.
	if err := m.AttachChannelExternalID("wamid.abc"); err != nil {
		t.Errorf("re-attach same err = %v, want nil", err)
	}
	// Attaching empty is rejected.
	if err := m.AttachChannelExternalID(""); err == nil {
		t.Error("attach empty err = nil, want error")
	}
	// Attaching a different id is rejected.
	if err := m.AttachChannelExternalID("wamid.xyz"); err == nil {
		t.Error("attach different err = nil, want error")
	}
}

func TestHydrateMessage_Roundtrip(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	conv := uuid.New()
	uid := uuid.New()
	created := time.Now().UTC().Truncate(time.Second)
	m := inbox.HydrateMessage(id, tenant, conv, inbox.MessageDirectionOut,
		"hi", inbox.MessageStatusSent, "wamid.abc", &uid, created)
	if m.ID != id || m.TenantID != tenant || m.ConversationID != conv ||
		m.Direction != inbox.MessageDirectionOut || m.Body != "hi" ||
		m.Status != inbox.MessageStatusSent || m.ChannelExternalID != "wamid.abc" ||
		m.SentByUserID == nil || *m.SentByUserID != uid || !m.CreatedAt.Equal(created) {
		t.Errorf("Hydrate mismatch: %+v", m)
	}
}
