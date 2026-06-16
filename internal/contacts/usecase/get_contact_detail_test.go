package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeConversationReader is an in-process conversationHistoryReader. It
// records the limit it was called with so tests can assert the default is
// applied, and can inject an error.
type fakeConversationReader struct {
	convs     []*inbox.Conversation
	err       error
	gotLimit  int
	gotCalled bool
}

func (f *fakeConversationReader) ListConversationsByContact(_ context.Context, _ uuid.UUID, _ uuid.UUID, limit int) ([]*inbox.Conversation, error) {
	f.gotCalled = true
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.convs, nil
}

func seedConversation(t *testing.T, tenant, contactID uuid.UUID, channel string) *inbox.Conversation {
	t.Helper()
	c, err := inbox.NewConversation(tenant, contactID, channel)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	return c
}

func TestNewGetContactDetail_RejectsNilRepo(t *testing.T) {
	if u, err := NewGetContactDetail(nil, nil); err == nil || u != nil {
		t.Errorf("NewGetContactDetail(nil) = (%v, %v), want (nil, error)", u, err)
	}
}

func TestMustNewGetContactDetail_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewGetContactDetail(nil) did not panic")
		}
	}()
	_ = MustNewGetContactDetail(nil, nil)
}

func TestGetContactDetail_RejectsNilTenant(t *testing.T) {
	u := MustNewGetContactDetail(newFakeRepo(), nil)
	_, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: uuid.Nil, ContactID: uuid.New()})
	if !errors.Is(err, contacts.ErrInvalidTenant) {
		t.Errorf("err = %v, want ErrInvalidTenant", err)
	}
}

func TestGetContactDetail_RejectsNilContactID(t *testing.T) {
	u := MustNewGetContactDetail(newFakeRepo(), nil)
	_, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: uuid.New(), ContactID: uuid.Nil})
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetContactDetail_NotFound(t *testing.T) {
	u := MustNewGetContactDetail(newFakeRepo(), nil)
	_, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: uuid.New(), ContactID: uuid.New()})
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetContactDetail_HappyPath_WithIdentitiesAndHistory(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice",
		contacts.ChannelIdentity{Channel: "whatsapp", ExternalID: "+5511999990001"},
		contacts.ChannelIdentity{Channel: "email", ExternalID: "alice@example.com"},
	)

	conv := seedConversation(t, tenant, c.ID, "whatsapp")
	reader := &fakeConversationReader{convs: []*inbox.Conversation{conv}}

	u := MustNewGetContactDetail(repo, reader)
	res, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: tenant, ContactID: c.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	d := res.Contact
	if d.ID != c.ID || d.DisplayName != "Alice" {
		t.Errorf("detail header = %s/%q, want %s/Alice", d.ID, d.DisplayName, c.ID)
	}
	if len(d.Identities) != 2 {
		t.Errorf("identities = %d, want 2", len(d.Identities))
	}
	if len(d.Channels) != 2 || d.Channels[0] != "email" || d.Channels[1] != "whatsapp" {
		t.Errorf("Channels = %v, want [email whatsapp]", d.Channels)
	}
	if len(d.Conversations) != 1 {
		t.Fatalf("conversations = %d, want 1", len(d.Conversations))
	}
	if d.Conversations[0].ID != conv.ID || d.Conversations[0].Channel != "whatsapp" {
		t.Errorf("conversation summary = %+v", d.Conversations[0])
	}
	if d.Conversations[0].State != string(inbox.ConversationStateOpen) {
		t.Errorf("State = %q, want open", d.Conversations[0].State)
	}
	// Default conversation limit applied.
	if reader.gotLimit != defaultDetailConversationLimit {
		t.Errorf("conversation limit = %d, want default %d", reader.gotLimit, defaultDetailConversationLimit)
	}
}

func TestGetContactDetail_AppliesExplicitConversationLimit(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice")
	reader := &fakeConversationReader{}
	u := MustNewGetContactDetail(repo, reader)
	_, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: tenant, ContactID: c.ID, ConversationLimit: 7})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if reader.gotLimit != 7 {
		t.Errorf("conversation limit = %d, want 7", reader.gotLimit)
	}
}

func TestGetContactDetail_NilReaderDegradesToEmptyHistory(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice")
	u := MustNewGetContactDetail(repo, nil) // no conversation reader wired
	res, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: tenant, ContactID: c.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Contact.Conversations) != 0 {
		t.Errorf("conversations = %d, want 0 (degraded)", len(res.Contact.Conversations))
	}
}

func TestGetContactDetail_PropagatesHistoryError(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice")
	sentinel := errors.New("synthetic history failure")
	reader := &fakeConversationReader{err: sentinel}
	u := MustNewGetContactDetail(repo, reader)
	_, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: tenant, ContactID: c.ID})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestGetContactDetail_PropagatesContactFindError(t *testing.T) {
	repo := newFakeRepo()
	sentinel := errors.New("synthetic find failure")
	repo.findErr = sentinel
	u := MustNewGetContactDetail(repo, nil)
	_, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: uuid.New(), ContactID: uuid.New()})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestGetContactDetail_AssignedFlag(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	c := seedContact(t, repo, tenant, "Alice")
	conv := seedConversation(t, tenant, c.ID, "whatsapp")
	uid := uuid.New()
	now := time.Now().UTC()
	assigned := inbox.HydrateConversation(conv.ID, tenant, c.ID, "whatsapp",
		inbox.ConversationStateOpen, &uid, now, now)
	reader := &fakeConversationReader{convs: []*inbox.Conversation{assigned}}
	u := MustNewGetContactDetail(repo, reader)
	res, err := u.Execute(context.Background(), GetContactDetailInput{TenantID: tenant, ContactID: c.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cv := res.Contact.Conversations[0]
	if !cv.Assigned || cv.AssignedUserID == nil || *cv.AssignedUserID != uid {
		t.Errorf("assigned projection wrong: %+v", cv)
	}
}
