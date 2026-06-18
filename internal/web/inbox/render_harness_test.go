//go:build render_harness

package inbox_test

// Visual-truth render harness for SIN-65151 (compact metadata header +
// dominant thread). This is NOT a CI assertion: it is behind the
// `render_harness` build tag (so it never compiles into the normal test
// binary) and writes the real, server-rendered conversation view — light
// and dark — into RENDER_OUT so a headless Chrome pass can screenshot it
// at desktop (>=900px) and mobile (<600px) viewports.
//
// It reuses the production handler render path (GET
// /inbox/conversations/{id}) with the full context projection AND the
// interactive assignment widget wired, so the captured HTML is exactly
// what an operator sees — the metadata strip must stay compact while the
// message thread owns the column.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

func TestRenderHarness_ConversationView(t *testing.T) {
	outDir := os.Getenv("RENDER_OUT")
	if outDir == "" {
		t.Fatalf("set RENDER_OUT=<dir> to emit visual fixtures")
	}

	tenant, conv := uuid.New(), uuid.New()
	assigned := uuid.New()
	base := time.Date(2026, 6, 18, 14, 30, 0, 0, time.UTC)

	mkMsg := func(dir, body, status string, min int) inboxusecase.MessageView {
		return inboxusecase.MessageView{
			ID:             uuid.New(),
			ConversationID: conv,
			Direction:      dir,
			Body:           body,
			Status:         status,
			CreatedAt:      base.Add(time.Duration(min) * time.Minute),
		}
	}
	messages := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{
		mkMsg("in", "Olá, gostaria de saber o preço do plano anual.", "delivered", 0),
		mkMsg("out", "Oi, Maria! O plano anual sai por R$ 1.200, com 2 meses grátis.", "read", 1),
		mkMsg("in", "Perfeito. Tem desconto para pagamento à vista?", "delivered", 2),
		mkMsg("out", "Sim — 10% à vista. Quer que eu já gere o link de pagamento?", "sent", 3),
	}}}

	ctxUC := &fixedContext{view: inboxusecase.ConversationContextView{
		ConversationID:     conv,
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

	h, err := webinbox.New(webinbox.Deps{
		ListConversations:   &stubLister{},
		ListMessages:        messages,
		SendOutbound:        &stubSender{},
		GetMessage:          &stubGetMessage{},
		ConversationContext: ctxUC,
		AssignConversation:  &stubAssigner{},
		ListAssignable: &stubListAssignable{rows: []webinbox.AssignableRow{
			{UserID: assigned, DisplayName: "Ana Lima"},
			{UserID: uuid.New(), DisplayName: "Bruno Reis"},
		}},
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return assigned },
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
	pane := rec.Body.String()

	cssDir := filepath.Join(repoRootHarness(t), "web", "static", "css")
	tokens := readFileHarness(t, filepath.Join(cssDir, "tokens.css"))
	components := readFileHarness(t, filepath.Join(cssDir, "components.css"))
	inbox := readFileHarness(t, filepath.Join(cssDir, "inbox.css"))

	for _, theme := range []string{"light", "dark"} {
		doc := `<!doctype html><html lang="pt-BR" data-theme="` + theme + `"><head><meta charset="utf-8">` +
			`<meta name="viewport" content="width=device-width, initial-scale=1">` +
			`<style>` + tokens + "\n" + components + "\n" + inbox + `</style>` +
			`<style>html,body{margin:0}` +
			`.harness-pane{display:flex;flex-direction:column;height:100dvh;background:var(--surface-0);color:var(--text-default);font-family:var(--font-sans)}` +
			`</style></head><body>` +
			`<section class="inbox-conversation-pane harness-pane">` + pane + `</section>` +
			`</body></html>`
		path := filepath.Join(outDir, "conversation-"+theme+".html")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(doc))
	}
}

func repoRootHarness(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found from cwd")
		}
		dir = parent
	}
}

func readFileHarness(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
