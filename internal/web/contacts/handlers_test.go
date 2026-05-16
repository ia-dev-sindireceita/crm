package contacts_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	webcontacts "github.com/pericles-luz/crm/internal/web/contacts"
)

// stubLoad captures the LoadIdentityForContact call args and returns a
// preconfigured result/error.
type stubLoad struct {
	mu     sync.Mutex
	in     contactsusecase.LoadIdentityInput
	called bool
	res    contactsusecase.LoadIdentityResult
	err    error
}

func (s *stubLoad) Execute(_ context.Context, in contactsusecase.LoadIdentityInput) (contactsusecase.LoadIdentityResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

// stubSplit captures the SplitIdentityLink call args.
type stubSplit struct {
	mu     sync.Mutex
	in     contactsusecase.SplitInput
	called bool
	res    contactsusecase.SplitResult
	err    error
}

func (s *stubSplit) Execute(_ context.Context, in contactsusecase.SplitInput) (contactsusecase.SplitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

func newHandler(t *testing.T, load *stubLoad, split *stubSplit) *webcontacts.Handler {
	t.Helper()
	h, err := webcontacts.New(webcontacts.Deps{
		LoadIdentity: load,
		SplitLink:    split,
		CSRFToken:    func(*http.Request) string { return "csrf-test-token" },
	})
	if err != nil {
		t.Fatalf("webcontacts.New: %v", err)
	}
	return h
}

func reqWithTenant(method, target, body string, tenantID uuid.UUID) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	tenant := &tenancy.Tenant{ID: tenantID}
	return r.WithContext(tenancy.WithContext(r.Context(), tenant))
}

func reqNoTenant(method, target, body string) *http.Request {
	if body == "" {
		return httptest.NewRequest(method, target, nil)
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestNew_RequiresAllDeps(t *testing.T) {
	t.Parallel()
	full := webcontacts.Deps{
		LoadIdentity: &stubLoad{},
		SplitLink:    &stubSplit{},
		CSRFToken:    func(*http.Request) string { return "tok" },
	}
	if _, err := webcontacts.New(full); err != nil {
		t.Fatalf("New(full): %v", err)
	}
	cases := map[string]webcontacts.Deps{
		"missing LoadIdentity": {SplitLink: full.SplitLink, CSRFToken: full.CSRFToken},
		"missing SplitLink":    {LoadIdentity: full.LoadIdentity, CSRFToken: full.CSRFToken},
		"missing CSRFToken":    {LoadIdentity: full.LoadIdentity, SplitLink: full.SplitLink},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := webcontacts.New(deps); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func twoLinkIdentity(survivorContact uuid.UUID) (*contacts.Identity, uuid.UUID, uuid.UUID) {
	tenant := uuid.New()
	identityID := uuid.New()
	survivorLinkID := uuid.New()
	orphanLinkID := uuid.New()
	orphanContact := uuid.New()
	return &contacts.Identity{
		ID:       identityID,
		TenantID: tenant,
		Links: []contacts.IdentityLink{
			{ID: survivorLinkID, IdentityID: identityID, ContactID: survivorContact,
				Reason:    contacts.LinkReasonExternalID,
				CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
			{ID: orphanLinkID, IdentityID: identityID, ContactID: orphanContact,
				Reason:    contacts.LinkReasonPhone,
				CreatedAt: time.Date(2026, 2, 2, 14, 30, 0, 0, time.UTC)},
		},
	}, orphanContact, orphanLinkID
}

func TestView_RendersIdentityPanel(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	survivor := uuid.New()
	identity, orphanContact, orphanLinkID := twoLinkIdentity(survivor)
	load := &stubLoad{res: contactsusecase.LoadIdentityResult{Identity: identity}}
	h := newHandler(t, load, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)

	r := reqWithTenant(http.MethodGet, "/contacts/"+survivor.String(), "", tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !load.called || load.in.TenantID != tenant || load.in.ContactID != survivor {
		t.Fatalf("LoadIdentity called wrong: %+v", load.in)
	}
	body := rec.Body.String()
	wants := []string{
		`<meta name="csrf-token"`,
		`id="identity-panel"`,
		`data-identity-id="` + identity.ID.String() + `"`,
		`data-contact-id="` + orphanContact.String() + `"`,
		`name="link_id" value="` + orphanLinkID.String() + `"`,
		`name="survivor_contact_id" value="` + survivor.String() + `"`,
		`hx-confirm="Tem certeza`,
		`Separar este contato`,
		`Telefone`,
		`ID externo`,
		`Contato corrente`,
		`2026-02-02 14:30 UTC`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Confirm there is NO split button for the survivor's own row.
	survivorRow := `data-contact-id="` + survivor.String() + `"`
	idx := strings.Index(body, survivorRow)
	if idx < 0 {
		t.Fatalf("survivor row not rendered")
	}
	end := idx
	if next := strings.Index(body[idx:], "</li>"); next > 0 {
		end = idx + next
	}
	row := body[idx:end]
	if strings.Contains(row, "Separar este contato") {
		t.Errorf("survivor row should NOT carry a split button:\n%s", row)
	}
}

func TestView_EmptyLinksRendersEmptyState(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	contactID := uuid.New()
	load := &stubLoad{res: contactsusecase.LoadIdentityResult{Identity: &contacts.Identity{
		ID: uuid.New(), TenantID: tenant,
	}}}
	h := newHandler(t, load, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)

	r := reqWithTenant(http.MethodGet, "/contacts/"+contactID.String(), "", tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Nenhum contato vinculado") {
		t.Errorf("empty-state copy missing")
	}
}

func TestView_BadContactIDReturns400(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLoad{}, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)
	r := reqWithTenant(http.MethodGet, "/contacts/not-a-uuid", "", uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestView_MissingTenantReturns500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLoad{}, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)
	r := reqNoTenant(http.MethodGet, "/contacts/"+uuid.New().String(), "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestView_NotFoundReturns404(t *testing.T) {
	t.Parallel()
	load := &stubLoad{err: contacts.ErrNotFound}
	h := newHandler(t, load, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)
	r := reqWithTenant(http.MethodGet, "/contacts/"+uuid.New().String(), "", uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestView_UseCaseErrorReturns500(t *testing.T) {
	t.Parallel()
	load := &stubLoad{err: errors.New("simulated read failure")}
	h := newHandler(t, load, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)
	r := reqWithTenant(http.MethodGet, "/contacts/"+uuid.New().String(), "", uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestView_EmptyCSRFReturns500(t *testing.T) {
	t.Parallel()
	load := &stubLoad{res: contactsusecase.LoadIdentityResult{Identity: &contacts.Identity{ID: uuid.New()}}}
	h, err := webcontacts.New(webcontacts.Deps{
		LoadIdentity: load, SplitLink: &stubSplit{},
		CSRFToken: func(*http.Request) string { return "" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	r := reqWithTenant(http.MethodGet, "/contacts/"+uuid.New().String(), "", uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestSplit_SuccessRendersFragment(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	survivor := uuid.New()
	identity, _, orphanLinkID := twoLinkIdentity(survivor)
	// After split: survivor identity loses the orphan link.
	post := &contacts.Identity{
		ID: identity.ID, TenantID: identity.TenantID,
		Links: identity.Links[:1], // only the survivor's own link remains
	}
	split := &stubSplit{res: contactsusecase.SplitResult{Identity: post}}
	h := newHandler(t, &stubLoad{}, split)
	mux := http.NewServeMux()
	h.Routes(mux)

	form := url.Values{
		"link_id":             {orphanLinkID.String()},
		"survivor_contact_id": {survivor.String()},
	}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/identity/split", form, tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	if !split.called {
		t.Fatalf("SplitLink not called")
	}
	if split.in.TenantID != tenant || split.in.LinkID != orphanLinkID || split.in.SurvivorContactID != survivor {
		t.Fatalf("split args wrong: %+v", split.in)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), `<section id="identity-panel"`) {
		t.Errorf("fragment should start with identity-panel section, got: %q", body[:min(200, len(body))])
	}
	// The orphan must be absent post-split.
	if strings.Contains(body, "Separar este contato") {
		t.Errorf("post-split panel still has a split button; should only have survivor row")
	}
}

func TestSplit_BadFormReturnsBadRequest(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"bad link":     "link_id=nope&survivor_contact_id=" + uuid.New().String(),
		"bad survivor": "link_id=" + uuid.New().String() + "&survivor_contact_id=nope",
		"empty body":   "",
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHandler(t, &stubLoad{}, &stubSplit{})
			mux := http.NewServeMux()
			h.Routes(mux)
			r := reqWithTenant(http.MethodPost, "/contacts/identity/split", form, uuid.New())
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400", rec.Code)
			}
		})
	}
}

func TestSplit_MissingTenantReturns500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLoad{}, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)
	form := url.Values{
		"link_id":             {uuid.New().String()},
		"survivor_contact_id": {uuid.New().String()},
	}.Encode()
	r := reqNoTenant(http.MethodPost, "/contacts/identity/split", form)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestSplit_NotFoundReturns404(t *testing.T) {
	t.Parallel()
	split := &stubSplit{err: contacts.ErrNotFound}
	h := newHandler(t, &stubLoad{}, split)
	mux := http.NewServeMux()
	h.Routes(mux)
	form := url.Values{
		"link_id":             {uuid.New().String()},
		"survivor_contact_id": {uuid.New().String()},
	}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/identity/split", form, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestSplit_UseCaseErrorReturns500(t *testing.T) {
	t.Parallel()
	split := &stubSplit{err: errors.New("tx failure")}
	h := newHandler(t, &stubLoad{}, split)
	mux := http.NewServeMux()
	h.Routes(mux)
	form := url.Values{
		"link_id":             {uuid.New().String()},
		"survivor_contact_id": {uuid.New().String()},
	}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/identity/split", form, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestSplit_EmptyCSRFReturns500(t *testing.T) {
	t.Parallel()
	split := &stubSplit{res: contactsusecase.SplitResult{Identity: &contacts.Identity{ID: uuid.New()}}}
	h, err := webcontacts.New(webcontacts.Deps{
		LoadIdentity: &stubLoad{}, SplitLink: split,
		CSRFToken: func(*http.Request) string { return "" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	form := url.Values{
		"link_id":             {uuid.New().String()},
		"survivor_contact_id": {uuid.New().String()},
	}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/identity/split", form, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

// TestPanel_Snapshot is the snapshot test required by AC #5. The
// expected fragment is hand-crafted from a fixed identity so the
// template rendering is byte-stable across runs.
func TestPanel_Snapshot(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	survivor := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	orphan := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	identityID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	survivorLinkID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	orphanLinkID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	identity := &contacts.Identity{
		ID: identityID, TenantID: tenant,
		Links: []contacts.IdentityLink{
			{ID: survivorLinkID, IdentityID: identityID, ContactID: survivor,
				Reason:    contacts.LinkReasonExternalID,
				CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
			{ID: orphanLinkID, IdentityID: identityID, ContactID: orphan,
				Reason:    contacts.LinkReasonPhone,
				CreatedAt: time.Date(2026, 2, 2, 14, 30, 0, 0, time.UTC)},
		},
	}
	load := &stubLoad{res: contactsusecase.LoadIdentityResult{Identity: identity}}
	h := newHandler(t, load, &stubSplit{})
	mux := http.NewServeMux()
	h.Routes(mux)
	r := reqWithTenant(http.MethodGet, "/contacts/"+survivor.String(), "", tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	body := rec.Body.String()

	// Spot-check the exact fragment for the orphan row — the snapshot is
	// the form block with the split button so accidental shape regressions
	// (e.g. dropping the survivor_contact_id hidden input) fail loudly.
	wantFragment := `<form class="identity-link__form"
            hx-post="/contacts/identity/split"
            hx-target="#identity-panel"
            hx-swap="outerHTML"
            hx-confirm="Tem certeza? Esta separação é destrutiva — o merge automático será desfeito.">
        <input type="hidden" name="_csrf" value="csrf-test-token">
        <input type="hidden" name="link_id" value="` + orphanLinkID.String() + `">
        <input type="hidden" name="survivor_contact_id" value="` + survivor.String() + `">
        <button type="submit" class="identity-link__split">Separar este contato</button>
      </form>`
	if !strings.Contains(body, wantFragment) {
		t.Errorf("orphan-row fragment did not match snapshot\nGot body:\n%s", body)
	}
}

func min(a, b int) int { //nolint:predeclared
	if a < b {
		return a
	}
	return b
}
