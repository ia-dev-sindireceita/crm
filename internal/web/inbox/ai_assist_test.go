package inbox_test

// SIN-62908 — tests for the AI-assist HTMX surface.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/pericles-luz/crm/internal/aiassist"
	aiassistusecase "github.com/pericles-luz/crm/internal/aiassist/usecase"
	"github.com/pericles-luz/crm/internal/aipolicy"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubSummarizer is a controllable AssistSummarizer.
type stubSummarizer struct {
	mu     sync.Mutex
	called bool
	in     aiassistusecase.SummarizeRequest
	out    *aiassistusecase.SummarizeResponse
	err    error
}

func (s *stubSummarizer) Summarize(_ context.Context, req aiassistusecase.SummarizeRequest) (*aiassistusecase.SummarizeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	s.in = req
	return s.out, s.err
}

// stubPolicy is a controllable AssistPolicyChecker.
type stubPolicy struct {
	enabled bool
	err     error
	calls   int
}

func (p *stubPolicy) IsEnabled(_ context.Context, _ uuid.UUID, _, _ string) (bool, error) {
	p.calls++
	return p.enabled, p.err
}

// stubArguments is a controllable AssistProductArgumentLister.
type stubArguments struct {
	out  []string
	err  error
	last struct {
		tenantID  uuid.UUID
		productID uuid.UUID
		channelID string
		teamID    string
	}
}

func (a *stubArguments) List(_ context.Context, tenantID, productID uuid.UUID, channelID, teamID string) ([]string, error) {
	a.last.tenantID = tenantID
	a.last.productID = productID
	a.last.channelID = channelID
	a.last.teamID = teamID
	return a.out, a.err
}

// newAssistHandler wires the full Deps surface plus AssistDeps. The
// fake summarizer pre-defines its return; tests override via the
// returned struct.
func newAssistHandler(
	t *testing.T,
	summarizer webinbox.AssistSummarizer,
	policy webinbox.AssistPolicyChecker,
	args webinbox.AssistProductArgumentLister,
	messages webinbox.ListMessagesUseCase,
	metrics *webinbox.AssistMetrics,
) *webinbox.Handler {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      messages,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "csrf-test-token" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer:     summarizer,
			Policy:         policy,
			Arguments:      args,
			Metrics:        metrics,
			RequestID:      func(*http.Request) string { return "req-test-1" },
			ProductID:      func(*http.Request) uuid.UUID { return uuid.Nil },
			MaxPromptChars: 1024,
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	return h
}

// assistPostReq builds a tenanted POST to /inbox/conversations/:id/ai-assist
// with the standard channelId / teamId form payload.
func assistPostReq(t *testing.T, tenantID, conversationID uuid.UUID, channelID, teamID string) *http.Request {
	t.Helper()
	body := strings.NewReader("channelId=" + channelID + "&teamId=" + teamID)
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+conversationID.String()+"/ai-assist", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID}))
	return r
}

// makeSummary builds an aiassist.Summary that returns the supplied
// text. Used by happy-path tests so newAssistPanel parses the
// suggestion markers.
func makeSummary(t *testing.T, tenantID, conversationID uuid.UUID, text string) *aiassist.Summary {
	t.Helper()
	s, err := aiassist.NewSummary(
		tenantID,
		conversationID,
		text,
		"openrouter/auto",
		120,
		60,
		time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		24*time.Hour,
	)
	if err != nil {
		t.Fatalf("aiassist.NewSummary: %v", err)
	}
	return s
}

