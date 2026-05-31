package llmcustomer_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/canned"
	"github.com/pericles-luz/crm/internal/inbox"
)

// recordingInbox is the test double for inbox.InboundChannel. It
// captures every event the adapter forwards downstream so tests can
// assert that scheduled replies and bootstrap injections land with the
// expected shape.
type recordingInbox struct {
	mu     sync.Mutex
	events []inbox.InboundEvent
	err    error
}

func (r *recordingInbox) HandleInbound(_ context.Context, ev inbox.InboundEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingInbox) snapshot() []inbox.InboundEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]inbox.InboundEvent, len(r.events))
	copy(out, r.events)
	return out
}

// recordingLLM captures every call to NextCustomerMessage and returns
// a configurable canned reply. Round-robin replies live in the
// `replies` slice; the call count is exposed for idempotency assertions.
type recordingLLM struct {
	mu        sync.Mutex
	replies   []string
	histories [][]llmcustomer.Turn
	called    atomic.Int32
	err       error
}

func (l *recordingLLM) NextCustomerMessage(_ context.Context, _ string, history []llmcustomer.Turn) (string, error) {
	l.called.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return "", l.err
	}
	snap := make([]llmcustomer.Turn, len(history))
	copy(snap, history)
	l.histories = append(l.histories, snap)
	if len(l.replies) == 0 {
		return "ok", nil
	}
	r := l.replies[0]
	if len(l.replies) > 1 {
		l.replies = l.replies[1:]
	}
	return r, nil
}

