package channels_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestCreateMessenger_SavesAssociationUnderMessengerChannel is the
// regression guard for the bug-fix-in-passing found while generalizing the
// WhatsApp-only onboarding branches to also cover Messenger: the
// SaveAssociation call used to hardcode channel="whatsapp" regardless of
// the submitted channel_key. A Messenger Page ID must be filed under
// channel="messenger", or the inbound webhook could never resolve it (and
// worse, could collide with a WhatsApp phone_number_id in the same
// (channel, association) keyspace).
func TestCreateMessenger_SavesAssociationUnderMessengerChannel(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	assoc := &fakeAssociations{}
	mux := newHandlerAssoc(t, repo, acc, assoc)

	form := url.Values{}
	form.Set("name", "Página do Facebook")
	form.Set("channel_key", "messenger")
	form.Set("identity", "123456789012345")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.created) != 1 {
		t.Fatalf("want 1 channel created, got %d", len(repo.created))
	}
	if repo.created[0].ChannelKey != "messenger" {
		t.Fatalf("channel_key=%q, want messenger", repo.created[0].ChannelKey)
	}
	if len(assoc.calls) != 1 {
		t.Fatalf("want 1 SaveAssociation call, got %d", len(assoc.calls))
	}
	got := assoc.calls[0]
	if got.channel != "messenger" {
		t.Fatalf("association channel=%q, want messenger (not whatsapp)", got.channel)
	}
	if got.assoc != "123456789012345" {
		t.Fatalf("association=%q, want 123456789012345", got.assoc)
	}
}

func TestCreateMessenger_RejectsNonDigitIdentity(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	assoc := &fakeAssociations{}
	mux := newHandlerAssoc(t, repo, acc, assoc)

	form := url.Values{}
	form.Set("name", "Página do Facebook")
	form.Set("channel_key", "messenger")
	form.Set("identity", "not-a-page-id")
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
	if !strings.Contains(rec.Body.String(), "Page ID deve conter apenas dígitos.") {
		t.Fatalf("expected messenger-specific digit-validation bounce, body=%s", rec.Body.String())
	}
}

// TestCreateMessenger_IsSelectableType pins that "messenger" is offered in
// the create-form picker (not just accepted server-side) — regression guard
// for the closed channelTypes list.
func TestCreateMessenger_IsSelectableType(t *testing.T) {
	mux := newHandler(t, newFakeRepo(), newFakeAccess(rosterUser("ana", "tenant_atendente")))
	rec := do(t, mux, http.MethodGet, "/settings/channels/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `value="messenger"`) {
		t.Fatalf("messenger option must be present in the create form\nbody=%s", rec.Body.String())
	}
}
