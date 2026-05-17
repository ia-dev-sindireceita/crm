package catalog_test

import (
	"context"
	"errors"
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

	"github.com/pericles-luz/crm/internal/catalog"
	"github.com/pericles-luz/crm/internal/tenancy"
	webcatalog "github.com/pericles-luz/crm/internal/web/catalog"
)

// memStore is the in-memory ProductReader+Writer+ArgumentReader+Writer
// the handler tests stack against. The four ports are implemented on
// one struct so a single store can drive both the read and write paths
// without bespoke wiring per test.
//
// memStore mirrors the cardinality / ordering contract the production
// pgx adapter exposes: products are sorted by created_at ASC, arguments
// by (scope_type, scope_id, created_at).
type memStore struct {
	mu sync.Mutex

	products  map[memProductKey]*catalog.Product
	arguments map[uuid.UUID]*catalog.ProductArgument

	// failures keyed by method name return the configured error so a
	// test can drive the handler's failure paths.
	getProductErr    error
	listProductsErr  error
	saveProductErr   error
	deleteProductErr error
	listArgsErr      error
	saveArgErr       error
	deleteArgErr     error
}

type memProductKey struct {
	tenant uuid.UUID
	id     uuid.UUID
}

func newMemStore() *memStore {
	return &memStore{
		products:  map[memProductKey]*catalog.Product{},
		arguments: map[uuid.UUID]*catalog.ProductArgument{},
	}
}

func (s *memStore) GetByID(_ context.Context, tenantID, productID uuid.UUID) (*catalog.Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getProductErr != nil {
		return nil, s.getProductErr
	}
	p, ok := s.products[memProductKey{tenant: tenantID, id: productID}]
	if !ok {
		return nil, catalog.ErrNotFound
	}
	return p, nil
}

func (s *memStore) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]*catalog.Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listProductsErr != nil {
		return nil, s.listProductsErr
	}
	out := make([]*catalog.Product, 0)
	for k, p := range s.products {
		if k.tenant == tenantID {
			out = append(out, p)
		}
	}
	// stable order by created_at, then id
	sortProductsByCreated(out)
	return out, nil
}

func (s *memStore) SaveProduct(_ context.Context, p *catalog.Product, _ uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveProductErr != nil {
		return s.saveProductErr
	}
	s.products[memProductKey{tenant: p.TenantID(), id: p.ID()}] = p
	return nil
}

func (s *memStore) DeleteProduct(_ context.Context, tenantID, productID, _ uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteProductErr != nil {
		return s.deleteProductErr
	}
	k := memProductKey{tenant: tenantID, id: productID}
	if _, ok := s.products[k]; !ok {
		return catalog.ErrNotFound
	}
	delete(s.products, k)
	// cascade arguments (mirrors FK ON DELETE CASCADE)
	for id, a := range s.arguments {
		if a.TenantID() == tenantID && a.ProductID() == productID {
			delete(s.arguments, id)
		}
	}
	return nil
}

func (s *memStore) ListByProduct(_ context.Context, tenantID, productID uuid.UUID) ([]*catalog.ProductArgument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listArgsErr != nil {
		return nil, s.listArgsErr
	}
	out := make([]*catalog.ProductArgument, 0)
	for _, a := range s.arguments {
		if a.TenantID() == tenantID && a.ProductID() == productID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *memStore) SaveArgument(_ context.Context, a *catalog.ProductArgument, _ uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveArgErr != nil {
		return s.saveArgErr
	}
	// emulate the (tenant_id, product_id, scope_type, scope_id) unique
	// constraint: a save that targets the same composite key as an
	// existing row (with a different id) yields ErrDuplicateArgument.
	for _, existing := range s.arguments {
		if existing.ID() == a.ID() {
			continue
		}
		if existing.TenantID() == a.TenantID() && existing.ProductID() == a.ProductID() &&
			existing.Anchor().Type == a.Anchor().Type && existing.Anchor().ID == a.Anchor().ID {
			return catalog.ErrDuplicateArgument
		}
	}
	s.arguments[a.ID()] = a
	return nil
}

func (s *memStore) DeleteArgument(_ context.Context, tenantID, argumentID, _ uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteArgErr != nil {
		return s.deleteArgErr
	}
	a, ok := s.arguments[argumentID]
	if !ok || a.TenantID() != tenantID {
		return catalog.ErrNotFound
	}
	delete(s.arguments, argumentID)
	return nil
}

func sortProductsByCreated(p []*catalog.Product) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j-1].CreatedAt().After(p[j].CreatedAt()); j-- {
			p[j-1], p[j] = p[j], p[j-1]
		}
	}
}

