package inbox_test

// SIN-64970 — the conversation view must render a context side panel
// from the ConversationContextView projection (contact identity,
// channel, funnel stage, assignment) AND degrade gracefully when the
// contact, funnel stage, or assignment data is missing. The panel must
// also collapse to a single "indisponível" line when no context use
// case is wired at all.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// fixedContext returns a caller-supplied ConversationContextView (or an
// error) for any input. Separate from view_context_scope_test.go's
// stubConversationContext, which only carries the channel.
type fixedContext struct {
	view inboxusecase.ConversationContextView
	err  error
}

func (f *fixedContext) Execute(_ context.Context, _ inboxusecase.GetConversationContextInput) (inboxusecase.GetConversationContextResult, error) {
	if f.err != nil {
		return inboxusecase.GetConversationContextResult{}, f.err
	}
	return inboxusecase.GetConversationContextResult{Context: f.view}, nil
}

// renderView wires a handler with the given ConversationContext use case
// (nil-tolerant) and returns the rendered conversation-view body for a
// GET of one conversation. The assist feature is left off so these tests
// isolate the context panel; assist-enabled rendering is asserted by
// TestContextPanel_AssistEnabledRendersFunctionally below.
func renderView(t *testing.T, ctxUC webinbox.GetConversationContextUseCase) (int, string) {
	t.Helper()
	tenant, conv := uuid.New(), uuid.New()
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: conv, Direction: "in", Body: "olá", Status: "delivered"},
	}}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations:   &stubLister{},
		ListMessages:        messages,
		SendOutbound:        &stubSender{},
		GetMessage:          &stubGetMessage{},
		ConversationContext: ctxUC,
		CSRFToken:           func(*http.Request) string { return "tok" },
		UserID:              func(*http.Request) uuid.UUID { return uuid.Nil },
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
	return rec.Code, rec.Body.String()
}

