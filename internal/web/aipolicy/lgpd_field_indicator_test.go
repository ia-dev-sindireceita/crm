package aipolicy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
)

// TestFieldTier_Red_AlwaysDisabled is SE regression test #1: every Red
// field renders with disabled + aria-disabled="true". Walks the
// /settings/ai-policy editor and confirms each Red name appears in
// the markup with both attributes on the <input>.
func TestFieldTier_Red_AlwaysDisabled(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, name := range aipolicy.LGPDRedFieldNames() {
		expectRedRowDisabled(t, body, name)
	}
}

// expectRedRowDisabled walks a chunk of rendered HTML and confirms the
// data-field row for name carries disabled + aria-disabled="true" on
// its <input>. Generous enough to survive minor whitespace deltas.
func expectRedRowDisabled(t *testing.T, body, name string) {
	t.Helper()
	// Locate the <li data-field="name" data-tier="red"> block.
	marker := `data-field="` + name + `"`
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Errorf("body missing data-field marker for %q", name)
		return
	}
	// Look for the end of the <li> for this field — the next "</li>".
	endIdx := strings.Index(body[idx:], "</li>")
	if endIdx < 0 {
		t.Errorf("unterminated <li> for %q", name)
		return
	}
	chunk := body[idx : idx+endIdx]
	if !strings.Contains(chunk, ` disabled`) {
		t.Errorf("Red field %q missing disabled attribute", name)
	}
	if !strings.Contains(chunk, `aria-disabled="true"`) {
		t.Errorf("Red field %q missing aria-disabled attribute", name)
	}
}

// TestCreatePolicy_RedFieldRejected is the gate on the POST side
// of SE regression test #1. The handler returns 422 with an
// aipolicy-form-error fragment naming structured_fields.
func TestCreatePolicy_RedFieldRejected(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", tenant.ID.String())
	form.Set("model", "claude-haiku")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	form.Set("anonymize", "on")
	form.Add("structured_fields", "email")
	form.Add("structured_fields", "cpf")

	req := newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-field="structured_fields"`) {
		t.Errorf("body missing structured_fields error marker:\n%s", body)
	}
	if !strings.Contains(body, "bloqueado por LGPD") {
		t.Errorf("body missing LGPD-block message:\n%s", body)
	}
}

// TestYellowFieldsOpen_BannerSticky is SE regression test #2: toggling
// at least one Yellow field surfaces the inline LGPD banner.
func TestYellowFieldsOpen_BannerSticky(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	// First, create a policy WITH a Yellow field opted-in.
	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", tenant.ID.String())
	form.Set("model", "gemini-flash")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	form.Set("ai_enabled", "on")
	form.Set("anonymize", "on")
	form.Add("structured_fields", "email")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	// Now fetch the edit form for that policy — banner must be present.
	editURL := "/settings/ai-policy/tenant/" + tenant.ID.String() + "/edit"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, editURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="lgpd-yellow-banner"`) {
		t.Errorf("Yellow-on edit form missing LGPD banner:\n%s", body)
	}

	// And a NEW form (no yellow fields) MUST NOT show the banner.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("new status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `id="lgpd-yellow-banner"`) {
		t.Errorf("new form should NOT show banner before any Yellow tick")
	}
}

// TestFreshFormDefaults_YellowOffGreenOnRedDisabled is SE regression
// test #6: a fresh newForm renders all-Yellow-OFF, all-Green-ON,
// all-Red-disabled.
func TestFreshFormDefaults_YellowOffGreenOnRedDisabled(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	for _, name := range aipolicy.LGPDYellowFieldNames() {
		chunk := extractFieldChunk(t, body, name)
		if strings.Contains(chunk, ` checked`) {
			t.Errorf("Yellow field %q rendered checked on fresh form", name)
		}
		if strings.Contains(chunk, ` disabled`) {
			t.Errorf("Yellow field %q should NOT be disabled", name)
		}
	}
	for _, name := range aipolicy.LGPDRedFieldNames() {
		chunk := extractFieldChunk(t, body, name)
		if !strings.Contains(chunk, ` disabled`) {
			t.Errorf("Red field %q must be disabled", name)
		}
	}
	for _, f := range aipolicy.LGPDFieldCatalog() {
		if f.Tier != aipolicy.TierGreen {
			continue
		}
		chunk := extractFieldChunk(t, body, f.Name)
		if !strings.Contains(chunk, ` checked`) {
			t.Errorf("Green field %q must render checked", f.Name)
		}
	}
}