// failingResolver returns the configured err on every Resolve call.
type failingResolver struct{ err error }

func (f failingResolver) ResolveArguments(_ context.Context, _, _ uuid.UUID, _ catalog.Scope) ([]*catalog.ProductArgument, error) {
	return nil, f.err
}

// resolverFromStore wires the production *catalog.Resolver against the
// memStore so the cascade behavior in tests mirrors production.
func resolverFromStore(t *testing.T, store *memStore) *catalog.Resolver {
	t.Helper()
	return catalog.NewResolver(store)
}

const (
	testCSRFToken = "csrf-tok-xyz"
)

var (
	testTenantID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testUserID   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

func newHandler(t *testing.T, store *memStore, resolver webcatalog.ArgumentResolver) http.Handler {
	t.Helper()
	h, err := webcatalog.New(webcatalog.Deps{
		ProductReader:  store,
		ProductWriter:  store,
		ArgumentReader: store,
		ArgumentWriter: store,
		Resolver:       resolver,
		CSRFToken:      func(*http.Request) string { return testCSRFToken },
		UserID:         func(*http.Request) uuid.UUID { return testUserID },
		Now:            func() time.Time { return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC) },
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("webcatalog.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func tenantCtx() context.Context {
	tenant := &tenancy.Tenant{
		ID:   testTenantID,
		Name: "Acme CRM",
		Host: "acme.crm.local",
	}
	return tenancy.WithContext(context.Background(), tenant)
}

func newRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(tenantCtx(), method, target, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if method == http.MethodPost || method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req
}

func formBody(values url.Values) io.Reader {
	return strings.NewReader(values.Encode())
}

// seedProduct constructs a Product directly via the domain constructor,
// inserts it into the store, and returns it.
func seedProduct(t *testing.T, store *memStore, name string) *catalog.Product {
	t.Helper()
	p, err := catalog.NewProduct(testTenantID, name, "desc-"+name, 1000, []string{"tag1"}, time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if err := store.SaveProduct(context.Background(), p, testUserID); err != nil {
		t.Fatalf("SaveProduct: %v", err)
	}
	return p
}

func seedArgument(t *testing.T, store *memStore, productID uuid.UUID, scope catalog.ScopeType, scopeID, text string) *catalog.ProductArgument {
	t.Helper()
	a, err := catalog.NewProductArgument(testTenantID, productID,
		catalog.ScopeAnchor{Type: scope, ID: scopeID}, text,
		time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewProductArgument: %v", err)
	}
	if err := store.SaveArgument(context.Background(), a, testUserID); err != nil {
		t.Fatalf("SaveArgument: %v", err)
	}
	return a
}

// -----------------------------------------------------------------------------
// New / construction
// -----------------------------------------------------------------------------

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	resolver := resolverFromStore(t, store)
	full := webcatalog.Deps{
		ProductReader:  store,
		ProductWriter:  store,
		ArgumentReader: store,
		ArgumentWriter: store,
		Resolver:       resolver,
		CSRFToken:      func(*http.Request) string { return testCSRFToken },
		UserID:         func(*http.Request) uuid.UUID { return testUserID },
	}
	mutate := func(fn func(*webcatalog.Deps)) webcatalog.Deps {
		d := full
		fn(&d)
		return d
	}
	cases := []struct {
		name string
		deps webcatalog.Deps
	}{
		{"missing ProductReader", mutate(func(d *webcatalog.Deps) { d.ProductReader = nil })},
		{"missing ProductWriter", mutate(func(d *webcatalog.Deps) { d.ProductWriter = nil })},
		{"missing ArgumentReader", mutate(func(d *webcatalog.Deps) { d.ArgumentReader = nil })},
		{"missing ArgumentWriter", mutate(func(d *webcatalog.Deps) { d.ArgumentWriter = nil })},
		{"missing Resolver", mutate(func(d *webcatalog.Deps) { d.Resolver = nil })},
		{"missing CSRFToken", mutate(func(d *webcatalog.Deps) { d.CSRFToken = nil })},
		{"missing UserID", mutate(func(d *webcatalog.Deps) { d.UserID = nil })},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := webcatalog.New(c.deps); err == nil {
				t.Fatalf("New(%s) err = nil, want failure", c.name)
			}
		})
	}
}

func TestNew_DefaultsOptionalDeps(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	resolver := resolverFromStore(t, store)
	h, err := webcatalog.New(webcatalog.Deps{
		ProductReader:  store,
		ProductWriter:  store,
		ArgumentReader: store,
		ArgumentWriter: store,
		Resolver:       resolver,
		CSRFToken:      func(*http.Request) string { return "x" },
		UserID:         func(*http.Request) uuid.UUID { return uuid.New() },
		// Now + Logger intentionally nil to exercise defaults.
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("New returned nil handler")
	}
}

// -----------------------------------------------------------------------------
// GET /catalog
// -----------------------------------------------------------------------------

func TestList_RendersProductsAndAffordances(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Catálogo", "Acme CRM", "Mensalidade", `hx-get="/catalog/new"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\nbody=%s", want, body)
		}
	}
}

