package inbox_test

// SIN-63939 / UX-F2 — tests for the three-pane inbox shell, the right
// rail customer panel, the per-channel SVG badge component, and the
// CustomerInfoLoader port.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubCustomerInfo is a controllable CustomerInfoLoader. The recorded
// args let tests assert that the handler propagates the right tenant
// and conversation IDs through the port.
type stubCustomerInfo struct {
	mu       sync.Mutex
	called   bool
	tenantID uuid.UUID
	convID   uuid.UUID
	out      webinbox.CustomerInfo
	err      error
}

func (s *stubCustomerInfo) Load(_ context.Context, tenantID, conversationID uuid.UUID) (webinbox.CustomerInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	s.tenantID = tenantID
	s.convID = conversationID
	return s.out, s.err
}

func TestList_RendersThreePaneShell(t *testing.T) {
	t.Parallel()
	convID := uuid.New()
	lister := &stubLister{res: inboxusecase.ListConversationsResult{Items: []inboxusecase.ConversationView{{
		ID:            convID,
		Channel:       "whatsapp",
		LastMessageAt: time.Now(),
	}}}}
	h := newHandler(t, lister, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="inbox-shell"`,
		`data-testid="inbox-list-pane"`,
		`data-testid="inbox-conversation-pane"`,
		`data-testid="customer-panel"`,
		`data-testid="customer-empty"`,
		`<aside id="inbox-customer-pane"`,
		// initial empty conversation-pane copy + nav hint
		`Selecione uma conversa.`,
		`← lista à esquerda`,
		// new SVG-based channel badge appears alongside the legacy text node
		`data-testid="channel-badge-whatsapp"`,
		`aria-label="Canal: WhatsApp"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestChannelBadge_AllSupportedChannelsRender(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	cases := []struct {
		channel string
		testid  string
		label   string
	}{
		{"whatsapp", "channel-badge-whatsapp", "WhatsApp"},
		{"instagram", "channel-badge-instagram", "Instagram"},
		{"facebook", "channel-badge-facebook", "Facebook"},
		{"chatbot", "channel-badge-chatbot", "Chatbot"},
		{"", "channel-badge-unknown", "Canal desconhecido"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.channel, func(t *testing.T) {
			t.Parallel()
			lister := &stubLister{res: inboxusecase.ListConversationsResult{Items: []inboxusecase.ConversationView{{
				ID:            uuid.New(),
				Channel:       tc.channel,
				LastMessageAt: time.Now(),
			}}}}
			h := newHandler(t, lister, &stubMessages{}, &stubSender{})
			mux := http.NewServeMux()
			h.Routes(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", tenant))
			body := rec.Body.String()
			if !strings.Contains(body, `data-testid="`+tc.testid+`"`) {
				t.Errorf("missing data-testid %q in body: %s", tc.testid, body)
			}
			if !strings.Contains(body, `aria-label="Canal: `+tc.label+`"`) {
				t.Errorf("missing aria-label for %q", tc.label)
			}
			if !strings.Contains(body, "<svg") {
				t.Errorf("channel badge missing SVG output for channel %q", tc.channel)
			}
		})
	}
}

func TestView_RendersCustomerPanelOOBSwap(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: convID, Direction: "in", Body: "olá", Status: "delivered", CreatedAt: time.Now()},
	}}}
	loader := &stubCustomerInfo{out: webinbox.CustomerInfo{
		DisplayName: "Maria da Silva",
		Email:       "maria@example.com",
		Phone:       "11 99999-0000",
		Tags:        []string{"lead-quente", "premium"},
		Identities: []webinbox.CustomerIdentity{
			{Channel: "whatsapp", Handle: "11 99999-0000"},
			{Channel: "instagram", Handle: "@maria"},
		},
	}}

	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      msgs,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		CustomerInfo:      loader,
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}

	if !loader.called {
		t.Fatalf("CustomerInfo.Load not called")
	}
	if loader.tenantID != tenant || loader.convID != convID {
		t.Fatalf("loader args: tenant=%s conv=%s want %s/%s", loader.tenantID, loader.convID, tenant, convID)
	}

	body := rec.Body.String()
	// OOB-swap envelope + customer-card contents.
	for _, want := range []string{
		`id="inbox-customer-pane"`,
		`hx-swap-oob="outerHTML"`,
		`data-testid="customer-panel"`,
		`data-testid="customer-card"`,
		`data-testid="customer-identities"`,
		`Maria da Silva`,
		`maria@example.com`,
		`11 99999-0000`,
		`lead-quente`,
		`premium`,
		`@maria`,
		`data-testid="customer-summary"`,
		`data-testid="customer-tips"`,
		`data-testid="customer-actions"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("view body missing %q", want)
		}
	}
}

func TestView_RendersCustomerPanelDegradedWhenLoaderMissing(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{Items: nil}}
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="customer-panel"`) {
		t.Errorf("customer-panel missing from view body")
	}
	// Degraded copy when no contact data available
	if !strings.Contains(body, `Contato sem nome`) {
		t.Errorf("expected degraded display-name fallback in body: %s", body)
	}
	if strings.Contains(body, `data-testid="customer-identities"`) {
		t.Errorf("identities section must NOT render when loader is nil; body=%q", body)
	}
}

func TestView_CustomerInfoErrorDegradesGracefully(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{}}
	loader := &stubCustomerInfo{err: errors.New("boom")}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      msgs,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		CustomerInfo:      loader,
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Loader error must not 500; the panel still renders with the degraded copy.
	if !strings.Contains(body, `data-testid="customer-panel"`) {
		t.Errorf("customer-panel must still render when loader errors; body=%q", body)
	}
	if !strings.Contains(body, `Contato sem nome`) {
		t.Errorf("expected fallback display name when loader errors")
	}
}

func TestView_CustomerSummaryFallbackWhenAssistDisabled(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{}}
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="customer-summary-disabled"`) {
		t.Errorf("expected customer-summary-disabled fallback when assist not wired; body=%q", body)
	}
	if strings.Contains(body, `id="ai-assist-button"`) {
		t.Errorf("assist button must NOT render when summarizer is nil; body=%q", body)
	}
}

func TestStatusBubble_HasTitleTooltip(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: convID, Direction: "out", Body: "oi", Status: "delivered", CreatedAt: time.Now()},
	}}}
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	body := rec.Body.String()
	if !strings.Contains(body, `title="Entregue"`) {
		t.Errorf("expected status bubble tooltip via title attribute; body=%q", body)
	}
}

func TestComposeForm_HasHXIndicator(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{}}
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	body := rec.Body.String()
	if !strings.Contains(body, `hx-indicator="#compose-indicator"`) {
		t.Errorf("expected hx-indicator on compose form; body=%q", body)
	}
	if !strings.Contains(body, `id="compose-indicator"`) {
		t.Errorf("expected compose indicator anchor; body=%q", body)
	}
}
