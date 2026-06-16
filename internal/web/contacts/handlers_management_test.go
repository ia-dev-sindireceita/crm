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
	webcontacts "github.com/pericles-luz/crm/internal/web/contacts"
)

// --- stubs for the SIN-64977 management use cases ---------------------

type stubList struct {
	mu     sync.Mutex
	in     contactsusecase.ListContactsInput
	called bool
	res    contactsusecase.ListContactsResult
	err    error
}

func (s *stubList) Execute(_ context.Context, in contactsusecase.ListContactsInput) (contactsusecase.ListContactsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

type stubDetail struct {
	mu     sync.Mutex
	in     contactsusecase.GetContactDetailInput
	called bool
	res    contactsusecase.GetContactDetailResult
	err    error
}

func (s *stubDetail) Execute(_ context.Context, in contactsusecase.GetContactDetailInput) (contactsusecase.GetContactDetailResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

type stubUpdate struct {
	mu     sync.Mutex
	in     contactsusecase.UpdateContactInput
	called bool
	res    contactsusecase.UpdateContactResult
	err    error
}

func (s *stubUpdate) Execute(_ context.Context, in contactsusecase.UpdateContactInput) (contactsusecase.UpdateContactResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

// fullDeps wires every use case so the list + edit routes register.
func fullDeps(list *stubList, detail *stubDetail, update *stubUpdate) webcontacts.Deps {
	return webcontacts.Deps{
		LoadIdentity:  &stubLoad{res: contactsusecase.LoadIdentityResult{Identity: &contacts.Identity{ID: uuid.New()}}},
		SplitLink:     &stubSplit{},
		CSRFToken:     func(*http.Request) string { return "csrf-test-token" },
		ListContacts:  list,
		GetDetail:     detail,
		UpdateContact: update,
	}
}

func newFullHandler(t *testing.T, deps webcontacts.Deps) http.Handler {
	t.Helper()
	h, err := webcontacts.New(deps)
	if err != nil {
		t.Fatalf("webcontacts.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func sampleSummary(name string, channels ...string) contactsusecase.ContactSummaryView {
	ids := make([]contactsusecase.ContactIdentityView, 0, len(channels))
	for _, c := range channels {
		ids = append(ids, contactsusecase.ContactIdentityView{Channel: c, ExternalID: "+5511" + c})
	}
	return contactsusecase.ContactSummaryView{
		ID:          uuid.New(),
		DisplayName: name,
		Identities:  ids,
		Channels:    channels,
	}
}

// --- New() backward + forward compatibility --------------------------

func TestNew_AcceptsOptionalManagementDeps(t *testing.T) {
	t.Parallel()
	if _, err := webcontacts.New(fullDeps(&stubList{}, &stubDetail{}, &stubUpdate{})); err != nil {
		t.Fatalf("New(full management deps): %v", err)
	}
}

// --- List / search ----------------------------------------------------

func TestList_RendersFullPage(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	list := &stubList{res: contactsusecase.ListContactsResult{
		Items: []contactsusecase.ContactSummaryView{
			sampleSummary("Alice", "whatsapp"),
			sampleSummary("Bob", "email"),
		},
		Total: 2, Limit: 50, Offset: 0,
	}}
	h := newFullHandler(t, fullDeps(list, &stubDetail{}, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts", "", tenant)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !list.called || list.in.TenantID != tenant {
		t.Fatalf("ListContacts called wrong: %+v", list.in)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<meta name="csrf-token"`,
		`name="q"`,
		`hx-trigger="keyup changed delay:300ms`,
		`id="contacts-results"`,
		">Alice<",
		">Bob<",
		"Exibindo 1–2 de 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list body missing %q", want)
		}
	}
}

func TestList_HXRequestReturnsFragmentOnly(t *testing.T) {
	t.Parallel()
	list := &stubList{res: contactsusecase.ListContactsResult{
		Items: []contactsusecase.ContactSummaryView{sampleSummary("Carol", "whatsapp")},
		Total: 1, Limit: 50, Offset: 0,
	}}
	h := newFullHandler(t, fullDeps(list, &stubDetail{}, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts?q=car", "", uuid.New())
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, `<div id="contacts-results"`) {
		t.Errorf("fragment should start with contacts-results div; got %q", body[:min(120, len(body))])
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HX fragment must NOT include the page shell")
	}
	if list.in.Query != "car" {
		t.Errorf("query not propagated: got %q", list.in.Query)
	}
}

func TestList_ParsesPagination(t *testing.T) {
	t.Parallel()
	list := &stubList{res: contactsusecase.ListContactsResult{
		Items: []contactsusecase.ContactSummaryView{sampleSummary("Dora")},
		Total: 120, Limit: 50, Offset: 50,
	}}
	h := newFullHandler(t, fullDeps(list, &stubDetail{}, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts?q=&limit=50&offset=50", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if list.in.Limit != 50 || list.in.Offset != 50 {
		t.Fatalf("pagination not propagated: %+v", list.in)
	}
	body := rec.Body.String()
	// With offset 50 of 120 there must be both a prev and a next pager link.
	if !strings.Contains(body, "offset=0") {
		t.Errorf("expected prev pager link to offset 0")
	}
	if !strings.Contains(body, "offset=100") {
		t.Errorf("expected next pager link to offset 100")
	}
}

func TestList_MalformedPaginationDefaultsToZero(t *testing.T) {
	t.Parallel()
	list := &stubList{res: contactsusecase.ListContactsResult{Limit: 50}}
	h := newFullHandler(t, fullDeps(list, &stubDetail{}, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts?limit=abc&offset=-5", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if list.in.Limit != 0 || list.in.Offset != 0 {
		t.Fatalf("malformed pagination should default to 0: %+v", list.in)
	}
}

func TestList_EmptyResultRendersEmptyState(t *testing.T) {
	t.Parallel()
	list := &stubList{res: contactsusecase.ListContactsResult{Limit: 50}}
	h := newFullHandler(t, fullDeps(list, &stubDetail{}, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts?q=zzz", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nenhum contato") {
		t.Errorf("empty-state copy missing")
	}
}

func TestList_MissingTenantReturns500(t *testing.T) {
	t.Parallel()
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, &stubUpdate{}))
	r := reqNoTenant(http.MethodGet, "/contacts", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}

func TestList_UseCaseErrorReturns500(t *testing.T) {
	t.Parallel()
	list := &stubList{err: errors.New("boom")}
	h := newFullHandler(t, fullDeps(list, &stubDetail{}, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}

func TestList_EmptyCSRFReturns500(t *testing.T) {
	t.Parallel()
	deps := fullDeps(&stubList{res: contactsusecase.ListContactsResult{Limit: 50}}, &stubDetail{}, &stubUpdate{})
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := newFullHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/contacts", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}

func TestList_NotMountedWhenUseCaseNil(t *testing.T) {
	t.Parallel()
	// Identity-split-only deployment: no ListContacts → GET /contacts 404.
	h := newFullHandler(t, webcontacts.Deps{
		LoadIdentity: &stubLoad{},
		SplitLink:    &stubSplit{},
		CSRFToken:    func(*http.Request) string { return "tok" },
	})
	r := reqWithTenant(http.MethodGet, "/contacts", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (list route should be unmounted)", rec.Code)
	}
}

// --- Detail enrichment (view + GetDetail) -----------------------------

func TestView_EnrichedWithDetail(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	contactID := uuid.New()
	convID := uuid.New()
	detail := &stubDetail{res: contactsusecase.GetContactDetailResult{Contact: contactsusecase.ContactDetailView{
		ID:          contactID,
		DisplayName: "Eve Example",
		Channels:    []string{"email", "whatsapp"},
		Identities:  []contactsusecase.ContactIdentityView{{Channel: "whatsapp", ExternalID: "+5511999"}},
		Conversations: []contactsusecase.ConversationSummaryView{{
			ID: convID, Channel: "whatsapp", State: "open",
			LastMessageAt: time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC),
		}},
	}}}
	deps := fullDeps(&stubList{}, detail, &stubUpdate{})
	deps.LoadIdentity = &stubLoad{res: contactsusecase.LoadIdentityResult{Identity: &contacts.Identity{ID: uuid.New(), TenantID: tenant}}}
	h := newFullHandler(t, deps)

	r := reqWithTenant(http.MethodGet, "/contacts/"+contactID.String(), "", tenant)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !detail.called || detail.in.ContactID != contactID {
		t.Fatalf("GetDetail called wrong: %+v", detail.in)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Eve Example",
		`data-channel="email"`,
		`data-channel="whatsapp"`,
		`data-conversation-id="` + convID.String() + `"`,
		"Aberta",
		`hx-get="/contacts/` + contactID.String() + `/edit"`,
		`id="identity-panel"`, // identity panel still present
	} {
		if !strings.Contains(body, want) {
			t.Errorf("enriched detail body missing %q", want)
		}
	}
}

func TestView_DetailNotFoundReturns404(t *testing.T) {
	t.Parallel()
	detail := &stubDetail{err: contacts.ErrNotFound}
	deps := fullDeps(&stubList{}, detail, &stubUpdate{})
	h := newFullHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/contacts/"+uuid.New().String(), "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

func TestView_DetailErrorReturns500(t *testing.T) {
	t.Parallel()
	detail := &stubDetail{err: errors.New("read fail")}
	deps := fullDeps(&stubList{}, detail, &stubUpdate{})
	h := newFullHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/contacts/"+uuid.New().String(), "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}

func TestView_NoHistoryRendersEmptyState(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	detail := &stubDetail{res: contactsusecase.GetContactDetailResult{Contact: contactsusecase.ContactDetailView{
		ID: contactID, DisplayName: "No Convos",
	}}}
	deps := fullDeps(&stubList{}, detail, &stubUpdate{})
	h := newFullHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/contacts/"+contactID.String(), "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Nenhuma conversa registrada") {
		t.Errorf("empty-history copy missing")
	}
}

// --- Edit form (GET) --------------------------------------------------

func TestEditForm_RendersFullPage(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	detail := &stubDetail{res: contactsusecase.GetContactDetailResult{Contact: contactsusecase.ContactDetailView{
		ID: contactID, DisplayName: "Frank",
	}}}
	h := newFullHandler(t, fullDeps(&stubList{}, detail, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts/"+contactID.String()+"/edit", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<!doctype html>`,
		`id="contact-edit-panel"`,
		`name="display_name" value="Frank"`,
		`hx-post="/contacts/` + contactID.String() + `/edit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit page missing %q", want)
		}
	}
}

func TestEditForm_HXReturnsFragment(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	detail := &stubDetail{res: contactsusecase.GetContactDetailResult{Contact: contactsusecase.ContactDetailView{
		ID: contactID, DisplayName: "Grace",
	}}}
	h := newFullHandler(t, fullDeps(&stubList{}, detail, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts/"+contactID.String()+"/edit", "", uuid.New())
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, `<section id="contact-edit-panel"`) {
		t.Errorf("HX edit must return the panel fragment; got %q", body[:min(120, len(body))])
	}
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HX edit fragment must not include page shell")
	}
}

func TestEditForm_BadContactID400(t *testing.T) {
	t.Parallel()
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts/not-a-uuid/edit", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

func TestEditForm_NilContactID404(t *testing.T) {
	t.Parallel()
	detail := &stubDetail{}
	h := newFullHandler(t, fullDeps(&stubList{}, detail, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts/00000000-0000-0000-0000-000000000000/edit", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
	if detail.called {
		t.Fatalf("GetDetail must not run for nil contact id")
	}
}

func TestEditForm_NotFound404(t *testing.T) {
	t.Parallel()
	detail := &stubDetail{err: contacts.ErrNotFound}
	h := newFullHandler(t, fullDeps(&stubList{}, detail, &stubUpdate{}))
	r := reqWithTenant(http.MethodGet, "/contacts/"+uuid.New().String()+"/edit", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

func TestEditForm_MissingTenant500(t *testing.T) {
	t.Parallel()
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, &stubUpdate{}))
	r := reqNoTenant(http.MethodGet, "/contacts/"+uuid.New().String()+"/edit", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}

func TestEditRoutes_NotMountedWhenUpdateNil(t *testing.T) {
	t.Parallel()
	// GetDetail set but UpdateContact nil → edit routes unmounted.
	deps := webcontacts.Deps{
		LoadIdentity: &stubLoad{},
		SplitLink:    &stubSplit{},
		CSRFToken:    func(*http.Request) string { return "tok" },
		GetDetail:    &stubDetail{},
	}
	h := newFullHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/contacts/"+uuid.New().String()+"/edit", "", uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 (edit route should be unmounted)", rec.Code)
	}
}

// --- Update (POST) ----------------------------------------------------

func TestUpdate_SuccessHXReturnsSavedPanel(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	contactID := uuid.New()
	saved := sampleSummary("New Name", "whatsapp")
	saved.ID = contactID
	update := &stubUpdate{res: contactsusecase.UpdateContactResult{Contact: saved}}
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, update))

	form := url.Values{"display_name": {"New Name"}}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/"+contactID.String()+"/edit", form, tenant)
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !update.called || update.in.DisplayName != "New Name" || update.in.ContactID != contactID || update.in.TenantID != tenant {
		t.Fatalf("UpdateContact called wrong: %+v", update.in)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, `<section id="contact-edit-panel"`) {
		t.Errorf("expected saved panel fragment; got %q", body[:min(120, len(body))])
	}
	if !strings.Contains(body, "New Name") || !strings.Contains(body, "Nome atualizado") {
		t.Errorf("saved panel missing updated name/confirmation: %q", body)
	}
}

func TestUpdate_SuccessNonHXRedirects(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	saved := sampleSummary("Redir Name")
	saved.ID = contactID
	update := &stubUpdate{res: contactsusecase.UpdateContactResult{Contact: saved}}
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, update))
	form := url.Values{"display_name": {"Redir Name"}}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/"+contactID.String()+"/edit", form, uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/contacts/"+contactID.String() {
		t.Errorf("redirect target=%q want /contacts/%s", loc, contactID)
	}
}

func TestUpdate_EmptyNameReturns422Form(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	update := &stubUpdate{err: contacts.ErrEmptyDisplayName}
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, update))
	form := url.Values{"display_name": {"   "}}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/"+contactID.String()+"/edit", form, uuid.New())
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "não pode ficar em branco") {
		t.Errorf("422 re-render missing inline error: %q", body)
	}
	if !strings.Contains(body, `id="contact-edit-panel"`) {
		t.Errorf("422 must re-render the form panel")
	}
}

func TestUpdate_NotFound404(t *testing.T) {
	t.Parallel()
	update := &stubUpdate{err: contacts.ErrNotFound}
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, update))
	form := url.Values{"display_name": {"X"}}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/"+uuid.New().String()+"/edit", form, uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

func TestUpdate_UseCaseErrorReturns500(t *testing.T) {
	t.Parallel()
	update := &stubUpdate{err: errors.New("tx fail")}
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, update))
	form := url.Values{"display_name": {"X"}}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/"+uuid.New().String()+"/edit", form, uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}

func TestUpdate_BadContactID400(t *testing.T) {
	t.Parallel()
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, &stubUpdate{}))
	form := url.Values{"display_name": {"X"}}.Encode()
	r := reqWithTenant(http.MethodPost, "/contacts/not-a-uuid/edit", form, uuid.New())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

func TestUpdate_MissingTenant500(t *testing.T) {
	t.Parallel()
	h := newFullHandler(t, fullDeps(&stubList{}, &stubDetail{}, &stubUpdate{}))
	form := url.Values{"display_name": {"X"}}.Encode()
	r := reqNoTenant(http.MethodPost, "/contacts/"+uuid.New().String()+"/edit", form)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
}