func TestList_EmptyShowsCTA(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Nenhum produto cadastrado") {
		t.Fatalf("empty-state copy missing\nbody=%s", rr.Body.String())
	}
}

func TestList_TenantMissing500s(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	req, err := http.NewRequest(http.MethodGet, "/catalog", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestList_ReadFailure500s(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.listProductsErr = errors.New("boom")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /catalog/new
// -----------------------------------------------------------------------------

func TestNewProductForm_RendersEmptyForm(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/new", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `hx-post="/catalog"`) {
		t.Fatalf("missing POST action\nbody=%s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Novo produto") {
		t.Fatalf("missing legend\nbody=%s", rr.Body.String())
	}
}

// -----------------------------------------------------------------------------
// POST /catalog
// -----------------------------------------------------------------------------

func TestCreateProduct_HappyPath(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))

	form := url.Values{}
	form.Set("name", "Plano Premium")
	form.Set("description", "tudo incluído")
	form.Set("price_cents", "9900")
	form.Set("tags", "ouro, mensal")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	listed, err := store.ListByTenant(context.Background(), testTenantID)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d products, want 1", len(listed))
	}
	got := listed[0]
	if got.Name() != "Plano Premium" {
		t.Fatalf("name = %q", got.Name())
	}
	if got.PriceCents() != 9900 {
		t.Fatalf("price = %d", got.PriceCents())
	}
	if tags := got.Tags(); len(tags) != 2 || tags[0] != "ouro" || tags[1] != "mensal" {
		t.Fatalf("tags = %v", tags)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Plano Premium") {
		t.Fatalf("list partial missing new product\nbody=%s", body)
	}
}

func TestCreateProduct_MissingName_422AndForm(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "  ")
	form.Set("price_cents", "100")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "nome do produto é obrigatório") {
		t.Fatalf("missing field error\nbody=%s", rr.Body.String())
	}
	if got, _ := store.ListByTenant(context.Background(), testTenantID); len(got) != 0 {
		t.Fatalf("got %d products, want 0", len(got))
	}
}

func TestCreateProduct_NegativePrice_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "x")
	form.Set("price_cents", "-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateProduct_NonIntegerPrice_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "x")
	form.Set("price_cents", "abc")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
}

