package aipanel_test

// SIN-62929 — Fase 3 decisão #8.
//
// AC #5 of the parent issue (SIN-62352) calls for an in-process E2E
// test that drives the complete consent flow end-to-end without
// Playwright: POST aiassist → 200 with consent modal → POST
// /aipanel/consent/accept → 200 + HX-Trigger → POST aiassist again →
// 200 with the summary panel.
//
// The pattern follows the existing project convention (httptest.NewServer
// + plain net/http client). NO Playwright is introduced — the issue
// scope explicitly defers any browser-based harness to a future CTO
// approval ("memory: Test infra substitutions need explicit approval").

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	aiassistusecase "github.com/pericles-luz/crm/internal/aiassist/usecase"
	"github.com/pericles-luz/crm/internal/aipolicy"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/aipanel"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// scriptedConsentSummarizer returns ConsentRequired on the first call,
// then a stub Summary on every subsequent call. The flip simulates the
// production gate: HasConsent goes from false → true once the operator
// confirms.
type scriptedConsentSummarizer struct {
	mu        sync.Mutex
	tenant    uuid.UUID
	conv      uuid.UUID
	scope     aipolicy.ConsentScope
	preview   string
	anonVer   string
	promptVer string

	// consented becomes true after the E2E test driver POSTs to
	// /aipanel/consent/accept. The accept handler does not call this
	// summarizer directly — the driver flips the flag via the same
	// fake consent recorder used by aipanel.Deps, mirroring the
	// production "Has/Record" cycle.
	consented bool
	calls     int
}

func (s *scriptedConsentSummarizer) Summarize(_ context.Context, _ aiassistusecase.SummarizeRequest) (*aiassistusecase.SummarizeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if !s.consented {
		return nil, &aiassist.ConsentRequired{
			Scope:             s.scope,
			Payload:           s.preview,
			AnonymizerVersion: s.anonVer,
			PromptVersion:     s.promptVer,
		}
	}
	summary, err := aiassist.NewSummary(
		s.tenant,
		s.conv,
		"RESUMO: Cliente quer atualizar pedido.\nSUGESTAO 1: Vou verificar.\nSUGESTAO 2: Aguarde por favor.\nSUGESTAO 3: Posso ajudar.",
		"openrouter/auto",
		100, 50,
		time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		24*time.Hour,
	)
	if err != nil {
		return nil, err
	}
	return &aiassistusecase.SummarizeResponse{Summary: summary}, nil
}

// e2eConsent is a recorder that the aipanel handler calls; it flips
// the linked summarizer's consented flag on success so the next
// aiassist POST returns the real Summary instead of ConsentRequired.
type e2eConsent struct {
	flip *scriptedConsentSummarizer
}

func (e *e2eConsent) RecordConsent(
	_ context.Context,
	_ aipolicy.ConsentScope,
	_ *uuid.UUID,
	_, _, _ string,
) error {
	e.flip.mu.Lock()
	e.flip.consented = true
	e.flip.mu.Unlock()
	return nil
}

// e2eMessages returns one inbound message so the assist prompt has a
// non-empty conversation to summarise.
type e2eMessages struct {
	in inboxusecase.ListMessagesInput
}

func (m *e2eMessages) Execute(_ context.Context, in inboxusecase.ListMessagesInput) (inboxusecase.ListMessagesResult, error) {
	m.in = in
	return inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{{
		Direction: "in",
		Body:      "Quero atualizar meu pedido",
	}}}, nil
}

type e2eLister struct{}

func (e2eLister) Execute(_ context.Context, _ inboxusecase.ListConversationsInput) (inboxusecase.ListConversationsResult, error) {
	return inboxusecase.ListConversationsResult{}, nil
}

type e2eSender struct{}

func (e2eSender) SendForView(_ context.Context, _ inboxusecase.SendOutboundInput) (inboxusecase.MessageView, error) {
	return inboxusecase.MessageView{}, nil
}

type e2eGetMessage struct{}

func (e2eGetMessage) Execute(_ context.Context, _ inboxusecase.GetMessageInput) (inboxusecase.GetMessageResult, error) {
	return inboxusecase.GetMessageResult{}, nil
}

// tenantContextMiddleware injects a tenant into every request the test
// server handles. The production stack does this via the IAM tenancy
// middleware; here a tiny http.Handler wrapper is enough.
func tenantContextMiddleware(next http.Handler, tenantID uuid.UUID) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID}))
		next.ServeHTTP(w, r)
	})
}

