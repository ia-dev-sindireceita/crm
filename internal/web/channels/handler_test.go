package channels_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/channels"
	"github.com/pericles-luz/crm/internal/tenancy"
	webchannels "github.com/pericles-luz/crm/internal/web/channels"
)

// ---- in-memory fakes (web-layer collaborators; the real DB behaviour is
// covered by the postgres adapter integration tests) --------------------

type fakeRepo struct {
	chans     map[uuid.UUID]*channels.Channel
	createErr error
	renameErr error
	listErr   error
	getErr    error
	activeErr error
	created   []*channels.Channel
	renamed   map[uuid.UUID]string
	active    map[uuid.UUID]bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		chans:   map[uuid.UUID]*channels.Channel{},
		renamed: map[uuid.UUID]string{},
		active:  map[uuid.UUID]bool{},
	}
}

func (f *fakeRepo) List(_ context.Context, tenantID uuid.UUID) ([]*channels.Channel, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*channels.Channel, 0, len(f.chans))
	for _, c := range f.chans {
		out = append(out, c)
	}
	// deterministic by name for stable assertions
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].DisplayName < out[i].DisplayName {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (f *fakeRepo) Create(_ context.Context, c *channels.Channel) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.chans[c.ID] = c
	f.created = append(f.created, c)
	return nil
}

func (f *fakeRepo) Rename(_ context.Context, _ uuid.UUID, id uuid.UUID, name string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	c, ok := f.chans[id]
	if !ok {
		return channels.ErrNotFound
	}
	c.DisplayName = name
	f.renamed[id] = name
	return nil
}

func (f *fakeRepo) SetActive(_ context.Context, _ uuid.UUID, id uuid.UUID, a bool) error {
	if f.activeErr != nil {
		return f.activeErr
	}
	c, ok := f.chans[id]
	if !ok {
		return channels.ErrNotFound
	}
	c.IsActive = a
	f.active[id] = a
	return nil
}

