package usecase_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

// TestGetMessage_ProjectsMediaForThreeStatuses verifies the projector
// reflects message.media into MessageView.Media across the three
// terminal-for-render scan states and applies the "hide the key on
// non-clean" rule from [SIN-62805] F2-05d.
//
// Use-case-level (not template-level): the renderer test already
// exercises the template with a hand-crafted view; this test pins down
// the *projection* — that the inbox read path actually populates the
// view from the domain entity, not just that the template knows how to
// render one.
func TestGetMessage_ProjectsMediaForThreeStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		scanStatus string
		hashIn     string
		formatIn   string
		wantHash   string
		wantFormat string
	}{
		{
			name:       "pending hides hash and format intact",
			scanStatus: "pending",
			hashIn:     "abc123",
			formatIn:   "png",
			wantHash:   "", // pending: hide hash so deep-link is impossible
			wantFormat: "png",
		},
		{
			name:       "clean exposes hash",
			scanStatus: "clean",
			hashIn:     "def456",
			formatIn:   "pdf",
			wantHash:   "def456",
			wantFormat: "pdf",
		},
		{
			name:       "infected hides hash even when format is present",
			scanStatus: "infected",
			hashIn:     "ghi789",
			formatIn:   "jpg",
			wantHash:   "", // infected: AC "Sem expor a key infectada"
			wantFormat: "jpg",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newInMemoryRepo()
			tenant := uuid.New()
			conv := mustSeedConv(t, repo, tenant, "whatsapp", inbox.ConversationStateOpen, mustNow())

			msg, err := inbox.NewMessage(inbox.NewMessageInput{
				TenantID:       tenant,
				ConversationID: conv.ID,
				Direction:      inbox.MessageDirectionIn,
				Body:           "image",
			})
			if err != nil {
				t.Fatalf("NewMessage: %v", err)
			}
			msg.AttachMedia(tc.hashIn, tc.formatIn, tc.scanStatus)
			if err := repo.SaveMessage(context.Background(), msg); err != nil {
				t.Fatalf("SaveMessage: %v", err)
			}

			uc := usecase.MustNewGetMessage(repo)
			res, err := uc.Execute(context.Background(), usecase.GetMessageInput{
				TenantID:       tenant,
				ConversationID: conv.ID,
				MessageID:      msg.ID,
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Message.Media == nil {
				t.Fatalf("Media: got nil want populated (scanStatus=%s)", tc.scanStatus)
			}
			if res.Message.Media.ScanStatus != tc.scanStatus {
				t.Fatalf("ScanStatus: got %q want %q", res.Message.Media.ScanStatus, tc.scanStatus)
			}
			if res.Message.Media.Hash != tc.wantHash {
				t.Fatalf("Hash: got %q want %q (scanStatus=%s)", res.Message.Media.Hash, tc.wantHash, tc.scanStatus)
			}
			if res.Message.Media.Format != tc.wantFormat {
				t.Fatalf("Format: got %q want %q", res.Message.Media.Format, tc.wantFormat)
			}
		})
	}
}

// TestGetMessage_TextOnlyMessageHasNilMedia verifies that the projector
// leaves Media nil for text messages (no jsonb row in the adapter, no
// AttachMedia call). The bubble template must distinguish "no media" from
// "media with empty fields".
func TestGetMessage_TextOnlyMessageHasNilMedia(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	tenant := uuid.New()
	conv := mustSeedConv(t, repo, tenant, "whatsapp", inbox.ConversationStateOpen, mustNow())

	msg, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Direction:      inbox.MessageDirectionIn,
		Body:           "hello",
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if err := repo.SaveMessage(context.Background(), msg); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	uc := usecase.MustNewGetMessage(repo)
	res, err := uc.Execute(context.Background(), usecase.GetMessageInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		MessageID:      msg.ID,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Message.Media != nil {
		t.Fatalf("Media: got %+v want nil", res.Message.Media)
	}
}