// contextFragment slices out just the conversation-context <aside> so
// assertions don't collide with the right-rail customer panel (which
// carries its own "Contato sem nome" fallback). Returns "" when the
// panel is absent.
func contextFragment(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<aside class="conversation-context"`)
	if start < 0 {
		return ""
	}
	rest := body[start:]
	end := strings.Index(rest, "</aside>")
	if end < 0 {
		t.Fatalf("conversation-context aside not closed in body: %s", body)
	}
	return rest[:end+len("</aside>")]
}

func TestContextPanel_RendersFullData(t *testing.T) {
	t.Parallel()
	assigned := uuid.New()
	ctxUC := &fixedContext{view: inboxusecase.ConversationContextView{
		Channel:            "whatsapp",
		ContactDisplayName: "Maria Souza",
		ContactIdentities: []inboxusecase.ContactIdentityView{
			{Channel: "whatsapp", ExternalID: "+5511999998888"},
		},
		FunnelStageKey:  "negociacao",
		FunnelStageName: "Negociação",
		Assigned:        true,
		AssignedUserID:  &assigned,
	}}

	code, body := renderView(t, ctxUC)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", code, body)
	}
	frag := contextFragment(t, body)
	for _, want := range []string{
		`data-testid="conversation-context"`,
		`data-testid="conversation-context-contact"`,
		"Maria Souza",
		// digit substring survives html/template's escaping of the
		// leading "+" (→ &#43;), so the assertion stays escaping-agnostic.
		"5511999998888",
		`data-testid="conversation-context-funnel"`,
		"Negociação",
		`data-stage-key="negociacao"`,
		`data-testid="conversation-context-assignment"`,
		"Atribuída",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("full-data context panel missing %q\nfragment=%s", want, frag)
		}
	}
	// The full-data case must not render any of the degraded fallbacks.
	for _, unwanted := range []string{"Contato sem nome", "Sem etapa definida", "Não atribuída", "Contexto indisponível"} {
		if strings.Contains(frag, unwanted) {
			t.Errorf("full-data context panel unexpectedly rendered fallback %q\nfragment=%s", unwanted, frag)
		}
	}
}

func TestContextPanel_DegradesOnMissingContactAndStage(t *testing.T) {
	t.Parallel()
	// Empty contact, no identities, no funnel transition, unassigned —
	// every optional block must collapse to its fallback without breaking
	// the layout.
	ctxUC := &fixedContext{view: inboxusecase.ConversationContextView{
		Channel: "instagram",
	}}

	code, body := renderView(t, ctxUC)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", code, body)
	}
	frag := contextFragment(t, body)
	for _, want := range []string{
		`data-testid="conversation-context"`,
		"Contato sem nome",
		"Sem etapa definida",
		"Não atribuída",
		// channel block still renders the real channel label
		"Instagram",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("degraded context panel missing %q\nfragment=%s", want, frag)
		}
	}
	// No identity list should render when the contact has no identities.
	if strings.Contains(frag, `class="conversation-context__identities"`) {
		t.Errorf("degraded panel rendered an identities list with no identities\nfragment=%s", frag)
	}
}

func TestContextPanel_NoUseCaseRendersUnavailable(t *testing.T) {
	t.Parallel()
	// No ConversationContext dep wired → HasContext stays false → the
	// panel collapses to the single "indisponível" line rather than a
	// half-empty card.
	code, body := renderView(t, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", code, body)
	}
	if !strings.Contains(body, `data-testid="conversation-context-empty"`) {
		t.Errorf("nil-usecase panel missing empty state\nbody=%s", body)
	}
	if !strings.Contains(body, "Contexto indisponível") {
		t.Errorf("nil-usecase panel missing indisponível copy\nbody=%s", body)
	}
}

func TestContextPanel_ReadErrorRendersUnavailable(t *testing.T) {
	t.Parallel()
	// A failing context read degrades to the unavailable state (the pane
	// itself still renders — ListMessages already 404s a missing conv).
	ctxUC := &fixedContext{err: errors.New("storage down")}
	code, body := renderView(t, ctxUC)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", code, body)
	}
	if !strings.Contains(body, "Contexto indisponível") {
		t.Errorf("read-error panel missing indisponível copy\nbody=%s", body)
	}
}

// TestContextPanel_FunnelStageRendersAsPill is the SIN-65156 refinement:
// the funnel stage is no longer a plain paragraph but a bordered pill chip.
// The stage name text is always present (WCAG colour-independence — any
// data-stage-key colouring is reinforcement only) and the data-stage-key
// hook is preserved.
func TestContextPanel_FunnelStageRendersAsPill(t *testing.T) {
	t.Parallel()
	ctxUC := &fixedContext{view: inboxusecase.ConversationContextView{
		Channel:         "whatsapp",
		FunnelStageKey:  "negociacao",
		FunnelStageName: "Negociação",
	}}
	_, body := renderView(t, ctxUC)
	frag := contextFragment(t, body)
	for _, want := range []string{
		`class="conversation-context__funnel-pill"`,
		`data-stage-key="negociacao"`,
		"Negociação",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("funnel pill missing %q\nfragment=%s", want, frag)
		}
	}
	// The old plain-paragraph funnel classes are replaced by the pill.
	for _, gone := range []string{
		`class="conversation-context__funnel-stage"`,
		`class="conversation-context__funnel-empty"`,
	} {
		if strings.Contains(frag, gone) {
			t.Errorf("stale funnel paragraph class %q still rendered\nfragment=%s", gone, frag)
		}
	}
}

// TestContextPanel_FunnelEmptyRendersAsMutedPill confirms the "no stage"
// state reuses the same pill shape (constant strip height) with the muted
// modifier rather than a differently-shaped paragraph.
func TestContextPanel_FunnelEmptyRendersAsMutedPill(t *testing.T) {
	t.Parallel()
	ctxUC := &fixedContext{view: inboxusecase.ConversationContextView{Channel: "instagram"}}
	_, body := renderView(t, ctxUC)
	frag := contextFragment(t, body)
	if !strings.Contains(frag, "conversation-context__funnel-pill--empty") {
		t.Errorf("empty funnel missing muted pill modifier\nfragment=%s", frag)
	}
	if !strings.Contains(frag, "Sem etapa definida") {
		t.Errorf("empty funnel missing copy\nfragment=%s", frag)
	}
}

// TestContextPanel_AssignmentCollapsedInDetails is the SIN-65156 refinement:
// the assignment editor (select + buttons) is collapsed behind a native
// <details>/<summary> so the compact metadata strip is not dominated by the
// form. The assignee chip + name stay visible above the disclosure, and the
// SIN-64979 swap contract (section id + hx-target/hx-swap=outerHTML) MUST be
// preserved exactly so the assign POST keeps swapping in place. The
// disclosure is pure markup — no inline event handlers (CSP-strict).
func TestContextPanel_AssignmentCollapsedInDetails(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	me, assigned := uuid.New(), uuid.New()
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: conv, Direction: "in", Body: "olá", Status: "delivered"},
	}}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      messages,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		ConversationContext: &fixedContext{view: inboxusecase.ConversationContextView{
			Channel:        "whatsapp",
			Assigned:       true,
			AssignedUserID: &assigned,
		}},
		ListAssignable: &stubListAssignable{rows: []webinbox.AssignableRow{
			{UserID: assigned, DisplayName: "Ana Lima"},
			{UserID: me, DisplayName: "Eu Mesmo"},
		}},
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return me },
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
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	frag := contextFragment(t, rec.Body.String())
	for _, want := range []string{
		`<details class="conversation-context__assign-details"`,
		`<summary class="conversation-context__assign-summary">Alterar</summary>`,
		// SIN-64979 swap contract — must survive the <details> wrapping.
		`id="conversation-context-assignment"`,
		`hx-target="#conversation-context-assignment"`,
		`hx-swap="outerHTML"`,
		// The assignee identity stays visible above the disclosure.
		"Ana Lima",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("collapsed assignment missing %q\nfragment=%s", want, frag)
		}
	}
	// CSP-strict: the disclosure is pure server markup — no inline handlers.
	for _, banned := range []string{"hx-on", " onclick", " ontoggle="} {
		if strings.Contains(frag, banned) {
			t.Errorf("assignment disclosure leaked inline handler %q\nfragment=%s", banned, frag)
		}
	}
}

// TestContextPanel_AssistEnabledRendersFunctionally is the deliverable-3
// confirmation: with AssistDeps.Summarizer wired (+ policy enabled) the
// view renders the assist button, the #ai-assist-panel swap target, and
// the #ai-consent-modal on-demand anchor — and the anchor no longer
// carries the removed --placeholder misnomer class.
func TestContextPanel_AssistEnabledRendersFunctionally(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{ID: uuid.New(), ConversationID: conv, Direction: "in", Body: "oi", Status: "delivered"},
	}}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations:   &stubLister{},
		ListMessages:        messages,
		SendOutbound:        &stubSender{},
		GetMessage:          &stubGetMessage{},
		ConversationContext: &fixedContext{view: inboxusecase.ConversationContextView{Channel: "whatsapp"}},
		CSRFToken:           func(*http.Request) string { return "tok" },
		UserID:              func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer: &stubSummarizer{},
			Policy:     &recordingPolicy{enabled: true},
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
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="ai-assist-button"`,
		`id="ai-assist-panel"`,
		`id="ai-consent-modal"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("assist-enabled view missing %q", want)
		}
	}
	// The enabled button must be a real submit (not the disabled variant).
	if strings.Contains(body, `ai-assist__button--disabled`) {
		t.Errorf("assist button rendered disabled despite policy enabled\nbody=%s", body)
	}
	// The misnomer class is gone (deliverable 3): the anchor is just an
	// on-demand swap target, not a "placeholder modal".
	if strings.Contains(body, "ai-consent-modal--placeholder") {
		t.Errorf("removed --placeholder misnomer still present in body")
	}
}
