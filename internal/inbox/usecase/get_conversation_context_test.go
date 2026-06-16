package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

// fakeContactReader is an in-memory contactReader. Tenant scope mirrors
// the Postgres adapter: a contact owned by another tenant collapses to
// contacts.ErrNotFound. err, when set, is returned verbatim so the
// hard-error propagation path can be exercised.
type fakeContactReader struct {
	byID map[uuid.UUID]*contacts.Contact
	err  error
}

func (f *fakeContactReader) FindByID(_ context.Context, tenantID, id uuid.UUID) (*contacts.Contact, error) {
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.byID[id]
	if !ok || c.TenantID != tenantID {
		return nil, contacts.ErrNotFound
	}
	return c, nil
}

// fakeTransitionReader is an in-memory funnelTransitionReader.
type fakeTransitionReader struct {
	byConv map[uuid.UUID]*funnel.Transition
	err    error
}

func (f *fakeTransitionReader) LatestForConversation(_ context.Context, tenantID, conversationID uuid.UUID) (*funnel.Transition, error) {
	if f.err != nil {
		return nil, f.err
	}
	t, ok := f.byConv[conversationID]
	if !ok || t.TenantID != tenantID {
		return nil, funnel.ErrNotFound
	}
	return t, nil
}

// fakeStageReader is an in-memory funnelStageReader.
type fakeStageReader struct {
	byID map[uuid.UUID]*funnel.Stage
	err  error
}

func (f *fakeStageReader) FindByID(_ context.Context, tenantID, stageID uuid.UUID) (*funnel.Stage, error) {
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.byID[stageID]
	if !ok || s.TenantID != tenantID {
		return nil, funnel.ErrNotFound
	}
	return s, nil
}

// seedConversation builds an open conversation under tenantID with the
// given contact + channel and stores it in repo. When assignTo is
// non-nil it sets AssignedUserID directly (the read use case only reads
// the field, so we bypass the AssignTo domain method to keep the
// fixture minimal).
func seedConversation(t *testing.T, repo *inMemoryRepo, tenantID, contactID uuid.UUID, channel string, assignTo *uuid.UUID) *inbox.Conversation {
	t.Helper()
	conv, err := inbox.NewConversation(tenantID, contactID, channel)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	conv.AssignedUserID = assignTo
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv
}