func newAdapter(t *testing.T, downstream inbox.InboundChannel, llm llmcustomer.PersonaLLM) *llmcustomer.Adapter {
	t.Helper()
	var counter atomic.Uint64
	cfg := llmcustomer.Config{
		Downstream: downstream,
		LLM:        llm,
		ReplyDelay: 0,
		Now:        func() time.Time { return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC) },
		IDGen: func() string {
			return fmt.Sprintf("test-%d", counter.Add(1))
		},
	}
	a, err := llmcustomer.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Stop)
	return a
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  llmcustomer.Config
	}{
		{name: "nil downstream", cfg: llmcustomer.Config{LLM: canned.NewDefault()}},
		{name: "nil llm", cfg: llmcustomer.Config{Downstream: &recordingInbox{}}},
		{name: "negative delay", cfg: llmcustomer.Config{
			Downstream: &recordingInbox{},
			LLM:        canned.NewDefault(),
			ReplyDelay: -time.Second,
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := llmcustomer.New(tc.cfg); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestChannelReturnsFakellm(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, &recordingInbox{}, canned.NewDefault())
	if got := a.Channel(); got != llmcustomer.ChannelName {
		t.Fatalf("Channel = %q, want %q", got, llmcustomer.ChannelName)
	}
}

func TestSendMessageValidates(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, &recordingInbox{}, canned.NewDefault())
	tenant := uuid.New()
	conv := uuid.New()
	cases := []struct {
		name string
		m    inbox.OutboundMessage
	}{
		{
			name: "nil tenant",
			m:    inbox.OutboundMessage{ConversationID: conv, Channel: llmcustomer.ChannelName, Body: "oi"},
		},
		{
			name: "nil conversation",
			m:    inbox.OutboundMessage{TenantID: tenant, Channel: llmcustomer.ChannelName, Body: "oi"},
		},
		{
			name: "wrong channel",
			m:    inbox.OutboundMessage{TenantID: tenant, ConversationID: conv, Channel: "whatsapp", Body: "oi"},
		},
		{
			name: "blank body",
			m:    inbox.OutboundMessage{TenantID: tenant, ConversationID: conv, Channel: llmcustomer.ChannelName, Body: "   "},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := a.SendMessage(context.Background(), tc.m); err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
}

func TestSendMessageSchedulesCustomerReply(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"resposta-da-mariana"}}
	a := newAdapter(t, downstream, llm)
	tenant := uuid.New()
	conv := uuid.New()

	operatorID, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       tenant,
		ConversationID: conv,
		Channel:        llmcustomer.ChannelName,
		Body:           "olá, posso ajudar?",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if operatorID == "" {
		t.Fatalf("SendMessage returned empty channelExternalID")
	}

	a.Drain()

	events := downstream.snapshot()
	if len(events) != 1 {
		t.Fatalf("downstream got %d events, want 1", len(events))
	}
	got := events[0]
	if got.TenantID != tenant {
		t.Fatalf("event tenant = %v, want %v", got.TenantID, tenant)
	}
	if got.Channel != llmcustomer.ChannelName {
		t.Fatalf("event channel = %q, want %q", got.Channel, llmcustomer.ChannelName)
	}
	if got.SenderExternalID != llmcustomer.SyntheticContactExternalID {
		t.Fatalf("event sender = %q, want %q", got.SenderExternalID, llmcustomer.SyntheticContactExternalID)
	}
	if got.SenderDisplayName != llmcustomer.SyntheticContactDisplayName {
		t.Fatalf("event display = %q, want %q", got.SenderDisplayName, llmcustomer.SyntheticContactDisplayName)
	}
	if got.Body != "resposta-da-mariana" {
		t.Fatalf("event body = %q, want %q", got.Body, "resposta-da-mariana")
	}
	if got.ChannelExternalID == "" {
		t.Fatalf("event channel-external-id is empty")
	}
	if got.OccurredAt.IsZero() {
		t.Fatalf("event OccurredAt is zero")
	}

	// LLM must have seen the operator turn (and only the operator turn —
	// the customer turn is appended only when HandleInbound is called).
	if llm.called.Load() != 1 {
		t.Fatalf("llm called %d times, want 1", llm.called.Load())
	}
	llm.mu.Lock()
	history := llm.histories[0]
	llm.mu.Unlock()
	if len(history) != 1 {
		t.Fatalf("llm history len = %d, want 1", len(history))
	}
	if history[0].Role != llmcustomer.TurnRoleOperator || history[0].Body != "olá, posso ajudar?" {
		t.Fatalf("llm history[0] = %+v, want operator turn 'olá, posso ajudar?'", history[0])
	}
}

func TestSendMessageDropsReplyWhenLLMErrors(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{err: errors.New("boom")}
	a := newAdapter(t, downstream, llm)
	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Channel:        llmcustomer.ChannelName,
		Body:           "olá",
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	a.Drain()
	if got := len(downstream.snapshot()); got != 0 {
		t.Fatalf("downstream got %d events, want 0 (LLM error must drop reply)", got)
	}
}

func TestSendMessageDropsReplyWhenLLMReturnsBlank(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"   \n\t  "}}
	a := newAdapter(t, downstream, llm)
	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Channel:        llmcustomer.ChannelName,
		Body:           "olá",
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	a.Drain()
	if got := len(downstream.snapshot()); got != 0 {
		t.Fatalf("downstream got %d events, want 0 (blank reply must drop)", got)
	}
}

func TestHandleInboundForwardsAndRecordsHistory(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"ok-reply"}}
	a := newAdapter(t, downstream, llm)
	tenant := uuid.New()
	conv := uuid.New()

	if err := a.HandleInbound(context.Background(), inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           llmcustomer.ChannelName,
		ChannelExternalID: "manual-1",
		SenderExternalID:  llmcustomer.SyntheticContactExternalID,
		SenderDisplayName: llmcustomer.SyntheticContactDisplayName,
		Body:              "primeiro contato da cliente",
	}); err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}
	if got := len(downstream.snapshot()); got != 1 {
		t.Fatalf("downstream got %d events after HandleInbound, want 1", got)
	}

	// Subsequent SendMessage's LLM call must see the prior customer
	// turn so the persona answers in-context.
	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       tenant,
		ConversationID: conv,
		Channel:        llmcustomer.ChannelName,
		Body:           "como posso ajudar?",
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	a.Drain()

	llm.mu.Lock()
	if len(llm.histories) != 1 {
		llm.mu.Unlock()
		t.Fatalf("llm called %d times, want 1", len(llm.histories))
	}
	history := llm.histories[0]
	llm.mu.Unlock()
	if len(history) != 2 {
		t.Fatalf("llm history len = %d, want 2", len(history))
	}
	if history[0].Role != llmcustomer.TurnRoleCustomer || history[0].Body != "primeiro contato da cliente" {
		t.Fatalf("history[0] = %+v, want customer turn", history[0])
	}
	if history[1].Role != llmcustomer.TurnRoleOperator || history[1].Body != "como posso ajudar?" {
		t.Fatalf("history[1] = %+v, want operator turn", history[1])
	}
}

