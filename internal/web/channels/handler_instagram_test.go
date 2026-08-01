package channels_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestCreateInstagram_SavesAssociationUnderInstagramChannel mirrors
// handler_messenger_test.go's regression guard: SaveAssociation must be
// filed under the submitted channel_key, not a hardcoded constant — an
// Instagram Business Account id must land under channel="instagram" or
// the inbound webhook resolver could never resolve it (and could collide
// with a WhatsApp/Messenger id in the same (channel, association)
// keyspace).
func TestCreateInstagram_SavesAssociationUnderInstagramChannel(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	assoc := &fakeAssociations{}
	mux := newHandlerAssoc(t, repo, acc, assoc)

	form := url.Values{}
	form.Set("name", "Conta do Instagram")
	form.Set("channel_key", "instagram")
	form.Set("identity", "123456789012345")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.created) != 1 {
		t.Fatalf("want 1 channel created, got %d", len(repo.created))
	}
	if repo.created[0].ChannelKey != "instagram" {
		t.Fatalf("channel_key=%q, want instagram", repo.created[0].ChannelKey)
	}
	if len(assoc.calls) != 1 {
		t.Fatalf("want 1 SaveAssociation call, got %d", len(assoc.calls))
	}
	got := assoc.calls[0]
	if got.channel != "instagram" {
		t.Fatalf("association channel=%q, want instagram (not whatsapp)", got.channel)
	}
	if got.assoc != "123456789012345" {
		t.Fatalf("association=%q, want 123456789012345", got.assoc)
	}
}

func TestCreateInstagram_RejectsNonDigitIdentity(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	assoc := &fakeAssociations{}
	mux := newHandlerAssoc(t, repo, acc, assoc)

	form := url.Values{}
	form.Set("name", "Conta do Instagram")
	form.Set("channel_key", "instagram")
	form.Set("identity", "not-an-ig-id")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Fatalf("non-digit identity must not create a channel, got %d", len(repo.created))
	}
	if len(assoc.calls) != 0 {
		t.Fatalf("non-digit identity must not write an association, got %d", len(assoc.calls))
	}
	if !strings.Contains(rec.Body.String(), "ID da conta comercial do Instagram deve conter apenas dígitos.") {
		t.Fatalf("expected instagram-specific digit-validation bounce, body=%s", rec.Body.String())
	}
}

// TestCreateInstagram_IsSelectableType pins that "instagram" is offered in
// the create-form picker (not just accepted server-side).
func TestCreateInstagram_IsSelectableType(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess(rosterUser("ana", "tenant_atendente")))
	rec := do(t, mux, http.MethodGet, "/settings/channels/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `value="instagram"`) {
		t.Fatalf("instagram option must be present in the create form\nbody=%s", rec.Body.String())
	}
}