func TestGetConversationContext_Execute(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()
	stageID := uuid.New()

	type fixture struct {
		repo        *inMemoryRepo
		contacts    *fakeContactReader
		transitions *fakeTransitionReader
		stages      *fakeStageReader
		convID      uuid.UUID
		contactID   uuid.UUID
	}

	tests := []struct {
		name   string
		setup  func(t *testing.T) fixture
		assert func(t *testing.T, got usecase.ConversationContextView)
	}{
		{
			name: "full data",
			setup: func(t *testing.T) fixture {
				repo := newInMemoryRepo()
				contactID := uuid.New()
				conv := seedConversation(t, repo, tenantID, contactID, "whatsapp", &userID)

				c, err := contacts.New(tenantID, "Ada Lovelace")
				if err != nil {
					t.Fatalf("contacts.New: %v", err)
				}
				if err := c.AddChannelIdentity("whatsapp", "+5511999999999"); err != nil {
					t.Fatalf("AddChannelIdentity: %v", err)
				}
				return fixture{
					repo:     repo,
					contacts: &fakeContactReader{byID: map[uuid.UUID]*contacts.Contact{contactID: c}},
					transitions: &fakeTransitionReader{byConv: map[uuid.UUID]*funnel.Transition{
						conv.ID: {TenantID: tenantID, ConversationID: conv.ID, ToStageID: stageID},
					}},
					stages: &fakeStageReader{byID: map[uuid.UUID]*funnel.Stage{
						stageID: {ID: stageID, TenantID: tenantID, Key: "qualificacao", Label: "Qualificação"},
					}},
					convID:    conv.ID,
					contactID: contactID,
				}
			},
			assert: func(t *testing.T, got usecase.ConversationContextView) {
				if got.Channel != "whatsapp" {
					t.Errorf("Channel = %q, want whatsapp", got.Channel)
				}
				if got.ContactDisplayName != "Ada Lovelace" {
					t.Errorf("ContactDisplayName = %q", got.ContactDisplayName)
				}
				if len(got.ContactIdentities) != 1 ||
					got.ContactIdentities[0].Channel != "whatsapp" ||
					got.ContactIdentities[0].ExternalID != "+5511999999999" {
					t.Errorf("ContactIdentities = %+v", got.ContactIdentities)
				}
				if got.FunnelStageKey != "qualificacao" || got.FunnelStageName != "Qualificação" {
					t.Errorf("funnel stage = %q/%q", got.FunnelStageKey, got.FunnelStageName)
				}
				if !got.Assigned || got.AssignedUserID == nil || *got.AssignedUserID != userID {
					t.Errorf("assignment = %v/%v", got.Assigned, got.AssignedUserID)
				}
			},
		},
		{
			name: "missing contact",
			setup: func(t *testing.T) fixture {
				repo := newInMemoryRepo()
				contactID := uuid.New()
				conv := seedConversation(t, repo, tenantID, contactID, "whatsapp", &userID)
				return fixture{
					repo:        repo,
					contacts:    &fakeContactReader{byID: map[uuid.UUID]*contacts.Contact{}}, // empty → ErrNotFound
					transitions: &fakeTransitionReader{byConv: map[uuid.UUID]*funnel.Transition{}},
					stages:      &fakeStageReader{byID: map[uuid.UUID]*funnel.Stage{}},
					convID:      conv.ID,
					contactID:   contactID,
				}
			},
			assert: func(t *testing.T, got usecase.ConversationContextView) {
				if got.ContactDisplayName != "" || got.ContactIdentities != nil {
					t.Errorf("expected zero-value contact, got %q / %+v", got.ContactDisplayName, got.ContactIdentities)
				}
				// channel + assignment still resolve.
				if got.Channel != "whatsapp" || !got.Assigned {
					t.Errorf("expected partial render: channel=%q assigned=%v", got.Channel, got.Assigned)
				}
			},
		},
		{
			name: "no funnel transition",
			setup: func(t *testing.T) fixture {
				repo := newInMemoryRepo()
				contactID := uuid.New()
				conv := seedConversation(t, repo, tenantID, contactID, "whatsapp", &userID)
				c, _ := contacts.New(tenantID, "Grace Hopper")
				return fixture{
					repo:        repo,
					contacts:    &fakeContactReader{byID: map[uuid.UUID]*contacts.Contact{contactID: c}},
					transitions: &fakeTransitionReader{byConv: map[uuid.UUID]*funnel.Transition{}}, // empty → ErrNotFound
					stages:      &fakeStageReader{byID: map[uuid.UUID]*funnel.Stage{}},
					convID:      conv.ID,
					contactID:   contactID,
				}
			},
			assert: func(t *testing.T, got usecase.ConversationContextView) {
				if got.FunnelStageKey != "" || got.FunnelStageName != "" {
					t.Errorf("expected zero-value funnel stage, got %q/%q", got.FunnelStageKey, got.FunnelStageName)
				}
				if got.ContactDisplayName != "Grace Hopper" {
					t.Errorf("contact should still resolve, got %q", got.ContactDisplayName)
				}
			},
		},
		{
			name: "unassigned",
			setup: func(t *testing.T) fixture {
				repo := newInMemoryRepo()
				contactID := uuid.New()
				conv := seedConversation(t, repo, tenantID, contactID, "telegram", nil)
				c, _ := contacts.New(tenantID, "Alan Turing")
				return fixture{
					repo:        repo,
					contacts:    &fakeContactReader{byID: map[uuid.UUID]*contacts.Contact{contactID: c}},
					transitions: &fakeTransitionReader{byConv: map[uuid.UUID]*funnel.Transition{}},
					stages:      &fakeStageReader{byID: map[uuid.UUID]*funnel.Stage{}},
					convID:      conv.ID,
					contactID:   contactID,
				}
			},
			assert: func(t *testing.T, got usecase.ConversationContextView) {
				if got.Assigned || got.AssignedUserID != nil {
					t.Errorf("expected unassigned, got assigned=%v id=%v", got.Assigned, got.AssignedUserID)
				}
				if got.Channel != "telegram" {
					t.Errorf("Channel = %q, want telegram", got.Channel)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.setup(t)
			uc := usecase.MustNewGetConversationContext(f.repo, f.contacts, f.transitions, f.stages)
			res, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{
				TenantID:       tenantID,
				ConversationID: f.convID,
			})
			if err != nil {
				t.Fatalf("Execute: unexpected error %v", err)
			}
			if res.Context.ConversationID != f.convID {
				t.Errorf("ConversationID = %v, want %v", res.Context.ConversationID, f.convID)
			}
			if res.Context.ContactID != f.contactID {
				t.Errorf("ContactID = %v, want %v", res.Context.ContactID, f.contactID)
			}
			tc.assert(t, res.Context)
		})
	}
}