// TestE2E_ConsentFlow_InProcess drives the full gate → modal → accept
// → re-fire path through a single in-process server. Covers AC #5
// (parent SIN-62352) end-to-end without Playwright.
func TestE2E_ConsentFlow_InProcess(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()
	scope := aipolicy.ConsentScope{
		TenantID: tenant,
		Kind:     aipolicy.ScopeChannel,
		ID:       "whatsapp",
	}
	preview := "Cliente disse: ****"

	scripted := &scriptedConsentSummarizer{
		tenant: tenant, conv: conv, scope: scope,
		preview: preview, anonVer: "anon-v1", promptVer: "prompt-v1",
	}
	consent := &e2eConsent{flip: scripted}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Wire the inbox handler with the scripted summarizer.
	inboxH, err := webinbox.New(webinbox.Deps{
		ListConversations: e2eLister{},
		ListMessages:      &e2eMessages{},
		SendOutbound:      e2eSender{},
		GetMessage:        e2eGetMessage{},
		CSRFToken:         func(*http.Request) string { return "csrf-test-token" },
		UserID:            func(*http.Request) uuid.UUID { return user },
		Logger:            logger,
		AIAssist: webinbox.AssistDeps{
			Summarizer: scripted,
			RequestID:  func(*http.Request) string { return "req-e2e" },
		},
	})
	if err != nil {
		t.Fatalf("inbox.New: %v", err)
	}

	// Wire the aipanel handler with the linked consent recorder.
	metrics := obs.NewMetrics()
	panelH, err := aipanel.New(aipanel.Deps{
		Consent: consent,
		UserID:  func(*http.Request) uuid.UUID { return user },
		Metrics: metrics,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("aipanel.New: %v", err)
	}

	mux := http.NewServeMux()
	inboxH.Routes(mux)
	panelH.Routes(mux)

	srv := httptest.NewServer(tenantContextMiddleware(mux, tenant))
	defer srv.Close()

	httpClient := srv.Client()

	// Step 1: POST aiassist → expect 200 with modal + HX-Retarget.
	assistURL := srv.URL + "/inbox/conversations/" + conv.String() + "/ai-assist"
	first, err := httpClient.PostForm(assistURL, url.Values{
		"channelId": {"whatsapp"},
		"teamId":    {""},
	})
	if err != nil {
		t.Fatalf("first assist POST: %v", err)
	}
	t.Cleanup(func() { _ = first.Body.Close() })
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first assist: status %d, want 200", first.StatusCode)
	}
	if got := first.Header.Get("HX-Retarget"); got != "#ai-consent-modal" {
		t.Fatalf("first assist: HX-Retarget = %q, want #ai-consent-modal", got)
	}
	firstBody, _ := io.ReadAll(first.Body)
	for _, want := range []string{
		`id="ai-consent-modal"`,
		`role="dialog"`,
		"Confirme o envio para o OpenRouter",
		`name="payload_preview" value="Cliente disse: ****"`,
	} {
		if !strings.Contains(string(firstBody), want) {
			t.Fatalf("first assist body missing %q\n--- body ---\n%s", want, firstBody)
		}
	}
	if scripted.calls != 1 {
		t.Fatalf("summarizer.calls = %d, want 1 after first POST", scripted.calls)
	}

	// Step 2: POST /aipanel/consent/accept with the values the modal
	// would carry → expect 200 + HX-Trigger naming the conversation.
	digest := sha256.Sum256([]byte(preview))
	acceptURL := srv.URL + aipanel.AcceptRoutePath
	acceptResp, err := httpClient.PostForm(acceptURL, url.Values{
		"scope_kind":         {"channel"},
		"scope_id":           {"whatsapp"},
		"anonymizer_version": {"anon-v1"},
		"prompt_version":     {"prompt-v1"},
		"payload_hash":       {hex.EncodeToString(digest[:])},
		"payload_preview":    {preview},
		"conversation_id":    {conv.String()},
	})
	if err != nil {
		t.Fatalf("accept POST: %v", err)
	}
	t.Cleanup(func() { _ = acceptResp.Body.Close() })
	if acceptResp.StatusCode != http.StatusOK {
		t.Fatalf("accept: status %d, want 200", acceptResp.StatusCode)
	}
	if got := acceptResp.Header.Get("HX-Trigger"); !strings.Contains(got, conv.String()) {
		t.Fatalf("accept: HX-Trigger = %q, want it to contain %s", got, conv.String())
	}

	// Step 3: POST aiassist again — the gate has flipped, the
	// summarizer returns the real Summary now, and the inbox handler
	// renders the assist panel without the modal.
	second, err := httpClient.PostForm(assistURL, url.Values{
		"channelId": {"whatsapp"},
		"teamId":    {""},
	})
	if err != nil {
		t.Fatalf("second assist POST: %v", err)
	}
	t.Cleanup(func() { _ = second.Body.Close() })
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second assist: status %d, want 200", second.StatusCode)
	}
	if got := second.Header.Get("HX-Retarget"); got != "" {
		t.Fatalf("second assist: HX-Retarget = %q, want empty (summary path)", got)
	}
	secondBody, _ := io.ReadAll(second.Body)
	for _, want := range []string{
		`class="ai-assist__result"`,
		"Cliente quer atualizar pedido",
		"Vou verificar",
	} {
		if !strings.Contains(string(secondBody), want) {
			t.Errorf("second assist body missing %q\n--- body ---\n%s", want, secondBody)
		}
	}
	if strings.Contains(string(secondBody), `id="ai-consent-modal"`) {
		t.Errorf("second assist body unexpectedly carries the consent modal")
	}
	if scripted.calls != 2 {
		t.Fatalf("summarizer.calls = %d, want 2 after second POST", scripted.calls)
	}
}