func TestCreateProduct_OverLongName_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", strings.Repeat("a", 201))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
}

func TestCreateProduct_OverLongDescription_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "ok")
	form.Set("description", strings.Repeat("x", 2001))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
}

func TestCreateProduct_TooManyTags_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "ok")
	tags := make([]string, 33)
	for i := range tags {
		tags[i] = "t"
	}
	form.Set("tags", strings.Join(tags, ","))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
}

func TestCreateProduct_OverLongTag_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "ok")
	form.Set("tags", strings.Repeat("x", 65))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
}

func TestCreateProduct_NilActor_401(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	h, err := webcatalog.New(webcatalog.Deps{
		ProductReader:  store,
		ProductWriter:  store,
		ArgumentReader: store,
		ArgumentWriter: store,
		Resolver:       resolverFromStore(t, store),
		CSRFToken:      func(*http.Request) string { return testCSRFToken },
		UserID:         func(*http.Request) uuid.UUID { return uuid.Nil },
		Now:            func() time.Time { return time.Now().UTC() },
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	form := url.Values{}
	form.Set("name", "ok")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestCreateProduct_SaveFailure500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	store.saveProductErr = errors.New("pg down")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "ok")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog", formBody(form)))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /catalog/{id}
// -----------------------------------------------------------------------------

func TestDetail_RendersProductAndArguments(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	seedArgument(t, store, p.ID(), catalog.ScopeTenant, testTenantID.String(), "argumento tenant default")
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String(), nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Mensalidade", "argumento tenant default", "Cascade preview", "Tenant (padrão)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\nbody=%s", want, body)
		}
	}
}

func TestDetail_NotFound_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+uuid.NewString(), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDetail_InvalidUUID_400(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/not-a-uuid", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDetail_MissingCSRFToken_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	h, err := webcatalog.New(webcatalog.Deps{
		ProductReader:  store,
		ProductWriter:  store,
		ArgumentReader: store,
		ArgumentWriter: store,
		Resolver:       resolverFromStore(t, store),
		CSRFToken:      func(*http.Request) string { return "" },
		UserID:         func(*http.Request) uuid.UUID { return testUserID },
		Now:            func() time.Time { return time.Now().UTC() },
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String(), nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// TestListAndDetail_EmitCSRFMetaAndHXHeaders is the contract test for the
// CSRF surface the authed group's middleware (csrfmw) requires. Without
// the <meta name="csrf-token"> tag in <head> and the hx-headers attr on
// <body>, HTMX requests on /catalog* would 403 in production even though
// the unit tests bypass the router CSRF check.
func TestListAndDetail_EmitCSRFMetaAndHXHeaders(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))
	cases := []struct {
		name string
		path string
	}{
		{"list", "/catalog"},
		{"detail", "/catalog/" + p.ID().String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, newRequest(t, http.MethodGet, tc.path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			for _, want := range []string{
				`<meta name="csrf-token" content="` + testCSRFToken + `">`,
				`hx-headers='{"X-CSRF-Token": "` + testCSRFToken + `"}'`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
		})
	}
}

func TestList_MissingCSRFToken_500(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	h, err := webcatalog.New(webcatalog.Deps{
		ProductReader:  store,
		ProductWriter:  store,
		ArgumentReader: store,
		ArgumentWriter: store,
		Resolver:       resolverFromStore(t, store),
		CSRFToken:      func(*http.Request) string { return "" },
		UserID:         func(*http.Request) uuid.UUID { return testUserID },
		Now:            func() time.Time { return time.Now().UTC() },
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /catalog/{id}/edit + PATCH /catalog/{id}
// -----------------------------------------------------------------------------

func TestEditProductForm_RendersExisting(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/edit", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `hx-patch="/catalog/`+p.ID().String()+`"`) {
		t.Fatalf("missing patch action\nbody=%s", body)
	}
	if !strings.Contains(body, `value="Mensalidade"`) {
		t.Fatalf("missing pre-filled name\nbody=%s", body)
	}
}

func TestEditProductForm_NotFound_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+uuid.NewString()+"/edit", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestUpdateProduct_HappyPath(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))

	form := url.Values{}
	form.Set("name", "Mensalidade Pro")
	form.Set("description", "novo plano")
	form.Set("price_cents", "12000")
	form.Set("tags", "ouro")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+p.ID().String(), formBody(form)))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	got, err := store.GetByID(context.Background(), testTenantID, p.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name() != "Mensalidade Pro" {
		t.Fatalf("name = %q", got.Name())
	}
	if got.PriceCents() != 12000 {
		t.Fatalf("price = %d", got.PriceCents())
	}
	if got.CreatedAt() != p.CreatedAt() {
		t.Fatalf("created_at drifted on update: was %v, now %v", p.CreatedAt(), got.CreatedAt())
	}
}

