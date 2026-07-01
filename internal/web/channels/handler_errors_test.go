package channels_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/channels"
	webchannels "github.com/pericles-luz/crm/internal/web/channels"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

var errBoom = errors.New("boom")

func TestPage_StoreError_Is500(t *testing.T) {
	repo := newFakeRepo()
	repo.listErr = errBoom
	mux := newHandler(t, repo, newFakeAccess())
	if rec := do(t, mux, http.MethodGet, "/settings/channels", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestPage_RosterError_Is500(t *testing.T) {
	acc := newFakeAccess()
	acc.rosterErr = errBoom
	mux := newHandler(t, newFakeRepo(), acc)
	if rec := do(t, mux, http.MethodGet, "/settings/channels", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestNewForm_RosterError_Is500(t *testing.T) {
	acc := newFakeAccess()
	acc.rosterErr = errBoom
	mux := newHandler(t, newFakeRepo(), acc)
	if rec := do(t, mux, http.MethodGet, "/settings/channels/new", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestCreate_AccessError_Is500(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	acc.replaceErr = errBoom
	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "X")
	form.Set("channel_key", "whatsapp")
	form.Set("identity", "+55")
	if rec := do(t, mux, http.MethodPost, "/settings/channels", form); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestCreate_GenericStoreError_Is500(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errBoom
	mux := newHandler(t, repo, newFakeAccess())
	form := url.Values{}
	form.Set("name", "X")
	form.Set("channel_key", "whatsapp")
	if rec := do(t, mux, http.MethodPost, "/settings/channels", form); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestEditForm_GetError_Is500(t *testing.T) {
	repo := newFakeRepo()
	repo.getErr = errBoom
	mux := newHandler(t, repo, newFakeAccess())
	if rec := do(t, mux, http.MethodGet, "/settings/channels/"+uuid.New().String()+"/edit", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestEditForm_ChannelAccessError_Is500(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "S", true)
	acc.channelErr = errBoom
	mux := newHandler(t, repo, acc)
	if rec := do(t, mux, http.MethodGet, "/settings/channels/"+ch.ID.String()+"/edit", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess())
	form := url.Values{}
	form.Set("name", "X")
	if rec := do(t, mux, http.MethodPost, "/settings/channels/"+uuid.New().String(), form); rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestUpdate_RenameGenericError_Is500(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "Old", true)
	repo.renameErr = errBoom
	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "New")
	if rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestUpdate_RenameNotFound_Is404(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "Old", true)
	repo.renameErr = channels.ErrNotFound
	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "New")
	if rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form); rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestUpdate_RenameEmptyDisplayName_FieldError(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "Old", true)
	repo.renameErr = channels.ErrEmptyDisplayName
	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "New")
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (re-render)", rec.Code)
	}
	if !contains(rec.Body.String(), "Informe um nome") {
		t.Fatalf("expected field error re-render, got %s", rec.Body.String())
	}
}

func TestUpdate_AccessError_Is500(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "Old", true)
	acc.replaceErr = errBoom
	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "New")
	if rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestToggle_SetActiveGenericError_Is500(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "S", true)
	repo.activeErr = errBoom
	mux := newHandler(t, repo, acc)
	if rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String()+"/active", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestToggle_RowRosterError_Is500(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "S", true)
	mux := newHandler(t, repo, acc)
	// SetActive succeeds, then the row rebuild's roster read fails.
	acc.rosterErr = errBoom
	if rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String()+"/active", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// ---- chrome wiring + label fallbacks -----------------------------------

func TestPage_ChromeWiring_CSRFAndUserLabel(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	h, err := webchannels.New(webchannels.Deps{
		Channels:   repo,
		Access:     acc,
		CSRFToken:  func(*http.Request) string { return "tok-123" },
		UserID:     func(*http.Request) uuid.UUID { return uuid.New() },
		UserLabels: fakeDir{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	// The CSRF token flows into the shell hx-headers / meta.
	if !contains(rec.Body.String(), "tok-123") {
		t.Fatalf("csrf token not wired into page")
	}
}

type fakeDir struct{}

func (fakeDir) LabelsByID(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		out[id] = "operador"
	}
	return out, nil
}

var _ userlabel.Directory = fakeDir{}

// TestRegistry_LabelFallbacks exercises typeLabel's legacy-key fallback +
// roleLabel's non-primary roles + the multi-digit atendente count (itoa).
func TestRegistry_LabelFallbacks(t *testing.T) {
	repo := newFakeRepo()
	// 12 roster users across roles so a partial grant renders "11 atendentes".
	var roster []channels.RosterUser
	roster = append(roster, channels.RosterUser{ID: uuid.New(), DisplayName: "lead", Role: "tenant_lider"})
	roster = append(roster, channels.RosterUser{ID: uuid.New(), DisplayName: "obs", Role: "tenant_common"})
	roster = append(roster, channels.RosterUser{ID: uuid.New(), DisplayName: "weird", Role: "tenant_unknown"})
	for i := 0; i < 9; i++ {
		roster = append(roster, channels.RosterUser{ID: uuid.New(), DisplayName: "a", Role: "tenant_atendente"})
	}
	acc := newFakeAccess(roster...)

	// A channel with a legacy channel_key outside the closed set.
	ch := mkChannel(t, repo, "sms", "12345", "Legacy", true)
	grant := make([]uuid.UUID, 0, 11)
	for i := 0; i < 11; i++ {
		grant = append(grant, roster[i].ID)
	}
	acc.grants[ch.ID] = grant // 11 of 12 → "11 atendentes"

	mux := newHandler(t, repo, acc)
	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	body := rec.Body.String()
	if !contains(body, "Sms") {
		t.Fatalf("legacy channel_key not title-cased in type column\nbody=%s", body)
	}
	if !contains(body, "11 atendentes") {
		t.Fatalf("multi-digit atendente count missing\nbody=%s", body)
	}

	// The edit form's roster renders the non-primary role labels.
	rec = do(t, mux, http.MethodGet, "/settings/channels/"+ch.ID.String()+"/edit", nil)
	eb := rec.Body.String()
	for _, want := range []string{"Líder", "Comum", "tenant_unknown"} {
		if !contains(eb, want) {
			t.Fatalf("roster missing role label %q", want)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