func TestHandleInboundValidates(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, &recordingInbox{}, canned.NewDefault())
	cases := []struct {
		name string
		ev   inbox.InboundEvent
	}{
		{name: "nil tenant", ev: inbox.InboundEvent{Channel: llmcustomer.ChannelName, Body: "x"}},
		{name: "wrong channel", ev: inbox.InboundEvent{TenantID: uuid.New(), Channel: "whatsapp", Body: "x"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := a.HandleInbound(context.Background(), tc.ev); err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
}

func TestHandleInboundPropagatesDownstreamError(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{err: errors.New("downstream-boom")}
	a := newAdapter(t, downstream, canned.NewDefault())
	err := a.HandleInbound(context.Background(), inbox.InboundEvent{
		TenantID:          uuid.New(),
		Channel:           llmcustomer.ChannelName,
		ChannelExternalID: "ev-1",
		SenderExternalID:  llmcustomer.SyntheticContactExternalID,
		Body:              "x",
	})
	if err == nil {
		t.Fatalf("expected downstream error, got nil")
	}
}

func TestBootstrapInjectsInitialCustomerMessage(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"oi-inicial"}}
	a := newAdapter(t, downstream, llm)
	tenant := uuid.New()

	if err := a.Bootstrap(context.Background(), tenant); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	events := downstream.snapshot()
	if len(events) != 1 {
		t.Fatalf("downstream got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.TenantID != tenant {
		t.Fatalf("event tenant = %v, want %v", ev.TenantID, tenant)
	}
	if ev.Body != "oi-inicial" {
		t.Fatalf("event body = %q, want %q", ev.Body, "oi-inicial")
	}
	if ev.SenderExternalID != llmcustomer.SyntheticContactExternalID {
		t.Fatalf("event sender = %q, want %q", ev.SenderExternalID, llmcustomer.SyntheticContactExternalID)
	}
	if ev.SenderDisplayName != llmcustomer.SyntheticContactDisplayName {
		t.Fatalf("event display = %q, want %q", ev.SenderDisplayName, llmcustomer.SyntheticContactDisplayName)
	}
	wantExt := fmt.Sprintf("%s:bootstrap:%s", llmcustomer.ChannelName, tenant.String())
	if ev.ChannelExternalID != wantExt {
		t.Fatalf("event channel-external-id = %q, want %q", ev.ChannelExternalID, wantExt)
	}
	if !strings.HasPrefix(ev.ChannelExternalID, llmcustomer.ChannelName+":bootstrap:") {
		t.Fatalf("bootstrap channel-external-id must be stable; got %q", ev.ChannelExternalID)
	}
}

func TestBootstrapIdempotentPerTenantWithinLifetime(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"first"}}
	a := newAdapter(t, downstream, llm)
	tenant := uuid.New()

	for i := 0; i < 3; i++ {
		if err := a.Bootstrap(context.Background(), tenant); err != nil {
			t.Fatalf("Bootstrap call %d: %v", i, err)
		}
	}
	if got := llm.called.Load(); got != 1 {
		t.Fatalf("llm called %d times, want 1 (re-bootstrap must short-circuit)", got)
	}
	if got := len(downstream.snapshot()); got != 1 {
		t.Fatalf("downstream got %d events, want 1", got)
	}
}