func TestAIAssist_HappyPathRendersSummaryAndSuggestions(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	text := strings.Join([]string{
		"RESUMO: Cliente quer renegociar parcelas em atraso.",
		"SUGESTAO 1: Olá! Vamos rever as parcelas em atraso juntos?",
		"SUGESTAO 2: Posso oferecer um parcelamento em 12x sem juros.",
		"SUGESTAO 3: Confirma seu e-mail para enviarmos a proposta?",
	}, "\n")
	summary := makeSummary(t, tenant, conv, text)
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary, CacheHit: false}}
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{Direction: "in", Body: "Olá, estou com parcelas atrasadas."},
		{Direction: "out", Body: "Boa tarde! Pode me dizer quais parcelas?"},
	}}}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, messages, metrics)

	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "whatsapp", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Resumo da conversa",
		"Cliente quer renegociar parcelas em atraso.",
		"Sugestões",
		"Olá! Vamos rever as parcelas em atraso juntos?",
		"Confirma seu e-mail para enviarmos a proposta?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if !summarizer.called {
		t.Fatalf("summarizer not called")
	}
	if summarizer.in.TenantID != tenant {
		t.Errorf("tenant: got %s want %s", summarizer.in.TenantID, tenant)
	}
	if summarizer.in.ConversationID != conv {
		t.Errorf("conversation: got %s want %s", summarizer.in.ConversationID, conv)
	}
	if summarizer.in.RequestID == "" {
		t.Errorf("request id must not be empty")
	}
	if summarizer.in.Scope.ChannelID != "whatsapp" {
		t.Errorf("scope.channel: got %q want whatsapp", summarizer.in.Scope.ChannelID)
	}
	// prompt must include both messages so the LLM has full context.
	if !strings.Contains(summarizer.in.Prompt, "estou com parcelas atrasadas") {
		t.Errorf("prompt missing inbound message; prompt=%q", summarizer.in.Prompt)
	}
	if !strings.Contains(summarizer.in.Prompt, "Pode me dizer quais parcelas?") {
		t.Errorf("prompt missing outbound message; prompt=%q", summarizer.in.Prompt)
	}
	// Outcome histogram observed exactly once (ok).
	got := countSamples(t, metrics.Duration, "outcome", "ok")
	if got != 1 {
		t.Errorf("duration[ok]: got %d want 1", got)
	}
}

func TestAIAssist_RendersCacheHintWhenCacheHit(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summary := makeSummary(t, tenant, conv, "RESUMO: cache test.\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary, CacheHit: true}}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cache") {
		t.Errorf("cache hint missing from body")
	}
	got := countSamples(t, metrics.Duration, "outcome", "cache_hit")
	if got != 1 {
		t.Errorf("duration[cache_hit]: got %d want 1", got)
	}
}

func TestAIAssist_RendersInsufficientBalanceBanner(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summarizer := &stubSummarizer{err: aiassist.ErrInsufficientBalance}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status: got %d want 402", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Saldo de tokens esgotado") {
		t.Errorf("balance banner missing from body=%q", rec.Body.String())
	}
	if got := counterValue(t, metrics.Errors, "reason", "insufficient_balance"); got != 1 {
		t.Errorf("errors[insufficient_balance]: got %v want 1", got)
	}
}

