package catalog_test

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	webcatalog "github.com/pericles-luz/crm/internal/web/catalog"
)

func TestBuildPromptPreview_HappyPath(t *testing.T) {
	p := webcatalog.BuildPromptPreview("Mensalidade", "tenant", "acme", "Aposte no plano anual.")
	if p.ProductName != "Mensalidade" {
		t.Errorf("ProductName = %q, want %q", p.ProductName, "Mensalidade")
	}
	if p.ScopeLabel != "Tenant" {
		t.Errorf("ScopeLabel = %q, want %q", p.ScopeLabel, "Tenant")
	}
	if len(p.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(p.Segments))
	}
	roles := []string{p.Segments[0].Role, p.Segments[1].Role, p.Segments[2].Role}
	want := []string{"system", "user", "argument"}
	for i := range want {
		if roles[i] != want[i] {
			t.Errorf("segment[%d].Role = %q, want %q", i, roles[i], want[i])
		}
	}
	if !strings.Contains(p.Segments[2].Text, "Aposte no plano anual") {
		t.Errorf("argument segment lost text: %q", p.Segments[2].Text)
	}
}

func TestBuildPromptPreview_EmptyProductNameFallback(t *testing.T) {
	p := webcatalog.BuildPromptPreview("", "channel", "whatsapp", "x")
	if p.ProductName != "(produto sem nome)" {
		t.Errorf("ProductName = %q, want fallback", p.ProductName)
	}
}

func TestBuildPromptPreview_InvalidScopeRendersDash(t *testing.T) {
	p := webcatalog.BuildPromptPreview("Plan", "garbage", "id", "x")
	if p.ScopeLabel != "—" {
		t.Errorf("ScopeLabel = %q, want \"—\" for invalid scope_type", p.ScopeLabel)
	}
}

func TestBuildPromptPreview_EmptyArgumentShowsHint(t *testing.T) {
	p := webcatalog.BuildPromptPreview("Plan", "tenant", "acme", "")
	if !strings.Contains(p.Segments[2].Text, "argumento vazio") {
		t.Errorf("empty argument should render hint; got %q", p.Segments[2].Text)
	}
}

func TestBuildPromptPreview_TooLongArgumentTrims(t *testing.T) {
	long := strings.Repeat("a", webcatalog.MaxPreviewArgumentLen+50)
	p := webcatalog.BuildPromptPreview("Plan", "tenant", "acme", long)
	if len(p.Segments[2].Text) > webcatalog.MaxPreviewArgumentLen+1 {
		t.Errorf("argument segment longer than MaxPreviewArgumentLen+1 (newline trim slack): %d", len(p.Segments[2].Text))
	}
}

// Endpoint integration ---------------------------------------------------

func TestPreviewPromptEndpoint_ReturnsFragmentForKnownProduct(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))

	q := url.Values{}
	q.Set("scope_type", "tenant")
	q.Set("scope_id", "acme")
	q.Set("argument_text", "compre já")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog/"+p.ID().String()+"/arguments/preview-prompt?"+q.Encode(), nil))

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="prompt-preview"`,
		`data-role="system"`,
		`data-role="user"`,
		`data-role="argument"`,
		"Mensalidade",
		"compre já",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview body missing %q\nbody=%s", want, body)
		}
	}
}

func TestPreviewPromptEndpoint_UnknownProductStillRenders(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))

	// random uuid → ProductReader returns ErrNotFound → handler swallows
	// and still renders with empty product name fallback. This lets the
	// new-argument-form preview render before the row is persisted.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog/11111111-1111-1111-1111-111111111111/arguments/preview-prompt?scope_type=tenant&scope_id=acme&argument_text=x", nil))

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "(produto sem nome)") {
		t.Errorf("unknown product should fall back to '(produto sem nome)' label; body=%s", rr.Body.String())
	}
}

func TestPreviewPromptEndpoint_InvalidProductID_400(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog/not-a-uuid/arguments/preview-prompt", nil))
	if rr.Code != 400 {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestPreviewPromptEndpoint_TenantMissing_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Plan")
	mux := newHandler(t, store, resolverFromStore(t, store))

	// raw request (no tenancy context) → 500
	req := httptest.NewRequest("GET", "/catalog/"+p.ID().String()+"/arguments/preview-prompt", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}