func TestBootstrapDistinctPerTenant(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"a-reply", "b-reply"}}
	a := newAdapter(t, downstream, llm)
	tenantA := uuid.New()
	tenantB := uuid.New()

	if err := a.Bootstrap(context.Background(), tenantA); err != nil {
		t.Fatalf("Bootstrap A: %v", err)
	}
	if err := a.Bootstrap(context.Background(), tenantB); err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	events := downstream.snapshot()
	if len(events) != 2 {
		t.Fatalf("downstream got %d events, want 2", len(events))
	}
	tenants := map[uuid.UUID]bool{}
	for _, e := range events {
		tenants[e.TenantID] = true
	}
	if !tenants[tenantA] || !tenants[tenantB] {
		t.Fatalf("expected events for both tenants, got %v", tenants)
	}
}

func TestBootstrapValidatesTenant(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, &recordingInbox{}, canned.NewDefault())
	if err := a.Bootstrap(context.Background(), uuid.Nil); err == nil {
		t.Fatalf("expected error for nil tenant, got nil")
	}
}

func TestBootstrapPropagatesLLMError(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{err: errors.New("kaboom")}
	a := newAdapter(t, downstream, llm)
	if err := a.Bootstrap(context.Background(), uuid.New()); err == nil {
		t.Fatalf("expected LLM error, got nil")
	}
	if got := len(downstream.snapshot()); got != 0 {
		t.Fatalf("downstream got %d events, want 0 (LLM error must abort bootstrap)", got)
	}
}

func TestBootstrapPropagatesBlankLLMReply(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"   "}}
	a := newAdapter(t, downstream, llm)
	if err := a.Bootstrap(context.Background(), uuid.New()); err == nil {
		t.Fatalf("expected error for blank LLM reply, got nil")
	}
}

func TestStopIsIdempotentAndBlocksFurtherWork(t *testing.T) {
	t.Parallel()
	a, err := llmcustomer.New(llmcustomer.Config{
		Downstream: &recordingInbox{},
		LLM:        canned.NewDefault(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.Stop()
	a.Stop() // must be a no-op

	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Channel:        llmcustomer.ChannelName,
		Body:           "x",
	}); err == nil {
		t.Fatalf("expected SendMessage to fail after Stop, got nil")
	}
	if err := a.Bootstrap(context.Background(), uuid.New()); err == nil {
		t.Fatalf("expected Bootstrap to fail after Stop, got nil")
	}
}

func TestSendMessageRespectsReplyDelayAndCancellation(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{replies: []string{"never-arrives"}}
	a, err := llmcustomer.New(llmcustomer.Config{
		Downstream: downstream,
		LLM:        llm,
		ReplyDelay: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Channel:        llmcustomer.ChannelName,
		Body:           "olá",
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Stop before the delay elapses; the goroutine must observe ctx
	// cancellation and skip the LLM + downstream calls.
	a.Stop()
	if got := llm.called.Load(); got != 0 {
		t.Fatalf("llm called %d times, want 0 (cancellation must skip LLM)", got)
	}
	if got := len(downstream.snapshot()); got != 0 {
		t.Fatalf("downstream got %d events, want 0", got)
	}
}

func TestConcurrentSendsAreSafe(t *testing.T) {
	t.Parallel()
	downstream := &recordingInbox{}
	llm := &recordingLLM{}
	a := newAdapter(t, downstream, llm)
	tenant := uuid.New()
	conv := uuid.New()

	var wg sync.WaitGroup
	const fanout = 32
	wg.Add(fanout)
	for i := 0; i < fanout; i++ {
		go func() {
			defer wg.Done()
			_, _ = a.SendMessage(context.Background(), inbox.OutboundMessage{
				TenantID:       tenant,
				ConversationID: conv,
				Channel:        llmcustomer.ChannelName,
				Body:           "x",
			})
		}()
	}
	wg.Wait()
	a.Drain()

	if got := len(downstream.snapshot()); got != fanout {
		t.Fatalf("downstream got %d events, want %d", got, fanout)
	}
	if got := llm.called.Load(); int(got) != fanout {
		t.Fatalf("llm called %d times, want %d", got, fanout)
	}
}
