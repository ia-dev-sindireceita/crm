package inbox_test

import (
	"context"
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

// stubSummaries is the enriched read-side fake (SIN-64968). It captures
// the input the handler builds from the query string + session so the
// filter-parsing assertions can inspect it, and returns a preconfigured
// row set / error.
type stubSummaries struct {
	mu     sync.Mutex
	in     inboxusecase.ListConversationSummariesInput
	called bool
	res    inboxusecase.ListConversationSummariesResult
	err    error
}

func (s *stubSummaries) Execute(_ context.Context, in inboxusecase.ListConversationSummariesInput) (inboxusecase.ListConversationSummariesResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

func (s *stubSummaries) input() inboxusecase.ListConversationSummariesInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.in
}

// newHandlerWithSummaries builds a handler with the enriched read side
// wired (so the rich list + filters path is exercised) plus a configurable
// session user id for the "minhas" filter.
func newHandlerWithSummaries(t *testing.T, summaries webinbox.ListSummariesUseCase, msgs webinbox.ListMessagesUseCase, userID uuid.UUID) *webinbox.Handler {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListSummaries:     summaries,
		ListMessages:      msgs,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "csrf-test-token" },
		UserID:            func(*http.Request) uuid.UUID { return userID },
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	return h
}

func strptr(s string) *string { return &s }