func TestAIAssist_RendersPolicyDisabledAndSwapsButton(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summarizer := &stubSummarizer{err: aiassist.ErrAIDisabled}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"IA desabilitada neste canal",
		`id="ai-assist-button"`,
		"disabled",
		`hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if got := counterValue(t, metrics.Errors, "reason", "policy_disabled"); got != 1 {
		t.Errorf("errors[policy_disabled]: got %v want 1", got)
	}
}

func TestAIAssist_RendersRateLimitToast(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	// Construct the same multi-wrap the use case produces on rate-limit
	// deny so the handler sees ErrRateLimited + ErrLLMUnavailable.
	rateErr := errors.Join(aiassist.ErrLLMUnavailable, aiassist.ErrRateLimited)
	summarizer := &stubSummarizer{err: rateErr}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Aguarde 30s") {
		t.Errorf("rate-limit toast missing")
	}
	if got := counterValue(t, metrics.Errors, "reason", "rate_limited"); got != 1 {
		t.Errorf("errors[rate_limited]: got %v want 1", got)
	}
}

func TestAIAssist_RendersGenericUnavailableOnLLMError(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summarizer := &stubSummarizer{err: aiassist.ErrLLMUnavailable}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "IA temporariamente indisponível") {
		t.Errorf("unavailable banner missing")
	}
	if got := counterValue(t, metrics.Errors, "reason", "llm_unavailable"); got != 1 {
		t.Errorf("errors[llm_unavailable]: got %v want 1", got)
	}
}

func TestAIAssist_RendersInternalOnUnknownError(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summarizer := &stubSummarizer{err: errors.New("boom")}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	if got := counterValue(t, metrics.Errors, "reason", "internal"); got != 1 {
		t.Errorf("errors[internal]: got %v want 1", got)
	}
}

func TestAIAssist_AC7_ProductArgumentFoldedIntoPrompt(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	productID := uuid.New()
	args := &stubArguments{out: []string{
		"Plano Premium tem 30 dias de garantia integral",
		"Tenant fallback argument",
	}}
	summary := makeSummary(t, tenant, conv, "RESUMO: ok.\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary}}
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{Direction: "in", Body: "Quero entender o plano Premium."},
	}}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      messages,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer:     summarizer,
			Arguments:      args,
			RequestID:      func(*http.Request) string { return "req-ac7" },
			ProductID:      func(*http.Request) uuid.UUID { return productID },
			MaxPromptChars: 10_000,
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "whatsapp", "sales-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%q", rec.Code, rec.Body.String())
	}
	// AC #7 sanity: the first (most-specific) argument is in the prompt.
	if !strings.Contains(summarizer.in.Prompt, "Plano Premium tem 30 dias de garantia integral") {
		t.Errorf("product argument missing from prompt: %q", summarizer.in.Prompt)
	}
	// Arguments port received correct (tenant, product, scope) tuple.
	if args.last.productID != productID {
		t.Errorf("argument lister product: got %s want %s", args.last.productID, productID)
	}
	if args.last.channelID != "whatsapp" || args.last.teamID != "sales-a" {
		t.Errorf("argument lister scope: got (%q, %q)", args.last.channelID, args.last.teamID)
	}
}

func TestAIAssist_AC6_CacheMissAfterInvalidation(t *testing.T) {
	t.Parallel()
	// AC #6 covers the end-to-end behaviour "gerar resumo → nova
	// mensagem → re-pedir resumo → call OpenRouter novamente". From the
	// handler's perspective the boundary is: a fresh request lands the
	// handler with a Summarize that returns CacheHit=false. The wider
	// integration (NATS publisher → invalidator worker → next handler
	// call) is covered by the invalidator worker test.
	tenant, conv := uuid.New(), uuid.New()
	first := makeSummary(t, tenant, conv, "RESUMO: pre.\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	second := makeSummary(t, tenant, conv, "RESUMO: post.\nSUGESTAO 1: aa\nSUGESTAO 2: bb\nSUGESTAO 3: cc")
	summarizer := &scriptedSummarizer{responses: []scriptedSummarizerStep{
		{resp: &aiassistusecase.SummarizeResponse{Summary: first, CacheHit: true}},
		{resp: &aiassistusecase.SummarizeResponse{Summary: second, CacheHit: false}},
	}}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, assistPostReq(t, tenant, conv, "", ""))
	if rec1.Code != http.StatusOK || !strings.Contains(rec1.Body.String(), "pre.") {
		t.Fatalf("first call: status=%d body=%q", rec1.Code, rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), "cache") {
		t.Errorf("first call should advertise cache hit; body=%q", rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, assistPostReq(t, tenant, conv, "", ""))
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "post.") {
		t.Fatalf("second call: status=%d body=%q", rec2.Code, rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "cache") {
		t.Errorf("second call after invalidation must be cache_miss; body=%q", rec2.Body.String())
	}
	if got := countSamples(t, metrics.Duration, "outcome", "ok"); got != 1 {
		t.Errorf("duration[ok]: got %d want 1 (second call)", got)
	}
	if got := countSamples(t, metrics.Duration, "outcome", "cache_hit"); got != 1 {
		t.Errorf("duration[cache_hit]: got %d want 1 (first call)", got)
	}
}

func TestAIAssist_404WhenAssistNotWired(t *testing.T) {
	t.Parallel()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	// Route is not registered when AIAssist.Summarizer is nil. Go 1.22
	// ServeMux returns 404 for unmatched routes.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, uuid.New(), uuid.New(), "", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestAIAssist_4xxOnBadConversationID(t *testing.T) {
	t.Parallel()
	summarizer := &stubSummarizer{}
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	body := strings.NewReader("channelId=&teamId=")
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/not-a-uuid/ai-assist", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: uuid.New()}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestAIAssist_TenantRequired(t *testing.T) {
	t.Parallel()
	summarizer := &stubSummarizer{}
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	body := strings.NewReader("channelId=&teamId=")
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/ai-assist", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestAIAssist_404WhenConversationMissing(t *testing.T) {
	t.Parallel()
	summarizer := &stubSummarizer{}
	messages := &stubMessages{err: inboxusecase.ErrNotFound}
	h := newAssistHandler(t, summarizer, nil, nil, messages, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, uuid.New(), uuid.New(), "", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestView_RendersAssistButtonWhenAssistWired(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: conv, Direction: "in", Body: "hi", Status: "delivered"},
	}}}
	summarizer := &stubSummarizer{}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      messages,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer: summarizer,
			Policy:     &stubPolicy{enabled: true},
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
	body := rec.Body.String()
	for _, want := range []string{
		`id="ai-assist-button"`,
		"Resumir + sugerir 3 respostas",
		`hx-post="/inbox/conversations/` + conv.String() + `/ai-assist"`,
		`id="ai-assist-panel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestView_RendersDisabledButtonWhenPolicyDisabled(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	policy := &stubPolicy{enabled: false}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
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
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `disabled aria-disabled="true"`) {
		t.Errorf("disabled button missing; body=%q", body)
	}
	if !strings.Contains(body, "IA desabilitada neste canal") {
		t.Errorf("tooltip missing; body=%q", body)
	}
	if policy.calls != 1 {
		t.Errorf("policy.IsEnabled calls: got %d want 1", policy.calls)
	}
}