// extractFieldChunk returns the <li ... data-field="name" ...> block.
func extractFieldChunk(t *testing.T, body, name string) string {
	t.Helper()
	marker := `data-field="` + name + `"`
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Errorf("missing data-field=%q in body", name)
		return ""
	}
	endIdx := strings.Index(body[idx:], "</li>")
	if endIdx < 0 {
		return ""
	}
	return body[idx : idx+endIdx]
}

// TestLGPDBanner_VerbatimText is SE regression test #7: the banner
// body matches the SE-authored verbatim PT-BR text. The snapshot
// rejects any drift.
func TestLGPDBanner_VerbatimText(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", tenant.ID.String())
	form.Set("model", "gemini-flash")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	form.Set("ai_enabled", "on")
	form.Set("anonymize", "on")
	form.Add("structured_fields", "email")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/tenant/"+tenant.ID.String()+"/edit", nil))
	body := rec.Body.String()

	// Verbatim fragments from lgpd-field-spec §"Banner text (PT-BR)".
	for _, fragment := range []string{
		"Você está habilitando o envio de PII para a LLM (OpenRouter).",
		"Os campos amarelos selecionados serão enviados",
		"tokenizados",
		"[PII:EMAIL]",
		"[PII:PHONE]",
		"[PII:CNPJ]",
		"a LLM nunca recebe o valor em texto claro",
		"LGPD Art. 5 I, Art. 7 II",
		"consentimento do controlador titular do dado",
		"Você pode revogar esta opção a qualquer momento",
		"OpenRouter conforme o DPA",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("banner missing verbatim fragment: %q", fragment)
		}
	}
}

// TestRedFieldTooltip_VerbatimText pins the verbatim Red-tier tooltip
// (lgpd-field-spec §"Static Red-tier explanatory tooltip").
func TestRedFieldTooltip_VerbatimText(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/new", nil))
	body := rec.Body.String()

	for _, fragment := range []string{
		"Campo bloqueado por LGPD Art. 5 II / Art. 11",
		"dado pessoal sensível",
		"Este campo nunca é enviado à LLM",
		"parecer jurídico e ADR",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("Red tooltip missing verbatim fragment: %q", fragment)
		}
	}
}

// TestPrecedenceEndpoint_NoSideEffects is SE residual risk R3: the
// precedence GET endpoint MUST NOT write to ai_policy_audit nor upsert
// any row. We assert no Upsert / Delete calls land on the repo across
// 100 hits — the same property the audit-log regression must show
// against the real DB in the adapter suite.
func TestPrecedenceEndpoint_NoSideEffects(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	// Seed a real policy so the resolver returns SourceTenant, not Default.
	repo.rows[memKey{tenant: tenant.ID, scopeType: aipolicy.ScopeTenant, scopeID: tenant.ID.String()}] = aipolicy.Policy{
		TenantID:         tenant.ID,
		ScopeType:        aipolicy.ScopeTenant,
		ScopeID:          tenant.ID.String(),
		Model:            "gemini-flash",
		PromptVersion:    "v1",
		Tone:             "neutro",
		Language:         "pt-BR",
		AIEnabled:        true,
		Anonymize:        true,
		OptIn:            true,
		StructuredFields: []string{"email"},
	}
	beforeRows := len(repo.rows)

	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/settings/ai-policy/precedence?mode=average", nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("precedence #%d status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if len(repo.rows) != beforeRows {
		t.Fatalf("preview wrote rows: before=%d after=%d", beforeRows, len(repo.rows))
	}
	// Also confirm the panel renders the tokenised preview line for the
	// Yellow opted-in email field. html/template HTML-escapes the literal
	// quotes inside <code>, so we check for the escaped form.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/precedence?mode=average", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "customer.email") || !strings.Contains(body, "[PII:EMAIL]") {
		t.Errorf("precedence panel missing tokenised email preview:\n%s", body)
	}
}

