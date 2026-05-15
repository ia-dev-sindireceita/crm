package inbox_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestNewConversation_Validates(t *testing.T) {
	tenant := uuid.New()
	contact := uuid.New()

	tests := []struct {
		name    string
		tenant  uuid.UUID
		contact uuid.UUID
		channel string
		wantErr error
	}{
		{"zero tenant", uuid.Nil, contact, "whatsapp", inbox.ErrInvalidTenant},
		{"zero contact", tenant, uuid.Nil, "whatsapp", inbox.ErrInvalidContact},
		{"empty channel", tenant, contact, "  ", inbox.ErrInvalidChannel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inbox.NewConversation(tc.tenant, tc.contact, tc.channel)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewConversation_LowercasesChannel(t *testing.T) {
	c, err := inbox.NewConversation(uuid.New(), uuid.New(), " WhatsApp ")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Channel != "whatsapp" {
		t.Errorf("Channel = %q, want whatsapp", c.Channel)
	}
	if c.State != inbox.ConversationStateOpen {
		t.Errorf("State = %q, want open", c.State)
	}
	if c.AssignedUserID != nil {
		t.Errorf("AssignedUserID = %v, want nil", c.AssignedUserID)
	}
	if !c.LastMessageAt.IsZero() {
		t.Errorf("LastMessageAt = %v, want zero", c.LastMessageAt)
	}
}

func TestConversation_AssignTo(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	user := uuid.New()
	a, err := c.AssignTo(user, inbox.LeadReasonLead)
	if err != nil {
		t.Fatalf("AssignTo: %v", err)
	}
	if a == nil || a.UserID != user || a.Reason != inbox.LeadReasonLead {
		t.Errorf("returned assignment = %+v", a)
	}
	if c.AssignedUserID == nil || *c.AssignedUserID != user {
		t.Errorf("AssignedUserID = %v, want %v", c.AssignedUserID, user)
	}
	if c.Lead() == nil || c.Lead().UserID != user {
		t.Errorf("Lead = %+v, want user %v", c.Lead(), user)
	}
}

func TestConversation_AssignTo_RejectsZeroUser(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	if _, err := c.AssignTo(uuid.Nil, inbox.LeadReasonLead); !errors.Is(err, inbox.ErrInvalidAssignee) {
		t.Errorf("err = %v, want ErrInvalidAssignee", err)
	}
}

func TestConversation_AssignTo_RejectsClosed(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	c.Close()
	if _, err := c.AssignTo(uuid.New(), inbox.LeadReasonLead); !errors.Is(err, inbox.ErrConversationClosed) {
		t.Errorf("err = %v, want ErrConversationClosed", err)
	}
}

func TestConversation_History_ReturnsCopy(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	if _, err := c.AssignTo(uuid.New(), inbox.LeadReasonLead); err != nil {
		t.Fatalf("AssignTo: %v", err)
	}
	got := c.History()
	got[0] = nil // mutate the returned slice
	if len(c.History()) != 1 || c.History()[0] == nil {
		t.Error("History returned an aliased slice; internal state was mutated")
	}
}

func TestConversation_SetHistory_HydratesAndSyncsLeader(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	u1, u2 := uuid.New(), uuid.New()
	hydrated := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	rows := []*inbox.Assignment{
		inbox.HydrateAssignment(uuid.New(), c.TenantID, c.ID, u1, hydrated, inbox.LeadReasonLead),
		inbox.HydrateAssignment(uuid.New(), c.TenantID, c.ID, u2, hydrated.Add(60), inbox.LeadReasonReassign),
	}
	c.SetHistory(rows)
	if c.Lead() == nil || c.Lead().UserID != u2 {
		t.Errorf("Lead after Hydrate = %+v, want user %v", c.Lead(), u2)
	}
	if c.AssignedUserID == nil || *c.AssignedUserID != u2 {
		t.Errorf("AssignedUserID = %v, want %v", c.AssignedUserID, u2)
	}
	if got := c.History(); len(got) != 2 {
		t.Errorf("History len = %d, want 2", len(got))
	}
	// Empty rows should clear.
	c.SetHistory(nil)
	if c.Lead() != nil || len(c.History()) != 0 {
		t.Errorf("after clearing: Lead=%+v History=%+v", c.Lead(), c.History())
	}
}

func TestConversation_CloseAndReopen(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	c.Close()
	if c.State != inbox.ConversationStateClosed {
		t.Fatalf("State = %q, want closed", c.State)
	}
	// Idempotent close.
	c.Close()
	if c.State != inbox.ConversationStateClosed {
		t.Fatalf("State after second Close = %q, want closed", c.State)
	}
	if err := c.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if c.State != inbox.ConversationStateOpen {
		t.Errorf("State after Reopen = %q, want open", c.State)
	}
	if err := c.Reopen(); !errors.Is(err, inbox.ErrConversationAlreadyOpen) {
		t.Errorf("second Reopen err = %v, want ErrConversationAlreadyOpen", err)
	}
}

func TestConversation_RecordMessage_BumpsLastMessageAt(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	t0 := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	m1 := makeMessage(t, c, t0)
	if err := c.RecordMessage(m1); err != nil {
		t.Fatalf("RecordMessage m1: %v", err)
	}
	if !c.LastMessageAt.Equal(t0) {
		t.Errorf("LastMessageAt after m1 = %v, want %v", c.LastMessageAt, t0)
	}
	m2 := makeMessage(t, c, t1)
	if err := c.RecordMessage(m2); err != nil {
		t.Fatalf("RecordMessage m2: %v", err)
	}
	if !c.LastMessageAt.Equal(t1) {
		t.Errorf("LastMessageAt after m2 = %v, want %v", c.LastMessageAt, t1)
	}
}

func TestConversation_RecordMessage_RejectsBackwardsTime(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	t0 := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	tPrior := t0.Add(-time.Minute)
	if err := c.RecordMessage(makeMessage(t, c, t0)); err != nil {
		t.Fatalf("RecordMessage t0: %v", err)
	}
	// Out-of-order: should NOT regress LastMessageAt.
	if err := c.RecordMessage(makeMessage(t, c, tPrior)); err != nil {
		t.Fatalf("RecordMessage prior: %v", err)
	}
	if !c.LastMessageAt.Equal(t0) {
		t.Errorf("LastMessageAt = %v, want unchanged %v", c.LastMessageAt, t0)
	}
}

func TestConversation_RecordMessage_RejectsNil(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	if err := c.RecordMessage(nil); !errors.Is(err, inbox.ErrConversationMismatch) {
		t.Errorf("err = %v, want ErrConversationMismatch", err)
	}
}

func TestConversation_RecordMessage_RejectsClosed(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	m := makeMessage(t, c, time.Now().UTC())
	c.Close()
	if err := c.RecordMessage(m); !errors.Is(err, inbox.ErrConversationClosed) {
		t.Errorf("err = %v, want ErrConversationClosed", err)
	}
}

func TestConversation_RecordMessage_RejectsMismatch(t *testing.T) {
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	// Build a message for a different conversation.
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       c.TenantID,
		ConversationID: uuid.New(),
		Direction:      inbox.MessageDirectionIn,
		Body:           "hi",
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := c.RecordMessage(m); !errors.Is(err, inbox.ErrConversationMismatch) {
		t.Errorf("err = %v, want ErrConversationMismatch", err)
	}
}

func TestHydrateConversation(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	contact := uuid.New()
	user := uuid.New()
	last := time.Now().UTC().Truncate(time.Second)
	created := last.Add(-time.Hour)
	c := inbox.HydrateConversation(id, tenant, contact, "whatsapp",
		inbox.ConversationStateClosed, &user, last, created)
	if c.ID != id || c.TenantID != tenant || c.ContactID != contact ||
		c.Channel != "whatsapp" || c.State != inbox.ConversationStateClosed ||
		c.AssignedUserID == nil || *c.AssignedUserID != user ||
		!c.LastMessageAt.Equal(last) || !c.CreatedAt.Equal(created) {
		t.Errorf("Hydrate mismatch: %+v", c)
	}
}

// makeMessage builds a valid Message in the conversation, pinned at t.
// The body is non-empty so NewMessage accepts it; tests focus on
// conversation-level invariants.
func makeMessage(t *testing.T, c *inbox.Conversation, when time.Time) *inbox.Message {
	t.Helper()
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       c.TenantID,
		ConversationID: c.ID,
		Direction:      inbox.MessageDirectionIn,
		Body:           strings.Repeat("x", 4),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	// Override CreatedAt so RecordMessage's monotonic check sees the
	// scenario the test wants. The constructor populates it via now();
	// we don't touch the package var here so concurrent tests are safe.
	m.CreatedAt = when
	return m
}