func TestUpdateProduct_BlankName_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+p.ID().String(), formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
	got, _ := store.GetByID(context.Background(), testTenantID, p.ID())
	if got.Name() != "Mensalidade" {
		t.Fatalf("name mutated despite 422: %q", got.Name())
	}
}

func TestUpdateProduct_NotFound_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "x")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+uuid.NewString(), formBody(form)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestUpdateProduct_InvalidUUID_400(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("name", "x")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/oops", formBody(form)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// DELETE /catalog/{id}
// -----------------------------------------------------------------------------

func TestDeleteProduct_HappyPath(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "Mensalidade")
	mux := newHandler(t, store, resolverFromStore(t, store))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodDelete, "/catalog/"+p.ID().String(), nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if _, err := store.GetByID(context.Background(), testTenantID, p.ID()); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDeleteProduct_NotFound_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodDelete, "/catalog/"+uuid.NewString(), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDeleteProduct_InvalidUUID_400(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodDelete, "/catalog/oops", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// Argument CRUD
// -----------------------------------------------------------------------------

func TestNewArgumentForm_RendersScopeOptions(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/arguments/new", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{`value="tenant"`, `value="team"`, `value="channel"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing scope option %q\nbody=%s", want, body)
		}
	}
}

func TestCreateArgument_HappyPath_TenantScope(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", testTenantID.String())
	form.Set("argument_text", "argumento tenant default")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/"+p.ID().String()+"/arguments", formBody(form)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	args, err := store.ListByProduct(context.Background(), testTenantID, p.ID())
	if err != nil {
		t.Fatalf("ListByProduct: %v", err)
	}
	if len(args) != 1 {
		t.Fatalf("got %d args, want 1", len(args))
	}
	if args[0].Text() != "argumento tenant default" {
		t.Fatalf("text = %q", args[0].Text())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "argumento tenant default") {
		t.Fatalf("response missing argument text\nbody=%s", body)
	}
}

func TestCreateArgument_ChannelOverridesTenantInPreview(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	seedArgument(t, store, p.ID(), catalog.ScopeTenant, testTenantID.String(), "tenant default")
	mux := newHandler(t, store, resolverFromStore(t, store))

	form := url.Values{}
	form.Set("scope_type", "channel")
	form.Set("scope_id", "whatsapp")
	form.Set("argument_text", "override whatsapp")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/"+p.ID().String()+"/arguments", formBody(form)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d", rr.Code)
	}

	// Now request the preview with channel_id=whatsapp; resolver should
	// return the channel override (cascade specificity: channel > tenant).
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/preview?channel_id=whatsapp", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("preview status = %d", rr2.Code)
	}
	body := rr2.Body.String()
	if !strings.Contains(body, "override whatsapp") {
		t.Fatalf("preview missing channel argument\nbody=%s", body)
	}
	if !strings.Contains(body, "Canal (override mais específico)") {
		t.Fatalf("preview missing channel source label\nbody=%s", body)
	}
}

func TestCreateArgument_DuplicateScope_409(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	seedArgument(t, store, p.ID(), catalog.ScopeTenant, testTenantID.String(), "first")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", testTenantID.String())
	form.Set("argument_text", "second")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/"+p.ID().String()+"/arguments", formBody(form)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
}