func (f *fakeRepo) Get(_ context.Context, _ uuid.UUID, id uuid.UUID) (*channels.Channel, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.chans[id]
	if !ok {
		return nil, channels.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

type fakeAccess struct {
	roster     []channels.RosterUser
	grants     map[uuid.UUID][]uuid.UUID
	replaced   map[uuid.UUID][]uuid.UUID
	replaceErr error
	rosterErr  error
	channelErr error
}

func newFakeAccess(roster ...channels.RosterUser) *fakeAccess {
	return &fakeAccess{roster: roster, grants: map[uuid.UUID][]uuid.UUID{}, replaced: map[uuid.UUID][]uuid.UUID{}}
}

func (f *fakeAccess) ListRosterUsers(_ context.Context, _ uuid.UUID) ([]channels.RosterUser, error) {
	if f.rosterErr != nil {
		return nil, f.rosterErr
	}
	return f.roster, nil
}

func (f *fakeAccess) ChannelUserIDs(_ context.Context, _ uuid.UUID, channelID uuid.UUID) ([]uuid.UUID, error) {
	if f.channelErr != nil {
		return nil, f.channelErr
	}
	return f.grants[channelID], nil
}

func (f *fakeAccess) ReplaceAccess(_ context.Context, _ uuid.UUID, channelID uuid.UUID, userIDs []uuid.UUID) error {
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.grants[channelID] = userIDs
	f.replaced[channelID] = userIDs
	return nil
}

// ---- harness -----------------------------------------------------------

var testTenant = &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}

func newHandler(t *testing.T, repo *fakeRepo, acc *fakeAccess) http.Handler {
	t.Helper()
	h, err := webchannels.New(webchannels.Deps{Channels: repo, Access: acc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func do(t *testing.T, mux http.Handler, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(tenancy.WithContext(r.Context(), testTenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func rosterUser(name, role string) channels.RosterUser {
	return channels.RosterUser{ID: uuid.New(), DisplayName: name, Role: role}
}

func mkChannel(t *testing.T, repo *fakeRepo, key, ext, name string, active bool) *channels.Channel {
	t.Helper()
	c, err := channels.New(testTenant.ID, key, ext, name)
	if err != nil {
		t.Fatalf("channels.New: %v", err)
	}
	c.IsActive = active
	repo.chans[c.ID] = c
	return c
}

// ---- tests -------------------------------------------------------------

func TestNew_RequiresPorts(t *testing.T) {
	if _, err := webchannels.New(webchannels.Deps{}); err == nil {
		t.Fatal("New(nil Channels) = nil error, want error")
	}
	if _, err := webchannels.New(webchannels.Deps{Channels: newFakeRepo()}); err == nil {
		t.Fatal("New(nil Access) = nil error, want error")
	}
}

func TestPage_Empty(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess())
	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`data-testid="channels"`, "Nenhum canal configurado", "+ Novo canal"} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty page missing %q\nbody=%s", want, body)
		}
	}
	// htmx loaded by the surface (shell does not inject it), nonced.
	if !strings.Contains(body, `src="/static/vendor/htmx/2.0.9/htmx.min.js"`) {
		t.Fatalf("page missing vendored htmx script\nbody=%s", body)
	}
	if !strings.Contains(body, `<link rel="stylesheet" href="/static/css/channels.css">`) {
		t.Fatalf("page missing channels.css link")
	}
}

func TestPage_WithRows_AccessSummaries(t *testing.T) {
	repo := newFakeRepo()
	u1, u2, u3 := rosterUser("ana", "tenant_atendente"), rosterUser("bia", "tenant_atendente"), rosterUser("cid", "tenant_gerente")
	acc := newFakeAccess(u1, u2, u3)

	full := mkChannel(t, repo, "whatsapp", "+5511999990000", "Suporte", true)
	acc.grants[full.ID] = []uuid.UUID{u1.ID, u2.ID, u3.ID} // Todos
	one := mkChannel(t, repo, "telegram", "@lojabot", "Vendas", false)
	acc.grants[one.ID] = []uuid.UUID{u1.ID} // 1 atendente

	mux := newHandler(t, repo, acc)
	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	for _, want := range []string{"Suporte", "Vendas", "WhatsApp", "Telegram", "Todos", "1 atendente", "Ativo", "Inativo"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rows page missing %q\nbody=%s", want, body)
		}
	}
	// Identity is masked (D4) — the full number must never render.
	if strings.Contains(body, "5511999990000") {
		t.Fatalf("full identity leaked into registry (must be masked)\nbody=%s", body)
	}
	// Inactive channel keeps its row (deactivate-not-delete) with an
	// Ativar action.
	if !strings.Contains(body, "Ativar") {
		t.Fatalf("inactive row missing Ativar action")
	}
}

func TestNewForm_AllChecked(t *testing.T) {
	u1, u2 := rosterUser("ana", "tenant_atendente"), rosterUser("bia", "tenant_gerente")
	mux := newHandler(t, newFakeRepo(), newFakeAccess(u1, u2))
	rec := do(t, mux, http.MethodGet, "/settings/channels/new", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.Count(body, `type="checkbox"`) != 2 {
		t.Fatalf("want 2 roster checkboxes, body=%s", body)
	}
	if strings.Count(body, "checked") < 2 {
		t.Fatalf("new-channel roster must pre-check all users, body=%s", body)
	}
	if !strings.Contains(body, "2 de 2 com acesso") {
		t.Fatalf("missing live count, body=%s", body)
	}
	if !strings.Contains(body, "Novo canal") || !strings.Contains(body, "WhatsApp") {
		t.Fatalf("form missing title/type option")
	}
}

func TestCreate_Success_WritesRosterAndRefreshes(t *testing.T) {
	repo := newFakeRepo()
	u1, u2 := rosterUser("ana", "tenant_atendente"), rosterUser("bia", "tenant_gerente")
	acc := newFakeAccess(u1, u2)
	mux := newHandler(t, repo, acc)

	form := url.Values{}
	form.Set("name", "Suporte")
	form.Set("channel_key", "whatsapp")
	form.Set("identity", "+5511988887777")
	form.Add("user_ids", u1.ID.String())
	// u2 intentionally left unchecked

	rec := do(t, mux, http.MethodPost, "/settings/channels", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.created) != 1 {
		t.Fatalf("want 1 channel created, got %d", len(repo.created))
	}
	created := repo.created[0]
	got := acc.replaced[created.ID]
	if len(got) != 1 || got[0] != u1.ID {
		t.Fatalf("roster grant = %v, want [%s]", got, u1.ID)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-swap-oob="true"`) || !strings.Contains(body, "Canal criado.") {
		t.Fatalf("success response missing OOB list + toast\nbody=%s", body)
	}
}

func TestCreate_NameRequired(t *testing.T) {
	repo := newFakeRepo()
	mux := newHandler(t, repo, newFakeAccess(rosterUser("ana", "tenant_atendente")))
	form := url.Values{}
	form.Set("name", "")
	form.Set("channel_key", "whatsapp")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Fatalf("no channel should be created on validation error")
	}
	if !strings.Contains(rec.Body.String(), "Informe um nome") {
		t.Fatalf("missing name field error\nbody=%s", rec.Body.String())
	}
}

func TestCreate_InvalidType(t *testing.T) {
	repo := newFakeRepo()
	mux := newHandler(t, repo, newFakeAccess())
	form := url.Values{}
	form.Set("name", "X")
	form.Set("channel_key", "carrier-pigeon")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)
	if len(repo.created) != 0 {
		t.Fatalf("invalid type must not create")
	}
	if !strings.Contains(rec.Body.String(), "tipo de canal válido") {
		t.Fatalf("missing type error\nbody=%s", rec.Body.String())
	}
}

func TestCreate_Conflict(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = channels.ErrChannelConflict
	mux := newHandler(t, repo, newFakeAccess())
	form := url.Values{}
	form.Set("name", "Dup")
	form.Set("channel_key", "whatsapp")
	form.Set("identity", "+55")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Já existe um canal") {
		t.Fatalf("missing conflict error\nbody=%s", rec.Body.String())
	}
}

func TestEditForm_PrefilledAndGrantedChecked(t *testing.T) {
	repo := newFakeRepo()
	u1, u2 := rosterUser("ana", "tenant_atendente"), rosterUser("bia", "tenant_gerente")
	acc := newFakeAccess(u1, u2)
	ch := mkChannel(t, repo, "whatsapp", "+5511999990000", "Suporte", true)
	acc.grants[ch.ID] = []uuid.UUID{u1.ID}
	mux := newHandler(t, repo, acc)

	rec := do(t, mux, http.MethodGet, "/settings/channels/"+ch.ID.String()+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="Suporte"`) {
		t.Fatalf("edit form not prefilled with name\nbody=%s", body)
	}
	if !strings.Contains(body, "Editar canal") {
		t.Fatalf("edit modal title missing")
	}
	// Type is read-only on edit → the select is disabled + a hidden mirror.
	if !strings.Contains(body, "disabled") {
		t.Fatalf("edit type select must be disabled")
	}
	// u1 checked, u2 not: exactly one checked box.
	if strings.Count(body, "checked") != 1 {
		t.Fatalf("edit roster should pre-check exactly the granted user\nbody=%s", body)
	}
	// Identity is masked even in the readonly edit field.
	if strings.Contains(body, "5511999990000") {
		t.Fatalf("edit form leaked full identity")
	}
}

func TestEditForm_NotFound(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess())
	rec := do(t, mux, http.MethodGet, "/settings/channels/"+uuid.New().String()+"/edit", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	// A non-uuid id is also a 404, not a 500.
	rec = do(t, mux, http.MethodGet, "/settings/channels/not-a-uuid/edit", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad-id status=%d, want 404", rec.Code)
	}
}

func TestUpdate_RenamesAndReplacesRoster(t *testing.T) {
	repo := newFakeRepo()
	u1, u2 := rosterUser("ana", "tenant_atendente"), rosterUser("bia", "tenant_gerente")
	acc := newFakeAccess(u1, u2)
	ch := mkChannel(t, repo, "whatsapp", "+55", "Old", true)
	acc.grants[ch.ID] = []uuid.UUID{u1.ID}
	mux := newHandler(t, repo, acc)

	form := url.Values{}
	form.Set("name", "New")
	form.Add("user_ids", u2.ID.String())
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if repo.renamed[ch.ID] != "New" {
		t.Fatalf("rename = %q, want New", repo.renamed[ch.ID])
	}
	got := acc.replaced[ch.ID]
	if len(got) != 1 || got[0] != u2.ID {
		t.Fatalf("roster replace = %v, want [%s]", got, u2.ID)
	}
	if !strings.Contains(rec.Body.String(), "Canal atualizado.") {
		t.Fatalf("missing update toast")
	}
}

func TestUpdate_NameRequired(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "Old", true)
	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "   ")
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if repo.renamed[ch.ID] == "" && !strings.Contains(rec.Body.String(), "Informe um nome") {
		// rename must not have happened; error re-rendered
		t.Fatalf("blank rename should surface a field error\nbody=%s", rec.Body.String())
	}
	if _, ok := repo.renamed[ch.ID]; ok {
		t.Fatalf("rename should not be called on blank name")
	}
}

func TestToggle_FlipsAndReturnsRow(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	ch := mkChannel(t, repo, "whatsapp", "+55", "Suporte", true)
	acc.grants[ch.ID] = nil
	mux := newHandler(t, repo, acc)

	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String()+"/active", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if repo.active[ch.ID] != false {
		t.Fatalf("toggle should have deactivated the channel")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "channel-row-"+ch.ID.String()) || !strings.Contains(body, "Inativo") {
		t.Fatalf("toggle row response wrong\nbody=%s", body)
	}
	if !strings.Contains(body, "As conversas existentes permanecem") {
		t.Fatalf("deactivate toast missing reassurance copy")
	}

	// Toggle back → active + "Ativar" gone / "Desativar" present.
	rec = do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String()+"/active", nil)
	if repo.active[ch.ID] != true {
		t.Fatalf("second toggle should re-activate")
	}
	if !strings.Contains(rec.Body.String(), "Canal ativado.") {
		t.Fatalf("activate toast missing")
	}
}

