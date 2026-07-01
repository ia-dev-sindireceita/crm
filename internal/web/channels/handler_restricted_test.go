package channels_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/channels"
)

// TestPage_ShowsAccessMode pins the registry access-mode badge: an open
// channel reads "Aberto", a restricted one reads "Restrito" (P3 makes the
// enforced state visible, color-independent text label).
func TestPage_ShowsAccessMode(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))

	open := mkChannel(t, repo, "whatsapp", "+5511900000000", "Aberto Ch", true)
	_ = open
	restricted := mkChannel(t, repo, "telegram", "@bot", "Restrito Ch", true)
	restricted.Restricted = true

	mux := newHandler(t, repo, acc)
	rec := do(t, mux, http.MethodGet, "/settings/channels", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ">Aberto<") {
		t.Fatalf("open channel missing Aberto badge\nbody=%s", body)
	}
	if !strings.Contains(body, ">Restrito<") {
		t.Fatalf("restricted channel missing Restrito badge\nbody=%s", body)
	}
}

// TestEditForm_RestrictedCheckbox verifies the maintenance edit form
// exposes the restricted toggle and reflects the stored flag.
func TestEditForm_RestrictedCheckbox(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))

	// Restricted channel → checkbox rendered and checked.
	ch := mkChannel(t, repo, "whatsapp", "+5511911112222", "Suporte", true)
	ch.Restricted = true
	mux := newHandler(t, repo, acc)
	rec := do(t, mux, http.MethodGet, "/settings/channels/"+ch.ID.String()+"/edit", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, body)
	}
	if !strings.Contains(body, `name="restricted"`) {
		t.Fatalf("edit form missing restricted checkbox\nbody=%s", body)
	}
	if !strings.Contains(body, `name="restricted" value="true" checked`) {
		t.Fatalf("restricted channel must render the checkbox checked\nbody=%s", body)
	}

	// Open channel → checkbox rendered but unchecked.
	open := mkChannel(t, repo, "telegram", "@lojabot", "Vendas", true)
	rec2 := do(t, mux, http.MethodGet, "/settings/channels/"+open.ID.String()+"/edit", nil)
	body2 := rec2.Body.String()
	if !strings.Contains(body2, `name="restricted" value="true">`) {
		t.Fatalf("open channel must render the checkbox unchecked\nbody=%s", body2)
	}
}

// TestNewForm_NoRestrictedToggle: the create form defaults to open
// (backfill parity) — the restricted toggle is an edit-only maintenance
// action, so it must not appear on the new-channel form.
func TestNewForm_NoRestrictedToggle(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess(rosterUser("ana", "tenant_atendente")))
	rec := do(t, mux, http.MethodGet, "/settings/channels/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `name="restricted"`) {
		t.Fatalf("create form must not expose the restricted toggle")
	}
}

// TestUpdate_TogglesRestricted drives the maintenance edit: submitting the
// restricted checkbox flips the flag via Repository.SetRestricted;
// omitting it clears the flag.
func TestUpdate_TogglesRestricted(t *testing.T) {
	repo := newFakeRepo()
	u1 := rosterUser("ana", "tenant_atendente")
	acc := newFakeAccess(u1)
	ch := mkChannel(t, repo, "whatsapp", "+5511933334444", "Suporte", true)

	mux := newHandler(t, repo, acc)

	// Enable restricted.
	form := url.Values{}
	form.Set("name", "Suporte")
	form.Set("restricted", "true")
	form.Add("user_ids", u1.ID.String())
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if got, ok := repo.restricted[ch.ID]; !ok || !got {
		t.Fatalf("SetRestricted(true) not applied: restricted=%v ok=%v", got, ok)
	}
	// The refreshed list shows the Restrito badge.
	if !strings.Contains(rec.Body.String(), ">Restrito<") {
		t.Fatalf("refresh missing Restrito badge\nbody=%s", rec.Body.String())
	}

	// Disable restricted (checkbox omitted).
	form2 := url.Values{}
	form2.Set("name", "Suporte")
	form2.Add("user_ids", u1.ID.String())
	rec2 := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d", rec2.Code)
	}
	if got := repo.restricted[ch.ID]; got {
		t.Fatalf("SetRestricted(false) not applied: restricted=%v", got)
	}
}

// TestUpdate_SetRestrictedNotFound: a channel that vanishes between the
// Get and the SetRestricted (ErrNotFound) yields a 404, not a 500.
func TestUpdate_SetRestrictedNotFound(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "X", true)
	repo.restrictedErr = channels.ErrNotFound

	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "X")
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

// TestUpdate_SetRestrictedError: an unexpected store error on the
// restricted write surfaces as a 500 and does not proceed to the roster
// write.
func TestUpdate_SetRestrictedError(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess()
	ch := mkChannel(t, repo, "whatsapp", "+55", "X", true)
	repo.restrictedErr = errBoom

	mux := newHandler(t, repo, acc)
	form := url.Values{}
	form.Set("name", "X")
	rec := do(t, mux, http.MethodPost, "/settings/channels/"+ch.ID.String(), form)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if _, wrote := acc.replaced[ch.ID]; wrote {
		t.Fatalf("roster must not be written after a restricted-write failure")
	}
}