func TestView_OmitsAssistWhenSummarizerNil(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	r := httptest.NewRequest(http.MethodGet, "/inbox/conversations/"+conv.String(), nil)
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenant}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if strings.Contains(rec.Body.String(), "ai-assist-button") {
		t.Errorf("button must not render without summarizer")
	}
}

// scriptedSummarizer returns pre-set responses in order; later calls
// reuse the last response (so an over-long test sequence still
// returns something deterministic).
type scriptedSummarizerStep struct {
	resp *aiassistusecase.SummarizeResponse
	err  error
}

type scriptedSummarizer struct {
	mu        sync.Mutex
	responses []scriptedSummarizerStep
	idx       int
}

func (s *scriptedSummarizer) Summarize(_ context.Context, _ aiassistusecase.SummarizeRequest) (*aiassistusecase.SummarizeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.responses) == 0 {
		return nil, errors.New("scripted: no responses configured")
	}
	step := s.responses[s.idx]
	if s.idx < len(s.responses)-1 {
		s.idx++
	}
	return step.resp, step.err
}

// countSamples returns the sum count of histogram samples under the
// label name=val combination. The aggregation is by counting the
// `Histogram.SampleCount` reported via Prometheus DTO Collect.
func countSamples(t *testing.T, vec *prometheus.HistogramVec, label, val string) uint64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	var total uint64
	for m := range ch {
		var dtoMetric dto.Metric
		if err := m.Write(&dtoMetric); err != nil {
			t.Fatalf("metric.Write: %v", err)
		}
		matched := false
		for _, l := range dtoMetric.Label {
			if l.GetName() == label && l.GetValue() == val {
				matched = true
				break
			}
		}
		if matched && dtoMetric.Histogram != nil {
			total += dtoMetric.Histogram.GetSampleCount()
		}
	}
	return total
}

// counterValue returns the value of a CounterVec point for the given
// label name=val.
func counterValue(t *testing.T, vec *prometheus.CounterVec, label, val string) float64 {
	t.Helper()
	c, err := vec.GetMetricWith(prometheus.Labels{label: val})
	if err != nil {
		t.Fatalf("counter GetMetricWith: %v", err)
	}
	var dtoMetric dto.Metric
	if err := c.Write(&dtoMetric); err != nil {
		t.Fatalf("counter.Write: %v", err)
	}
	if dtoMetric.Counter == nil {
		return 0
	}
	return dtoMetric.Counter.GetValue()
}

// unused buffer guard so go vet doesn't complain about the import
// when none of the tests above use it.
var _ = bytes.Buffer{}

func TestAIAssist_PromptCapTrimsOldestMessages(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	// Build a long message list so the cap forces trimming. Each
	// message is ~100 chars; with 30 messages we comfortably exceed a
	// 500-char cap.
	items := make([]inboxusecase.MessageView, 0, 30)
	for i := 0; i < 30; i++ {
		body := "Mensagem número " + strings.Repeat("X", 100)
		dir := "in"
		if i%2 == 0 {
			dir = "out"
		}
		items = append(items, inboxusecase.MessageView{Direction: dir, Body: body})
	}
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: items}}
	summary := makeSummary(t, tenant, conv, "RESUMO: ok.\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      messages,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer:     summarizer,
			RequestID:      func(*http.Request) string { return "trim-req" },
			MaxPromptChars: 500,
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%q", rec.Code, rec.Body.String())
	}
	if len(summarizer.in.Prompt) > 500 {
		t.Errorf("prompt cap not honored: got %d chars want <=500", len(summarizer.in.Prompt))
	}
	if !strings.Contains(summarizer.in.Prompt, "RESUMO:") {
		t.Errorf("trimmed prompt must still include instruction header; prompt=%q", summarizer.in.Prompt)
	}
}