func TestGetConversationContext_InvalidInput(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	uc := usecase.MustNewGetConversationContext(repo, nil, nil, nil)

	t.Run("nil tenant", func(t *testing.T) {
		t.Parallel()
		_, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{
			ConversationID: uuid.New(),
		})
		if !errors.Is(err, inbox.ErrInvalidTenant) {
			t.Fatalf("err = %v, want ErrInvalidTenant", err)
		}
	})

	t.Run("nil conversation id", func(t *testing.T) {
		t.Parallel()
		_, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{
			TenantID: uuid.New(),
		})
		if !errors.Is(err, usecase.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("conversation missing", func(t *testing.T) {
		t.Parallel()
		_, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{
			TenantID:       uuid.New(),
			ConversationID: uuid.New(),
		})
		if !errors.Is(err, usecase.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestGetConversationContext_NilOptionalReaders proves a deployment that
// has not wired contacts/funnel storage still renders the channel +
// assignment panel (nil readers degrade to zero-valued fields).
func TestGetConversationContext_NilOptionalReaders(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	repo := newInMemoryRepo()
	contactID := uuid.New()
	conv := seedConversation(t, repo, tenantID, contactID, "whatsapp", nil)

	uc := usecase.MustNewGetConversationContext(repo, nil, nil, nil)
	res, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{
		TenantID:       tenantID,
		ConversationID: conv.ID,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Context.Channel != "whatsapp" {
		t.Errorf("Channel = %q", res.Context.Channel)
	}
	if res.Context.ContactDisplayName != "" || res.Context.FunnelStageKey != "" {
		t.Errorf("expected zero contact/funnel, got %+v", res.Context)
	}
}

// TestGetConversationContext_SubReadErrorsPropagate proves a genuine
// (non-ErrNotFound) storage failure in a sub-read is surfaced rather
// than masked as zero-values.
func TestGetConversationContext_SubReadErrorsPropagate(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	boom := errors.New("storage unavailable")
	stageID := uuid.New()

	t.Run("contact hard error", func(t *testing.T) {
		t.Parallel()
		repo := newInMemoryRepo()
		contactID := uuid.New()
		conv := seedConversation(t, repo, tenantID, contactID, "whatsapp", nil)
		uc := usecase.MustNewGetConversationContext(repo, &fakeContactReader{err: boom}, nil, nil)
		_, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{TenantID: tenantID, ConversationID: conv.ID})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})

	t.Run("transition hard error", func(t *testing.T) {
		t.Parallel()
		repo := newInMemoryRepo()
		contactID := uuid.New()
		conv := seedConversation(t, repo, tenantID, contactID, "whatsapp", nil)
		uc := usecase.MustNewGetConversationContext(repo, nil, &fakeTransitionReader{err: boom}, nil)
		_, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{TenantID: tenantID, ConversationID: conv.ID})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})

	t.Run("stage hard error", func(t *testing.T) {
		t.Parallel()
		repo := newInMemoryRepo()
		contactID := uuid.New()
		conv := seedConversation(t, repo, tenantID, contactID, "whatsapp", nil)
		transitions := &fakeTransitionReader{byConv: map[uuid.UUID]*funnel.Transition{
			conv.ID: {TenantID: tenantID, ConversationID: conv.ID, ToStageID: stageID},
		}}
		uc := usecase.MustNewGetConversationContext(repo, nil, transitions, &fakeStageReader{err: boom})
		_, err := uc.Execute(context.Background(), usecase.GetConversationContextInput{TenantID: tenantID, ConversationID: conv.ID})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

func TestNewGetConversationContext_NilConversations(t *testing.T) {
	t.Parallel()
	_, err := usecase.NewGetConversationContext(nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for nil conversations reader")
	}
}
