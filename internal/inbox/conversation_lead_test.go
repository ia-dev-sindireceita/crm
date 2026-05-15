package inbox_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestConversation_AssignLead_AppendsHistoryAndUpdatesLeader(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	if c.Lead() != nil {
		t.Fatalf("Lead before AssignLead = %+v, want nil", c.Lead())
	}
	user := uuid.New()
	a, err := c.AssignLead(user, inbox.LeadReasonLead)
	if err != nil {
		t.Fatalf("AssignLead: %v", err)
	}
	if a == nil || a.UserID != user || a.Reason != inbox.LeadReasonLead {
		t.Fatalf("returned assignment = %+v", a)
	}
	if c.Lead() == nil || c.Lead().UserID != user {
		t.Errorf("Lead = %+v, want user %v", c.Lead(), user)
	}
	if c.AssignedUserID == nil || *c.AssignedUserID != user {
		t.Errorf("AssignedUserID = %v, want %v", c.AssignedUserID, user)
	}
	if got := c.History(); len(got) != 1 || got[0].UserID != user {
		t.Errorf("History = %+v, want one row for user %v", got, user)
	}
}

func TestConversation_AssignLead_TwoTransitions_OrderPreserved(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	u1, u2 := uuid.New(), uuid.New()
	if _, err := c.AssignLead(u1, inbox.LeadReasonLead); err != nil {
		t.Fatalf("first AssignLead: %v", err)
	}
	if _, err := c.AssignLead(u2, inbox.LeadReasonReassign); err != nil {
		t.Fatalf("second AssignLead: %v", err)
	}
	hist := c.History()
	if len(hist) != 2 {
		t.Fatalf("History len = %d, want 2", len(hist))
	}
	if hist[0].UserID != u1 || hist[1].UserID != u2 {
		t.Errorf("history order = [%v, %v], want [%v, %v]",
			hist[0].UserID, hist[1].UserID, u1, u2)
	}
	if hist[1].Reason != inbox.LeadReasonReassign {
		t.Errorf("latest reason = %q, want %q", hist[1].Reason, inbox.LeadReasonReassign)
	}
	if c.Lead().UserID != u2 {
		t.Errorf("Lead = %v, want latest %v", c.Lead().UserID, u2)
	}
	if c.AssignedUserID == nil || *c.AssignedUserID != u2 {
		t.Errorf("AssignedUserID = %v, want %v", c.AssignedUserID, u2)
	}
}

func TestConversation_AssignLead_RejectsClosed(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	c.Close()
	if _, err := c.AssignLead(uuid.New(), inbox.LeadReasonLead); !errors.Is(err, inbox.ErrConversationClosed) {
		t.Errorf("err = %v, want ErrConversationClosed", err)
	}
}

func TestConversation_AssignLead_RejectsInvalidReason(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	if _, err := c.AssignLead(uuid.New(), inbox.LeadReason("")); !errors.Is(err, inbox.ErrInvalidLeadReason) {
		t.Errorf("err = %v, want ErrInvalidLeadReason", err)
	}
}

func TestConversation_AssignLead_RejectsZeroUser(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	if _, err := c.AssignLead(uuid.Nil, inbox.LeadReasonLead); !errors.Is(err, inbox.ErrInvalidAssignee) {
		t.Errorf("err = %v, want ErrInvalidAssignee", err)
	}
}

func TestConversation_History_ReturnsCopy(t *testing.T) {
	t.Parallel()
	c, _ := inbox.NewConversation(uuid.New(), uuid.New(), "whatsapp")
	if _, err := c.AssignLead(uuid.New(), inbox.LeadReasonLead); err != nil {
		t.Fatalf("AssignLead: %v", err)
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
	rows := []*inbox.Assignment{
		inbox.HydrateLeaderAssignment(uuid.New(), c.TenantID, c.ID, u1, leaderFixedTime, inbox.LeadReasonLead),
		inbox.HydrateLeaderAssignment(uuid.New(), c.TenantID, c.ID, u2, leaderFixedTime.Add(60), inbox.LeadReasonReassign),
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