func TestList_RichRowRendersNameSnippetAndBadges(t *testing.T) {
	t.Parallel()
	convID := uuid.New()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID:                   convID,
			Channel:              "whatsapp",
			State:                "open",
			ContactDisplayName:   "Maria Silva",
			LastMessageSnippet:   "preciso de ajuda com o boleto",
			LastMessageDirection: "in",
			AwaitingReply:        true,
			AssignedUserLabel:    strptr("Joao Souza"),
			LastMessageAt:        time.Now().Add(-3 * time.Minute),
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Maria Silva",
		"preciso de ajuda com o boleto",
		"Aguardando resposta",
		"is-waiting",
		`title="Joao Souza"`,
		"JS", // assignee initials
		"Atribuída a Joao Souza",
		`class="inbox-filters"`,
		`id="conversation-list-region"`,
		"/inbox/conversations/" + convID.String(),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestList_StateFilterPillsAreKeyboardAccessible(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID: uuid.New(), Channel: "whatsapp", State: "open",
			ContactDisplayName: "Cliente", LastMessageAt: time.Now(),
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?state=open", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// WCAG 2.1.1: pills must be real links (focusable + no-JS fallback),
	// each carrying an href equal to its hx-get target. The active pill is
	// marked with aria-current, and the legacy role="button"/aria-pressed
	// (which made them non-focusable) must be gone.
	for _, want := range []string{
		`href="/inbox?state=open`,
		`href="/inbox?state=closed`,
		`href="/inbox?state=&channel=`,
		`aria-current="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filter pills missing %q: %q", want, body)
		}
	}
	for _, gone := range []string{`role="button"`, "aria-pressed"} {
		if strings.Contains(body, gone) {
			t.Errorf("filter pills still carry legacy %q: %q", gone, body)
		}
	}
}

func TestList_OutboundSnippetGetsVocePrefix(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID:                   uuid.New(),
			Channel:              "whatsapp",
			State:                "open",
			ContactDisplayName:   "Cliente",
			LastMessageSnippet:   "ok, obrigado",
			LastMessageDirection: "out",
			LastMessageAt:        time.Now(),
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if !strings.Contains(rec.Body.String(), "Você:") {
		t.Errorf("outbound snippet missing 'Você:' prefix: %q", rec.Body.String())
	}
}

func TestList_ClosedConversationRendersClosedBadge(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID:                 uuid.New(),
			Channel:            "webchat",
			State:              "closed",
			ContactDisplayName: "Fulano",
			LastMessageAt:      time.Now(),
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	// state=closed so the closed conversation is not filtered out by the
	// default open filter when the (stubbed) use case ignores filters.
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?state=closed", "", uuid.New()))
	body := rec.Body.String()
	if !strings.Contains(body, "state-badge--closed") || !strings.Contains(body, "Fechada") {
		t.Errorf("closed badge missing: %q", body)
	}
}

func TestList_UnassignedConversationRendersUnassignedChip(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID:                 uuid.New(),
			Channel:            "instagram",
			State:              "open",
			ContactDisplayName: "Sem Atendente",
			LastMessageAt:      time.Now(),
			// AssignedUserLabel nil → unassigned
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	body := rec.Body.String()
	if !strings.Contains(body, "assignee-chip--unassigned") || !strings.Contains(body, "Não atribuída") {
		t.Errorf("unassigned chip missing: %q", body)
	}
}

func TestList_FiltersParsedAndForwardedToUseCase(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	summaries := &stubSummaries{}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, userID)
	mux := http.NewServeMux()
	h.Routes(mux)

	tenant := uuid.New()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?state=closed&channel=whatsapp&assigned=me", "", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	in := summaries.input()
	if in.TenantID != tenant {
		t.Errorf("tenant: got %s want %s", in.TenantID, tenant)
	}
	if in.State != "closed" {
		t.Errorf("state: got %q want closed", in.State)
	}
	if in.Channel != "whatsapp" {
		t.Errorf("channel: got %q want whatsapp", in.Channel)
	}
	if in.AssignedUserID != userID {
		t.Errorf("assigned id: got %s want session user %s", in.AssignedUserID, userID)
	}
}

// TestList_UnassignedQueueForwardsUnassignedFilter pins the SIN-64979
// "visão de fila" queue: ?assigned=unassigned must forward Unassigned=true
// to the read-side use case with NO AssignedUserID (the use case rejects
// the combination), so the list shows only conversations with no lead.
func TestList_UnassignedQueueForwardsUnassignedFilter(t *testing.T) {
	t.Parallel()
	sessionUser := uuid.New()
	summaries := &stubSummaries{}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, sessionUser)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?assigned=unassigned", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	in := summaries.input()
	if !in.Unassigned {
		t.Error("Unassigned = false, want true forwarded for the fila queue")
	}
	if in.AssignedUserID != uuid.Nil {
		t.Errorf("AssignedUserID = %v, want Nil (mutually exclusive with unassigned)", in.AssignedUserID)
	}
}

// TestList_UnassignedQueueRendersSelectedOption proves the assignment
// queue <select> reflects the active queue (so the swap doesn't silently
// reset it) and keeps the "Não atribuídas" option present.
func TestList_UnassignedQueueRendersSelectedOption(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID: uuid.New(), Channel: "whatsapp", State: "open",
			ContactDisplayName: "Cliente", LastMessageAt: time.Now(),
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?assigned=unassigned", "", uuid.New()))
	body := rec.Body.String()
	if !strings.Contains(body, `<option value="unassigned" selected>`) {
		t.Errorf("unassigned option not marked selected: %q", body)
	}
	if !strings.Contains(body, "Não atribuídas") {
		t.Errorf("fila queue option missing: %q", body)
	}
}

func TestList_AssignedMeNeverTrustsClientID(t *testing.T) {
	t.Parallel()
	sessionUser := uuid.New()
	clientSpoof := uuid.New()
	summaries := &stubSummaries{}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, sessionUser)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	// A hand-crafted "assignedUserId" param must be ignored; the filter id
	// always comes from the session.
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?assigned=me&assignedUserId="+clientSpoof.String(), "", uuid.New()))
	if got := summaries.input().AssignedUserID; got != sessionUser {
		t.Errorf("assigned id: got %s want session %s (client spoof %s must be ignored)", got, sessionUser, clientSpoof)
	}
}

func TestList_StateDefaultsAndExplicitAll(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		target    string
		wantState string
	}{
		"absent defaults open":       {"/inbox", "open"},
		"explicit empty means all":   {"/inbox?state=", ""},
		"closed":                     {"/inbox?state=closed", "closed"},
		"invalid falls back to open": {"/inbox?state=bogus", "open"},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			summaries := &stubSummaries{}
			h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
			mux := http.NewServeMux()
			h.Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, tc.target, "", uuid.New()))
			if got := summaries.input().State; got != tc.wantState {
				t.Errorf("state: got %q want %q", got, tc.wantState)
			}
		})
	}
}

func TestList_UnknownChannelSanitizedToAll(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?channel=telegram", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := summaries.input().Channel; got != "" {
		t.Errorf("channel: got %q want empty (sanitized)", got)
	}
}

func TestList_HXRequestRendersPartialOnly(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID: uuid.New(), Channel: "whatsapp", State: "open",
			ContactDisplayName: "Alguem", LastMessageAt: time.Now(),
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	r := reqWithTenant(http.MethodGet, "/inbox?state=open", "", uuid.New())
	r.Header.Set("HX-Request", "true")
	mux.ServeHTTP(rec, r)
	body := rec.Body.String()
	if !strings.Contains(body, `id="conversation-list-region"`) {
		t.Errorf("partial missing region wrapper: %q", body)
	}
	if strings.Contains(body, "inbox-shell") || strings.Contains(body, "<!doctype") {
		t.Errorf("HX partial must not include the full shell: %q", body)
	}
}

func TestList_FullNavigationRendersShell(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	body := rec.Body.String()
	if !strings.Contains(body, "inbox-shell") || !strings.Contains(body, "<!doctype") {
		t.Errorf("full navigation must render the shell: %q", body)
	}
}

func TestList_FilteredEmptyStateOffersClearLink(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{Items: nil}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox?state=closed", "", uuid.New()))
	body := rec.Body.String()
	if !strings.Contains(body, "Nenhuma conversa com estes filtros.") {
		t.Errorf("filtered empty copy missing: %q", body)
	}
	if !strings.Contains(body, "Limpar filtros") {
		t.Errorf("clear-filters link missing: %q", body)
	}
}

func TestList_DefaultEmptyStateHasNoClearLink(t *testing.T) {
	t.Parallel()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{Items: nil}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	body := rec.Body.String()
	if !strings.Contains(body, "Nenhuma conversa.") {
		t.Errorf("default empty copy missing: %q", body)
	}
	if strings.Contains(body, "Limpar filtros") {
		t.Errorf("default empty must not offer clear link: %q", body)
	}
}

func TestView_RefreshesListRegionOOBWithActiveRow(t *testing.T) {
	t.Parallel()
	convID := uuid.New()
	summaries := &stubSummaries{res: inboxusecase.ListConversationSummariesResult{
		Items: []inboxusecase.ConversationView{{
			ID: convID, Channel: "whatsapp", State: "open",
			ContactDisplayName: "Ativo", LastMessageAt: time.Now(),
		}},
	}}
	h := newHandlerWithSummaries(t, summaries, &stubMessages{}, uuid.Nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String()+"?state=open", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="conversation-list-region"`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("OOB list region missing: %q", body)
	}
	if !strings.Contains(body, `aria-current="page"`) || !strings.Contains(body, "is-active") {
		t.Errorf("active row marker missing: %q", body)
	}
	// The OOB refresh must re-query under the active filter set.
	if got := summaries.input().State; got != "open" {
		t.Errorf("OOB refresh state: got %q want open", got)
	}
}

func TestView_NoOOBListWhenSummariesNotWired(t *testing.T) {
	t.Parallel()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{}}
	// Legacy handler (no ListSummaries) — view must not emit the OOB list.
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", uuid.New()))
	if strings.Contains(rec.Body.String(), `id="conversation-list-region"`) {
		t.Errorf("legacy view must not emit the OOB list region: %q", rec.Body.String())
	}
}