func TestToggle_NotFound(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess())
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+uuid.New().String()+"/active", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestCancel_ClearsModal(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess())
	rec := do(t, mux, http.MethodGet, "/settings/channels/cancel", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("cancel should return empty body, got %q", rec.Body.String())
	}
}

func TestTenantMissing_Is500(t *testing.T) {
	h, _ := webchannels.New(webchannels.Deps{Channels: newFakeRepo(), Access: newFakeAccess()})
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/settings/channels", nil) // no tenant in ctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// TestNoInlineEventHandlers guards the strict CSP: the rendered surfaces
// must not carry inline on*= / hx-on: handlers (script-src 'self'
// 'nonce-…' silently strips them). All behaviour is wired via hx-get /
// hx-post attributes + a nonced external htmx.
func TestNoInlineEventHandlers(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	ch := mkChannel(t, repo, "whatsapp", "+55", "Suporte", true)
	acc.grants[ch.ID] = []uuid.UUID{}
	mux := newHandler(t, repo, acc)

	targets := []struct {
		method, path string
	}{
		{http.MethodGet, "/settings/channels"},
		{http.MethodGet, "/settings/channels/new"},
		{http.MethodGet, "/settings/channels/" + ch.ID.String() + "/edit"},
	}
	banned := []string{"onclick=", "onchange=", "onsubmit=", "oninput=", "hx-on:", "hx-on="}
	for _, tc := range targets {
		rec := do(t, mux, tc.method, tc.path, nil)
		body := strings.ToLower(rec.Body.String())
		for _, b := range banned {
			if strings.Contains(body, b) {
				t.Fatalf("%s %s leaked inline handler %q (strict CSP would strip it)", tc.method, tc.path, b)
			}
		}
	}
}
