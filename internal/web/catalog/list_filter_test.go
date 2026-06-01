package catalog_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/catalog"
	webcatalog "github.com/pericles-luz/crm/internal/web/catalog"
)

// helpers ----------------------------------------------------------------

func mkProductCat(t *testing.T, name, category string) *catalog.Product {
	t.Helper()
	p, err := catalog.NewProduct(uuid.New(), name, "", 0, nil,
		time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if category != "" {
		if err := p.SetCategory(category, time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("SetCategory: %v", err)
		}
	}
	return p
}

// ParseListFilter --------------------------------------------------------

func TestParseListFilter_Defaults(t *testing.T) {
	f := webcatalog.ExportedParseListFilter(httptest.NewRequest("GET", "http://x/catalog", nil))
	if f.Query != "" {
		t.Errorf("Query = %q, want \"\"", f.Query)
	}
	if f.Category != "" {
		t.Errorf("Category = %q, want \"\"", f.Category)
	}
}

func TestParseListFilter_TrimsAndExtracts(t *testing.T) {
	f := webcatalog.ExportedParseListFilter(httptest.NewRequest("GET",
		"http://x/catalog?q=%20mensal%20&category=%20Assinaturas%20", nil))
	if f.Query != "mensal" {
		t.Errorf("Query = %q, want %q", f.Query, "mensal")
	}
	if f.Category != "Assinaturas" {
		t.Errorf("Category = %q, want %q", f.Category, "Assinaturas")
	}
}

// ApplyListFilter --------------------------------------------------------

func TestApplyListFilter_Empty_PassesThrough(t *testing.T) {
	a := mkProductCat(t, "Plan A", "")
	b := mkProductCat(t, "Plan B", "Assinaturas")
	in := []*catalog.Product{a, b}
	got := webcatalog.ExportedApplyListFilter(in, webcatalog.ExportedListFilter{})
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
}

func TestApplyListFilter_QueryCaseInsensitive(t *testing.T) {
	a := mkProductCat(t, "Mensalidade", "")
	b := mkProductCat(t, "Plano Anual", "")
	got := webcatalog.ExportedApplyListFilter([]*catalog.Product{a, b}, webcatalog.ExportedListFilter{Query: "MENSAL"})
	if len(got) != 1 || got[0].Name() != "Mensalidade" {
		t.Errorf("got = %v, want [Mensalidade]", got)
	}
}

func TestApplyListFilter_Category_Exact(t *testing.T) {
	a := mkProductCat(t, "A", "Assinaturas")
	b := mkProductCat(t, "B", "Avulsos")
	got := webcatalog.ExportedApplyListFilter([]*catalog.Product{a, b}, webcatalog.ExportedListFilter{Category: "Avulsos"})
	if len(got) != 1 || got[0].Name() != "B" {
		t.Errorf("got = %v, want [B]", got)
	}
}

func TestApplyListFilter_Uncategorized(t *testing.T) {
	a := mkProductCat(t, "A", "")
	b := mkProductCat(t, "B", "Assinaturas")
	got := webcatalog.ExportedApplyListFilter([]*catalog.Product{a, b}, webcatalog.ExportedListFilter{Category: webcatalog.UncategorizedKey})
	if len(got) != 1 || got[0].Name() != "A" {
		t.Errorf("got = %v, want [A]", got)
	}
}

func TestApplyListFilter_NilEntriesSkipped(t *testing.T) {
	a := mkProductCat(t, "A", "")
	got := webcatalog.ExportedApplyListFilter([]*catalog.Product{nil, a, nil}, webcatalog.ExportedListFilter{})
	if len(got) != 1 || got[0].Name() != "A" {
		t.Errorf("got = %v, want [A]", got)
	}
}

func TestApplyListFilter_EmptyInput_ReturnsNil(t *testing.T) {
	if got := webcatalog.ExportedApplyListFilter(nil, webcatalog.ExportedListFilter{}); got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

// BuildCategoryCounts ----------------------------------------------------

func TestBuildCategoryCounts_GroupsAndSorts(t *testing.T) {
	in := []*catalog.Product{
		mkProductCat(t, "1", "B"),
		mkProductCat(t, "2", "A"),
		mkProductCat(t, "3", "B"),
		mkProductCat(t, "4", ""),
	}
	got := webcatalog.ExportedBuildCategoryCounts(in)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	// "A" first, "B" next, uncategorized last.
	if got[0].Key != "A" || got[0].Count != 1 {
		t.Errorf("got[0] = %+v, want A=1", got[0])
	}
	if got[1].Key != "B" || got[1].Count != 2 {
		t.Errorf("got[1] = %+v, want B=2", got[1])
	}
	if got[2].Key != webcatalog.UncategorizedKey || got[2].Label != "Sem categoria" || got[2].Count != 1 {
		t.Errorf("got[2] = %+v, want UncategorizedKey/1", got[2])
	}
}

func TestBuildCategoryCounts_EmptyInput(t *testing.T) {
	if got := webcatalog.ExportedBuildCategoryCounts(nil); got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestBuildCategoryCounts_AllUncategorized(t *testing.T) {
	got := webcatalog.ExportedBuildCategoryCounts([]*catalog.Product{mkProductCat(t, "1", "")})
	if len(got) != 1 || got[0].Key != webcatalog.UncategorizedKey {
		t.Errorf("got = %v, want one UncategorizedKey bucket", got)
	}
}

// list endpoint integration ---------------------------------------------

func TestList_FiltersByCategoryAndRendersSidebar(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	seedProductWithCategory(t, store, "Mensalidade", "Assinaturas")
	seedProductWithCategory(t, store, "Curso Aulao", "Cursos")
	seedProductWithCategory(t, store, "Brinde", "")
	mux := newHandler(t, store, resolverFromStore(t, store))

	// without filter — all three rows + 3 sidebar buckets
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog", nil))
	body := rr.Body.String()
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	for _, want := range []string{"Mensalidade", "Curso Aulao", "Brinde", "Assinaturas", "Cursos", "Sem categoria"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody=%s", want, body[:min(2000, len(body))])
		}
	}

	// filter to Cursos
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog?category=Cursos", nil))
	body = rr.Body.String()
	if !strings.Contains(body, "Curso Aulao") || strings.Contains(body, "<td><a href=\"/catalog/") && strings.Contains(body, "Mensalidade</a>") {
		t.Errorf("category=Cursos filter not applied; body=%s", body[:min(2000, len(body))])
	}

	// search by name
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog?q=brin", nil))
	body = rr.Body.String()
	if !strings.Contains(body, "Brinde") {
		t.Errorf("search q=brin should keep Brinde; body=%s", body[:min(2000, len(body))])
	}
	// Mensalidade row should be filtered out
	if strings.Contains(body, "Mensalidade</a>") {
		t.Errorf("search q=brin should drop Mensalidade")
	}
}

func TestList_UncategorizedFilter(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	seedProductWithCategory(t, store, "Sem cat", "")
	seedProductWithCategory(t, store, "Plano", "Assinaturas")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog?category="+webcatalog.UncategorizedKey, nil))
	body := rr.Body.String()
	if !strings.Contains(body, "Sem cat</a>") {
		t.Errorf("uncategorized filter should keep Sem cat; body=%s", body[:min(2000, len(body))])
	}
	if strings.Contains(body, "Plano</a>") {
		t.Errorf("uncategorized filter should drop Plano")
	}
}

// seed helper -----------------------------------------------------------

func seedProductWithCategory(t *testing.T, store *memStore, name, category string) *catalog.Product {
	t.Helper()
	p, err := catalog.NewProduct(testTenantID, name, "desc-"+name, 1000, nil,
		time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if category != "" {
		if err := p.SetCategory(category, time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("SetCategory: %v", err)
		}
	}
	if err := store.SaveProduct(t.Context(), p, testUserID); err != nil {
		t.Fatalf("SaveProduct: %v", err)
	}
	return p
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