func TestCreateArgument_InvalidScope_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("scope_type", "global")
	form.Set("scope_id", "x")
	form.Set("argument_text", "x")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/"+p.ID().String()+"/arguments", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCreateArgument_BlankText_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", "x")
	form.Set("argument_text", "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/"+p.ID().String()+"/arguments", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCreateArgument_BlankScopeID_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", " ")
	form.Set("argument_text", "x")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/"+p.ID().String()+"/arguments", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCreateArgument_OverLongText_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", "x")
	form.Set("argument_text", strings.Repeat("a", 4001))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/"+p.ID().String()+"/arguments", formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestCreateArgument_InvalidProductUUID_400(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", "x")
	form.Set("argument_text", "x")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPost, "/catalog/oops/arguments", formBody(form)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestEditArgumentForm_RendersExisting(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	a := seedArgument(t, store, p.ID(), catalog.ScopeChannel, "whatsapp", "texto")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/arguments/"+a.ID().String()+"/edit", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "whatsapp") || !strings.Contains(body, "texto") {
		t.Fatalf("missing pre-filled fields\nbody=%s", body)
	}
	if !strings.Contains(body, `hx-patch="/catalog/`+p.ID().String()+`/arguments/`+a.ID().String()+`"`) {
		t.Fatalf("missing patch action\nbody=%s", body)
	}
}

func TestEditArgumentForm_NotFound_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/arguments/"+uuid.NewString()+"/edit", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestUpdateArgument_HappyPath(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	a := seedArgument(t, store, p.ID(), catalog.ScopeTenant, testTenantID.String(), "antigo")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("argument_text", "novo texto")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+p.ID().String()+"/arguments/"+a.ID().String(), formBody(form)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	args, _ := store.ListByProduct(context.Background(), testTenantID, p.ID())
	if len(args) != 1 || args[0].Text() != "novo texto" {
		t.Fatalf("argument not updated: %+v", args)
	}
}

func TestUpdateArgument_BlankText_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	a := seedArgument(t, store, p.ID(), catalog.ScopeTenant, "x", "antigo")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("argument_text", "  ")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+p.ID().String()+"/arguments/"+a.ID().String(), formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
}

func TestUpdateArgument_OverLongText_422(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	a := seedArgument(t, store, p.ID(), catalog.ScopeTenant, "x", "antigo")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("argument_text", strings.Repeat("a", 4001))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+p.ID().String()+"/arguments/"+a.ID().String(), formBody(form)))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rr.Code)
	}
}

func TestUpdateArgument_NotFound_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("argument_text", "x")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+p.ID().String()+"/arguments/"+uuid.NewString(), formBody(form)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestUpdateArgument_InvalidArgUUID_400(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	form := url.Values{}
	form.Set("argument_text", "x")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodPatch, "/catalog/"+p.ID().String()+"/arguments/oops", formBody(form)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDeleteArgument_HappyPath(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	a := seedArgument(t, store, p.ID(), catalog.ScopeTenant, "x", "antigo")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodDelete, "/catalog/"+p.ID().String()+"/arguments/"+a.ID().String(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	args, _ := store.ListByProduct(context.Background(), testTenantID, p.ID())
	if len(args) != 0 {
		t.Fatalf("got %d args, want 0", len(args))
	}
}

func TestDeleteArgument_NotFound_404(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodDelete, "/catalog/"+p.ID().String()+"/arguments/"+uuid.NewString(), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// Preview
// -----------------------------------------------------------------------------

func TestPreview_NoArguments_RendersNoneSource(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/preview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Nenhum argumento configurado") {
		t.Fatalf("missing none-source label\nbody=%s", body)
	}
}

func TestPreview_TenantOnly(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	seedArgument(t, store, p.ID(), catalog.ScopeTenant, testTenantID.String(), "tenant arg")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/preview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "tenant arg") {
		t.Fatalf("missing tenant text\nbody=%s", body)
	}
	if !strings.Contains(body, "Tenant (padrão)") {
		t.Fatalf("missing tenant source label\nbody=%s", body)
	}
}

func TestPreview_ResolverError_FallsBackToNone(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	mux := newHandler(t, store, failingResolver{err: errors.New("boom")})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/preview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (preview never 500s)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Nenhum argumento configurado") {
		t.Fatalf("expected none-source fallback\nbody=%s", rr.Body.String())
	}
}