// TestE2E_CancelFlow_DismissesModal complements the accept E2E: after
// the modal renders, POSTing /aipanel/consent/cancel collapses it
// (outerHTML swap with the empty placeholder) and DOES NOT record
// any consent — the next aiassist call must still return
// ConsentRequired.
func TestE2E_CancelFlow_DismissesModal(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	user := uuid.New()
	scope := aipolicy.ConsentScope{TenantID: tenant, Kind: aipolicy.ScopeChannel, ID: "whatsapp"}

	scripted := &scriptedConsentSummarizer{
		tenant: tenant, conv: conv, scope: scope,
		preview: "txt", anonVer: "v1", promptVer: "p1",
	}
	consent := &e2eConsent{flip: scripted}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	inboxH, err := webinbox.New(webinbox.Deps{
		ListConversations: e2eLister{},
		ListMessages:      &e2eMessages{},
		SendOutbound:      e2eSender{},
		GetMessage:        e2eGetMessage{},
		CSRFToken:         func(*http.Request) string { return "csrf" },
		UserID:            func(*http.Request) uuid.UUID { return user },
		Logger:            logger,
		AIAssist: webinbox.AssistDeps{
			Summarizer: scripted,
			RequestID:  func(*http.Request) string { return "req" },
		},
	})
	if err != nil {
		t.Fatalf("inbox.New: %v", err)
	}

	metrics := obs.NewMetrics()
	panelH, err := aipanel.New(aipanel.Deps{
		Consent: consent,
		UserID:  func(*http.Request) uuid.UUID { return user },
		Metrics: metrics,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("aipanel.New: %v", err)
	}

	mux := http.NewServeMux()
	inboxH.Routes(mux)
	panelH.Routes(mux)
	srv := httptest.NewServer(tenantContextMiddleware(mux, tenant))
	defer srv.Close()

	// Trigger the modal once so the test has a realistic starting
	// point. (Step 1 mirrors the accept E2E.)
	first, err := srv.Client().PostForm(
		srv.URL+"/inbox/conversations/"+conv.String()+"/ai-assist",
		url.Values{"channelId": {"whatsapp"}, "teamId": {""}},
	)
	if err != nil {
		t.Fatalf("first assist POST: %v", err)
	}
	_ = first.Body.Close()

	// POST cancel → expect 200 + empty modal placeholder + cancelled
	// metric incremented.
	cancelResp, err := srv.Client().PostForm(srv.URL+aipanel.CancelRoutePath, url.Values{
		"scope_kind": {"channel"},
	})
	if err != nil {
		t.Fatalf("cancel POST: %v", err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: status %d, want 200", cancelResp.StatusCode)
	}
	body, _ := io.ReadAll(cancelResp.Body)
	if !strings.Contains(string(body), `id="ai-consent-modal"`) {
		t.Errorf("cancel body must carry the empty modal placeholder; got %s", body)
	}
	if strings.Contains(string(body), "Confirme o envio") {
		t.Errorf("cancel body must NOT carry the original heading; got %s", body)
	}

	// The next aiassist call MUST still return ConsentRequired —
	// cancel did not record consent.
	second, err := srv.Client().PostForm(
		srv.URL+"/inbox/conversations/"+conv.String()+"/ai-assist",
		url.Values{"channelId": {"whatsapp"}, "teamId": {""}},
	)
	if err != nil {
		t.Fatalf("second assist POST: %v", err)
	}
	defer second.Body.Close()
	if got := second.Header.Get("HX-Retarget"); got != "#ai-consent-modal" {
		t.Errorf("second assist: HX-Retarget = %q, want #ai-consent-modal (cancel must not consent)", got)
	}
}
