package inbox_test

// SIN-65474 — tests for the "Atualizar" refresh control + staleness hint.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	aiassistusecase "github.com/pericles-luz/crm/internal/aiassist/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubSummaryReader is a controllable AssistSummaryReader.
type stubSummaryReader struct {
	at     time.Time
	exists bool
	err    error
	calls  int
}

func (s *stubSummaryReader) LatestSummaryGeneratedAt(_ context.Context, _, _ uuid.UUID) (time.Time, bool, error) {
	s.calls++
	return s.at, s.exists, s.err
}

// assistRefreshReq builds a force=1 ("Atualizar") POST.
func assistRefreshReq(t *testing.T, tenantID, conversationID uuid.UUID, channelID string) *http.Request {
	t.Helper()
	body := strings.NewReader("channelId=" + channelID + "&teamId=&force=1")
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+conversationID.String()+"/ai-assist", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID}))
	return r
}

// TestAIAssist_RefreshSetsForce covers AC #1: the force=1 POST flows
// through to SummarizeRequest.Force so the use case regenerates instead
// of serving the cache. The non-force POST leaves Force false.
func TestAIAssist_RefreshSetsForce(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summary := makeSummary(t, tenant, conv, "RESUMO: x\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary}}
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, webinbox.NewAssistMetrics(nil))
	mux := http.NewServeMux()
	h.Routes(mux)

	// Plain POST → Force false.
	mux.ServeHTTP(httptest.NewRecorder(), assistPostReq(t, tenant, conv, "whatsapp", ""))
	if summarizer.in.Force {
		t.Fatalf("plain POST must not set Force")
	}

	// force=1 POST → Force true.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistRefreshReq(t, tenant, conv, "whatsapp"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%q", rec.Code, rec.Body.String())
	}
	if !summarizer.in.Force {
		t.Fatalf("force=1 POST must set SummarizeRequest.Force")
	}
}

// TestAIAssist_PanelRendersRefreshControl covers AC #1 + AC #3: the
// success panel carries an "Atualizar" button that re-POSTs with force=1
// (plain hx-post, no inline JS), and an OOB swap that clears the
// staleness hint after regeneration.
func TestAIAssist_PanelRendersRefreshControl(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	summary := makeSummary(t, tenant, conv, "RESUMO: ok\nSUGESTAO 1: a\nSUGESTAO 2: b\nSUGESTAO 3: c")
	summarizer := &stubSummarizer{out: &aiassistusecase.SummarizeResponse{Summary: summary}}
	h := newAssistHandler(t, summarizer, nil, nil, &stubMessages{}, webinbox.NewAssistMetrics(nil))
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, assistPostReq(t, tenant, conv, "whatsapp", ""))
	body := rec.Body.String()

	for _, want := range []string{
		`id="ai-assist-refresh"`,
		"Atualizar",
		`hx-post="/inbox/conversations/` + conv.String() + `/ai-assist"`,
		`name="force" value="1"`,
		`id="ai-assist-staleness"`,
		`hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("panel missing %q\n--- body ---\n%s", want, body)
		}
	}
	// CSP guard: the refresh control must not introduce inline JS.
	for _, forbidden := range []string{"onclick", "hx-on:", "hx-on::", "javascript:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("panel must not contain inline handler %q", forbidden)
		}
	}
}

// TestView_RendersStalenessHintWhenNewerMessage covers AC #2: with a
// valid summary older than the newest message, the conversation view
// renders the "há novas mensagens" affordance — computed server-side, no
// client polling.
func TestView_RendersStalenessHintWhenNewerMessage(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	generated := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{Direction: "in", Body: "primeira", CreatedAt: generated.Add(-time.Hour)},
		{Direction: "in", Body: "mais nova", CreatedAt: generated.Add(time.Minute)}, // newer than the summary
	}}}
	reader := &stubSummaryReader{at: generated, exists: true}
	h := newViewHandlerWithReader(t, messages, reader)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, viewReq(t, tenant, conv))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Há novas mensagens desde o último resumo") {
		t.Errorf("staleness hint missing; body=%q", body)
	}
	if !strings.Contains(body, `data-testid="ai-assist-staleness"`) {
		t.Errorf("staleness hint testid missing")
	}
	if reader.calls != 1 {
		t.Errorf("reader calls: got %d want 1", reader.calls)
	}
}

// TestView_NoStalenessHintWhenSummaryFresh covers the negative: a
// summary newer than the newest message renders the hidden placeholder
// (so the OOB swap has a target) but no visible "novas mensagens" copy.
func TestView_NoStalenessHintWhenSummaryFresh(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	generated := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{Direction: "in", Body: "antiga", CreatedAt: generated.Add(-time.Hour)},
	}}}
	reader := &stubSummaryReader{at: generated, exists: true}
	h := newViewHandlerWithReader(t, messages, reader)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, viewReq(t, tenant, conv))
	body := rec.Body.String()
	if strings.Contains(body, "Há novas mensagens desde o último resumo") {
		t.Errorf("fresh summary must not render the staleness hint")
	}
	if !strings.Contains(body, `id="ai-assist-staleness"`) {
		t.Errorf("hidden staleness placeholder must still render for the OOB target")
	}
}

// TestView_NoStalenessHintWhenNoSummary covers the miss path: no valid
// summary → exists false → no hint regardless of message recency.
func TestView_NoStalenessHintWhenNoSummary(t *testing.T) {
	t.Parallel()
	tenant, conv := uuid.New(), uuid.New()
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		{Direction: "in", Body: "qualquer", CreatedAt: time.Now()},
	}}}
	reader := &stubSummaryReader{exists: false}
	h := newViewHandlerWithReader(t, messages, reader)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, viewReq(t, tenant, conv))
	if strings.Contains(rec.Body.String(), "Há novas mensagens desde o último resumo") {
		t.Errorf("no-summary path must not render the staleness hint")
	}
}

// newViewHandlerWithReader wires the conversation-view dependencies plus
// a SummaryReader for the staleness computation.
func newViewHandlerWithReader(t *testing.T, messages webinbox.ListMessagesUseCase, reader webinbox.AssistSummaryReader) *webinbox.Handler {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      messages,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		AIAssist: webinbox.AssistDeps{
			Summarizer:    &stubSummarizer{},
			SummaryReader: reader,
		},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	return h
}

func viewReq(t *testing.T, tenantID, conversationID uuid.UUID) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/inbox/conversations/"+conversationID.String(), nil)
	return r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID}))
}