func TestPreview_InvalidProductUUID_400(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/oops/preview", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// SafeText / CSP-safe rendering
// -----------------------------------------------------------------------------

// TestSafeText_DescriptionEscapesHTML asserts the description column is
// rendered through html/template auto-escaping (no template.HTML
// bypass). The AC for SIN-62907 calls this out explicitly.
func TestSafeText_DescriptionEscapesHTML(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p, err := catalog.NewProduct(testTenantID, "x", `<script>alert("xss")</script>`, 0, nil, time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewProduct: %v", err)
	}
	if err := store.SaveProduct(context.Background(), p, testUserID); err != nil {
		t.Fatalf("SaveProduct: %v", err)
	}
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String(), nil))
	body := rr.Body.String()
	if strings.Contains(body, `<script>alert("xss")</script>`) {
		t.Fatalf("template did not escape description\nbody=%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("expected escaped <script>\nbody=%s", body)
	}
}

// TestSafeText_ArgumentTextEscapesHTML asserts argument_text is
// HTML-escaped on render.
func TestSafeText_ArgumentTextEscapesHTML(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	seedArgument(t, store, p.ID(), catalog.ScopeTenant, testTenantID.String(), `<img src=x onerror=alert(1)>`)
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String(), nil))
	body := rr.Body.String()
	if strings.Contains(body, `<img src=x onerror=alert(1)>`) {
		t.Fatalf("template did not escape argument text\nbody=%s", body)
	}
}

// TestNoInlineScriptTags asserts none of the templates ship an inline
// <script> tag. This is part of the CSP envelope (per doc.go) — adding
// any inline JS would silently break the strict CSP nonce policy.
func TestNoInlineScriptTags(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	seedArgument(t, store, p.ID(), catalog.ScopeTenant, testTenantID.String(), "ok")
	mux := newHandler(t, store, resolverFromStore(t, store))
	for _, target := range []string{
		"/catalog",
		"/catalog/new",
		"/catalog/" + p.ID().String(),
		"/catalog/" + p.ID().String() + "/edit",
		"/catalog/" + p.ID().String() + "/arguments/new",
		"/catalog/" + p.ID().String() + "/preview",
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, newRequest(t, http.MethodGet, target, nil))
		body := rr.Body.String()
		if strings.Contains(body, "<script") {
			t.Fatalf("%s: body has inline <script>\nbody=%s", target, body)
		}
	}
}

// -----------------------------------------------------------------------------
// Source-from-anchor coverage
// -----------------------------------------------------------------------------

func TestPreview_TeamSourceLabel(t *testing.T) {
	t.Parallel()
	store := newMemStore()
	p := seedProduct(t, store, "x")
	seedArgument(t, store, p.ID(), catalog.ScopeTeam, "team-A", "team arg")
	mux := newHandler(t, store, resolverFromStore(t, store))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, newRequest(t, http.MethodGet, "/catalog/"+p.ID().String()+"/preview?team_id=team-A", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Equipe (override)") {
		t.Fatalf("missing team label\nbody=%s", rr.Body.String())
	}
}

// -----------------------------------------------------------------------------
// FormError type round-trip
// -----------------------------------------------------------------------------

func TestFormError_ErrorIncludesFieldAndMessage(t *testing.T) {
	t.Parallel()
	e := &webcatalog.FormError{Field: "name", Message: "nope"}
	if got := e.Error(); got != "name: nope" {
		t.Fatalf("Error() = %q, want %q", got, "name: nope")
	}
}
