package inbox_test

// SIN-64969 — the conversation view must feed the AI-assist policy the
// real conversation channel scope (replacing the PR10 empty-scope stub)
// and render it into the view template.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubConversationContext returns a fixed ConversationContextView (or an
// error) for any input. It records the input so the test can assert the
// handler passes the tenant + conversation scope through.
type stubConversationContext struct {
	channel string
	err     error
	mu      sync.Mutex
	last    inboxusecase.GetConversationContextInput
}

func (s *stubConversationContext) Execute(_ context.Context, in inboxusecase.GetConversationContextInput) (inboxusecase.GetConversationContextResult, error) {
	s.mu.Lock()
	s.last = in
	s.mu.Unlock()
	if s.err != nil {
		return inboxusecase.GetConversationContextResult{}, s.err
	}
	return inboxusecase.GetConversationContextResult{
		Context: inboxusecase.ConversationContextView{Channel: s.channel},
	}, nil
}

// recordingPolicy captures the channel/team scope the handler hands the
// AI-assist policy check.
type recordingPolicy struct {
	enabled bool
	mu      sync.Mutex
	channel string
	team    string
	calls   int
}

func (p *recordingPolicy) IsEnabled(_ context.Context, _ uuid.UUID, channelID, teamID string) (bool, error) {
	p.mu.Lock()
	p.calls++
	p.channel = channelID
	p.team = teamID
	p.mu.Unlock()
	return p.enabled, nil
}

func TestView_FeedsRealChannelScopeToAssistPolicy(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: conv, Direction: "in", Body: "hi", Status: "delivered"},
	}}}
	policy := &recordingPolicy{enabled: true}
	ctxUC := &stubConversationContext{channel: "whatsapp"}

	h, err := webinbox.New(webinbox.Deps{
		ListConversations:   &stubLister{},
		ListMessages:        messages,
		SendOutbound:        &stubSender{},
		GetMessage:          &stubGetMessage{},
		ConversationContext: ctxUC,
		CSRFToken:           func(*http.Request) string { return "tok" },
		UserID:              func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer: &stubSummarizer{},
			Policy:     policy,
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	r := httptest.NewRequest(http.MethodGet, "/inbox/conversations/"+conv.String(), nil)
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenant}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%q", rec.Code, rec.Body.String())
	}
	// Policy received the real channel scope, not the empty PR10 stub.
	policy.mu.Lock()
	gotChannel, gotTeam, calls := policy.channel, policy.team, policy.calls
	policy.mu.Unlock()
	if calls == 0 {
		t.Fatal("policy.IsEnabled was not called")
	}
	if gotChannel != "whatsapp" {
		t.Errorf("policy channel scope = %q, want whatsapp", gotChannel)
	}
	if gotTeam != "" {
		t.Errorf("policy team scope = %q, want empty", gotTeam)
	}
	// The context read got the right tenant + conversation scope.
	ctxUC.mu.Lock()
	last := ctxUC.last
	ctxUC.mu.Unlock()
	if last.TenantID != tenant || last.ConversationID != conv {
		t.Errorf("context input = %+v, want tenant=%v conv=%v", last, tenant, conv)
	}
	// The channel is rendered into the hidden assist-button input.
	if !strings.Contains(rec.Body.String(), `name="channelId" value="whatsapp"`) {
		t.Errorf("body missing real channel scope in assist button: %q", rec.Body.String())
	}
}

// TestView_ContextReadErrorDegradesToEmptyScope proves a failing context
// read does not break the pane — it falls back to the empty channel
// scope (the policy resolver then uses its tenant-scope default).
func TestView_ContextReadErrorDegradesToEmptyScope(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: conv, Direction: "in", Body: "hi", Status: "delivered"},
	}}}
	policy := &recordingPolicy{enabled: true}
	ctxUC := &stubConversationContext{err: errors.New("storage down")}

	h, err := webinbox.New(webinbox.Deps{
		ListConversations:   &stubLister{},
		ListMessages:        messages,
		SendOutbound:        &stubSender{},
		GetMessage:          &stubGetMessage{},
		ConversationContext: ctxUC,
		CSRFToken:           func(*http.Request) string { return "tok" },
		UserID:              func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer: &stubSummarizer{},
			Policy:     policy,
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	r := httptest.NewRequest(http.MethodGet, "/inbox/conversations/"+conv.String(), nil)
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenant}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%q", rec.Code, rec.Body.String())
	}
	policy.mu.Lock()
	gotChannel := policy.channel
	policy.mu.Unlock()
	if gotChannel != "" {
		t.Errorf("expected empty channel scope on degraded read, got %q", gotChannel)
	}
}
