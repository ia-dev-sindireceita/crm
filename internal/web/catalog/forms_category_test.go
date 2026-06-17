package catalog_test

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProductForm_New_RendersCategoryField(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog/new", nil))

	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `name="category"`) {
		t.Errorf("new product form missing category field; body=%s", body)
	}
	if !strings.Contains(body, "maxlength=\"64\"") {
		t.Errorf("category field missing maxlength=64; body=%s", body)
	}
}

func TestProductForm_Edit_PrefillsCategory(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProductWithCategory(t, store, "Plan Pro", "Assinaturas")
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog/"+p.ID().String()+"/edit", nil))

	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `value="Assinaturas"`) {
		t.Errorf("edit form should prefill category; body=%s", body)
	}
}

func TestCreateProduct_PersistsCategory(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))

	form := url.Values{}
	form.Set("name", "Anual")
	form.Set("description", "Plano anual")
	form.Set("price_cents", "12000")
	form.Set("category", "Assinaturas")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "POST", "/catalog", formBody(form)))

	if rr.Code != 201 {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	// Confirm saved product carries category.
	products, err := store.ListByTenant(t.Context(), testTenantID)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(products) != 1 || products[0].Category() != "Assinaturas" {
		t.Errorf("saved category = %q, want %q", products[0].Category(), "Assinaturas")
	}
}

func TestCreateProduct_RejectsCategoryTooLong(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))

	form := url.Values{}
	form.Set("name", "P")
	form.Set("category", strings.Repeat("x", 65))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "POST", "/catalog", formBody(form)))
	if rr.Code != 422 {
		t.Errorf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `máximo 64 caracteres`) {
		t.Errorf("body should surface category-too-long error; got %s", rr.Body.String())
	}
}

func TestUpdateProduct_OverwritesCategory(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProductWithCategory(t, store, "Plan", "Assinaturas")
	mux := newHandler(t, store, resolverFromStore(t, store))

	form := url.Values{}
	form.Set("name", "Plan")
	form.Set("description", "desc")
	form.Set("price_cents", "0")
	form.Set("category", "Cursos")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "PATCH", "/catalog/"+p.ID().String(), formBody(form)))

	if rr.Code != 200 {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	// Confirm updated product carries new category.
	got, err := store.GetByID(t.Context(), testTenantID, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Category() != "Cursos" {
		t.Errorf("category = %q, want %q", got.Category(), "Cursos")
	}
}

func TestArgumentForm_New_EmbedsPromptPreview(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, "GET", "/catalog/"+p.ID().String()+"/arguments/new", nil))

	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="prompt-preview"`,
		`hx-get="/catalog/` + p.ID().String() + `/arguments/preview-prompt"`,
		`hx-trigger="keyup changed delay:300ms`,
		`hx-target="#prompt-preview"`,
		`data-role="system"`,
		`data-role="user"`,
		`data-role="argument"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("argument editor missing %q\nbody=%s", want, body)
		}
	}
}