// TestPrecedenceEndpoint_EmptyState pins the "mode=conversation,
// no id" branch.
func TestPrecedenceEndpoint_EmptyState(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/precedence?mode=conversation&conversation_id=", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Informe um id de conversa") {
		t.Errorf("empty state missing instructional copy:\n%s", rec.Body.String())
	}
}

// TestPrecedenceEndpoint_RejectsMissingTenant exercises the 500 fail
// path for surface coverage.
func TestPrecedenceEndpoint_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/settings/ai-policy/precedence?mode=average", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestPrecedenceEndpoint_DefaultsToConversationMode covers the empty
// mode parameter branch.
func TestPrecedenceEndpoint_DefaultsToConversationMode(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/precedence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// no conversation_id + no explicit mode → empty state.
	if !strings.Contains(rec.Body.String(), "Informe um id de conversa") {
		t.Errorf("default mode should land on empty state:\n%s", rec.Body.String())
	}
}

// TestPrecedenceEndpoint_TokenizedPreviewExcludesRed pins the
// "Red fields never appear in the preview" invariant.
func TestPrecedenceEndpoint_TokenizedPreviewExcludesRed(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	repo.rows[memKey{tenant: tenant.ID, scopeType: aipolicy.ScopeTenant, scopeID: tenant.ID.String()}] = aipolicy.Policy{
		TenantID:  tenant.ID,
		ScopeType: aipolicy.ScopeTenant,
		ScopeID:   tenant.ID.String(),
		Model:     "gemini-flash",
		AIEnabled: true,
		Anonymize: true,
		// Try to slip cpf in via the slice — the preview must still skip it.
		StructuredFields: []string{"email"},
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/precedence?mode=average", nil))
	pre := extractCode(rec.Body.String())
	for _, redName := range []string{"cpf", "address", "health_data"} {
		if strings.Contains(pre, "customer."+redName) {
			t.Errorf("preview leaked Red field %q:\n%s", redName, pre)
		}
	}
}

// extractCode pulls the <code>...</code> block from the rendered
// precedence panel.
func extractCode(body string) string {
	open := strings.Index(body, "<code>")
	if open < 0 {
		return ""
	}
	body = body[open+len("<code>"):]
	end := strings.Index(body, "</code>")
	if end < 0 {
		return body
	}
	return body[:end]
}

// TestCreatePolicy_StoresStructuredFields confirms the form parser
// round-trips structured_fields into the persisted Policy.
func TestCreatePolicy_StoresStructuredFields(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", tenant.ID.String())
	form.Set("model", "gemini-flash")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	form.Set("ai_enabled", "on")
	form.Set("anonymize", "on")
	form.Add("structured_fields", "email")
	form.Add("structured_fields", "phone")

	req := newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	stored, ok := repo.rows[memKey{tenant: tenant.ID, scopeType: aipolicy.ScopeTenant, scopeID: tenant.ID.String()}]
	if !ok {
		t.Fatalf("policy not stored")
	}
	if len(stored.StructuredFields) != 2 {
		t.Fatalf("StructuredFields = %v, want [email phone]", stored.StructuredFields)
	}
}

// TestCreatePolicy_UnknownFieldRejected exercises the
// ErrUnknownStructuredField branch of the form parser.
func TestCreatePolicy_UnknownFieldRejected(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", tenant.ID.String())
	form.Set("model", "claude-haiku")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	form.Set("anonymize", "on")
	form.Add("structured_fields", "wat")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fora da allowlist") {
		t.Errorf("missing unknown-field error message: %s", rec.Body.String())
	}
}

var _ = uuid.Nil // keep the import stable when adapter shapes shift