func TestAIAssist_PolicyResolverErrorFailsOpenButLogs(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	policy := &stubPolicy{err: errors.New("policy lookup boom")}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
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
		t.Fatalf("status: %d", rec.Code)
	}
	// Fail-open: enabled markup.
	if strings.Contains(rec.Body.String(), `disabled aria-disabled="true"`) {
		t.Errorf("policy lookup error must fail-open visually; body=%q", rec.Body.String())
	}
}

func TestAIAssist_ArgumentsListerErrorReturns500(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	productID := uuid.New()
	args := &stubArguments{err: errors.New("catalog boom")}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer: &stubSummarizer{},
			Arguments:  args,
			ProductID:  func(*http.Request) uuid.UUID { return productID },
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestAIAssist_RequestIDFallbackGeneratesValue(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summary := makeSummary(t, tenant, conv, "RESUMO: ok.\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary}}
	// RequestID hook returns empty → fallback path.
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer: summarizer,
			RequestID:  func(*http.Request) string { return "  " },
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(summarizer.in.RequestID, "assist-") {
		t.Errorf("fallback request id: got %q want assist-*", summarizer.in.RequestID)
	}
}

// TestAIAssist_PromptCapDefaultApplied verifies that when
// MaxPromptChars is zero the handler falls back to the default cap
// (200_000) — the prompt builder still runs and the request reaches
// the summarizer.
func TestAIAssist_PromptCapDefaultApplied(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summary := makeSummary(t, tenant, conv, "RESUMO: ok.\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer: summarizer,
			// MaxPromptChars left at zero on purpose.
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	// Generated fallback request id.
	if summarizer.in.RequestID == "" {
		t.Errorf("request id must not be empty under default cap path")
	}
}

func TestAIAssist_BadFormParse(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	summarizer := &stubSummarizer{}
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	// Malformed Content-Length without body produces a ParseForm error
	// path. Using percent-encoded broken input.
	body := strings.NewReader("channelId=%XX")
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/ai-assist", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenant}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestAIAssist_ListMessagesGenericErrorReturns500(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	messages := &stubMessages{err: errors.New("repo down")}
	h := newAssistHandler(t, &stubSummarizer{}, nil, nil, messages, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

// TestAIAssist_ConsentRequired_RendersModal exercises SIN-62929 from
// the inbox side: when the use-case returns *aiassist.ConsentRequired,
// the handler MUST render the consent modal partial with HX-Retarget
// pointing at the right-pane anchor (not the default #ai-assist-panel
// target), so HTMX swaps the dialog into view instead of replacing
// the summary tile.
func TestAIAssist_ConsentRequired_RendersModal(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	consentErr := &aiassist.ConsentRequired{
		Scope: aipolicy.ConsentScope{
			TenantID: tenant,
			Kind:     aipolicy.ScopeChannel,
			ID:       "whatsapp",
		},
		Payload:           "Olá ***, seu pedido foi atualizado.",
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v2",
	}
	summarizer := &stubSummarizer{err: consentErr}
	metrics := webinbox.NewAssistMetrics(nil)
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, metrics)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "whatsapp", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (HX-Retarget requires 2xx); body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("HX-Retarget"); got != "#ai-consent-modal" {
		t.Errorf("HX-Retarget = %q, want #ai-consent-modal", got)
	}
	if got := rec.Header().Get("HX-Reswap"); got != "outerHTML" {
		t.Errorf("HX-Reswap = %q, want outerHTML", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="ai-consent-modal"`,
		`role="dialog"`,
		`aria-modal="true"`,
		"Confirme o envio para o OpenRouter",
		`hx-post="/aipanel/consent/accept"`,
		`hx-post="/aipanel/consent/cancel"`,
		`name="scope_kind" value="channel"`,
		`name="scope_id" value="whatsapp"`,
		`name="anonymizer_version" value="anon-v1"`,
		`name="prompt_version" value="prompt-v2"`,
		`name="payload_preview"`,
		`name="payload_hash"`,
		`name="conversation_id" value="` + conv.String() + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("modal body missing %q\n--- body ---\n%s", want, body)
		}
	}
	if got := counterValue(t, metrics.Errors, "reason", "consent_required"); got != 1 {
		t.Errorf("errors[consent_required]: got %v want 1", got)
	}
}
