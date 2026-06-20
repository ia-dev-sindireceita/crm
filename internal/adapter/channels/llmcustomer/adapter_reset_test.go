package llmcustomer_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/inbox"
)

func TestResetConversation_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, &recordingInbox{}, &recordingLLM{})
	if err := a.ResetConversation(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Fatal("ResetConversation(nil tenant) err = nil, want error")
	}
}

// TestResetConversation_ClearsBootstrapped proves the bootstrapped flag
// is cleared: Bootstrap is idempotent per tenant within a lifetime (one
// LLM call), but after a reset a fresh Bootstrap consults the LLM again.
func TestResetConversation_ClearsBootstrapped(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{replies: []string{"greeting-1", "greeting-2"}}
	a := newAdapter(t, &recordingInbox{}, llm)
	tenant := uuid.New()

	if err := a.Bootstrap(context.Background(), tenant); err != nil {
		t.Fatalf("Bootstrap #1: %v", err)
	}
	// Idempotent: a second Bootstrap without a reset does not re-call.
	if err := a.Bootstrap(context.Background(), tenant); err != nil {
		t.Fatalf("Bootstrap #2 (idempotent): %v", err)
	}
	if got := llm.called.Load(); got != 1 {
		t.Fatalf("llm called %d times before reset, want 1 (bootstrap idempotent)", got)
	}

	if err := a.ResetConversation(context.Background(), tenant, uuid.New()); err != nil {
		t.Fatalf("ResetConversation: %v", err)
	}

	// After the reset the bootstrapped flag is gone → Bootstrap runs again.
	if err := a.Bootstrap(context.Background(), tenant); err != nil {
		t.Fatalf("Bootstrap after reset: %v", err)
	}
	if got := llm.called.Load(); got != 2 {
		t.Fatalf("llm called %d times after reset+bootstrap, want 2", got)
	}
}

// TestResetConversation_ClearsHistory proves the turn history is cleared:
// after a reset, the LLM call triggered by the next operator send sees
// only the new operator turn, not the pre-reset turns.
func TestResetConversation_ClearsHistory(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{replies: []string{"r1", "r2"}}
	a := newAdapter(t, &recordingInbox{}, llm)
	tenant := uuid.New()
	conv := uuid.New()

	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       tenant,
		ConversationID: conv,
		Channel:        llmcustomer.ChannelName,
		Body:           "primeira",
	}); err != nil {
		t.Fatalf("SendMessage #1: %v", err)
	}
	a.Drain()

	if err := a.ResetConversation(context.Background(), tenant, conv); err != nil {
		t.Fatalf("ResetConversation: %v", err)
	}

	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       tenant,
		ConversationID: conv,
		Channel:        llmcustomer.ChannelName,
		Body:           "depois-do-reset",
	}); err != nil {
		t.Fatalf("SendMessage #2: %v", err)
	}
	a.Drain()

	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.histories) != 2 {
		t.Fatalf("llm histories len = %d, want 2", len(llm.histories))
	}
	// The post-reset call must see exactly one turn (the new operator
	// message) — the pre-reset "primeira" turn was cleared.
	last := llm.histories[1]
	if len(last) != 1 {
		t.Fatalf("post-reset history len = %d, want 1 (history cleared)", len(last))
	}
	if last[0].Body != "depois-do-reset" {
		t.Fatalf("post-reset history[0].Body = %q, want %q", last[0].Body, "depois-do-reset")
	}
}
