package channels_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	webchannels "github.com/pericles-luz/crm/internal/web/channels"
)

// fakeAssociations records SaveAssociation calls for the SIN-67143
// WhatsApp-API onboarding tests. A non-nil err makes the write fail so the
// handler's 500 path can be exercised.
type fakeAssociations struct {
	calls []assocCall
	err   error
}

type assocCall struct {
	tenantID uuid.UUID
	channel  string
	assoc    string
}

func (f *fakeAssociations) SaveAssociation(_ context.Context, tenantID uuid.UUID, channel, association string) error {
	f.calls = append(f.calls, assocCall{tenantID: tenantID, channel: channel, assoc: association})
	return f.err
}

// newHandlerAssoc builds a channels handler with an association writer wired
// (SIN-67143), so the WhatsApp-API onboarding guard + upsert can be
// exercised. Mirrors newHandler but threads Associations through Deps.
func newHandlerAssoc(t *testing.T, repo *fakeRepo, acc *fakeAccess, assoc webchannels.ChannelAssociationWriter) http.Handler {
	t.Helper()
	h, err := webchannels.New(webchannels.Deps{Channels: repo, Access: acc, Associations: assoc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

// TestCreateWhatsAppAPI_SavesAssociation pins AC #1/#3: creating a whatsapp
// (WhatsApp API) channel with an all-digit identity persists the channel AND
// upserts the (whatsapp, phone_number_id) → tenant association so the inbound
// webhook can later resolve the tenant.
func TestCreateWhatsAppAPI_SavesAssociation(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	assoc := &fakeAssociations{}
	mux := newHandlerAssoc(t, repo, acc, assoc)

	form := url.Values{}
	form.Set("name", "Suporte")
	form.Set("channel_key", "whatsapp")
	form.Set("identity", "5511999990000")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.created) != 1 {
		t.Fatalf("want 1 channel created, got %d", len(repo.created))
	}
	if len(assoc.calls) != 1 {
		t.Fatalf("want 1 SaveAssociation call, got %d", len(assoc.calls))
	}
	got := assoc.calls[0]
	if got.tenantID != testTenant.ID {
		t.Errorf("association tenantID=%v, want %v", got.tenantID, testTenant.ID)
	}
	if got.channel != "whatsapp" {
		t.Errorf("association channel=%q, want whatsapp", got.channel)
	}
	if got.assoc != "5511999990000" {
		t.Errorf("association=%q, want 5511999990000", got.assoc)
	}
}

// TestCreateWhatsAppAPI_RejectsNonDigitIdentity pins AC #2: a whatsapp create
// whose identity contains non-digits bounces the modal with a field error and
// persists nothing (no channel, no association).
func TestCreateWhatsAppAPI_RejectsNonDigitIdentity(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	assoc := &fakeAssociations{}
	mux := newHandlerAssoc(t, repo, acc, assoc)

	form := url.Values{}
	form.Set("name", "Suporte")
	form.Set("channel_key", "whatsapp")
	form.Set("identity", "+55 11 99999-0000")
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
	if !strings.Contains(rec.Body.String(), "phone_number_id deve conter apenas dígitos.") {
		t.Fatalf("expected digit-validation bounce, body=%s", rec.Body.String())
	}
}

// TestCreateWhatsAppAPI_AssociationWriteError pins the failure path: when the
// association upsert errors, the handler returns 500 and does not proceed to
// the access-roster replace.
func TestCreateWhatsAppAPI_AssociationWriteError(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	assoc := &fakeAssociations{err: errors.New("db down")}
	mux := newHandlerAssoc(t, repo, acc, assoc)

	form := url.Values{}
	form.Set("name", "Suporte")
	form.Set("channel_key", "whatsapp")
	form.Set("identity", "5511999990000")
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if len(assoc.calls) != 1 {
		t.Fatalf("want 1 SaveAssociation attempt, got %d", len(assoc.calls))
	}
	// The channel row was created before the association write; the access
	// roster replace must not run once the association write fails.
	if len(acc.replaced) != 0 {
		t.Fatalf("access replace must not run after association write fails, got %d", len(acc.replaced))
	}
}

// TestCreateWhatsAppAPI_NilWriterSkipsOnboarding documents the feature-flag
// posture: with no association writer wired (Associations nil), a whatsapp
// create skips BOTH the digit guard and the association upsert, so existing
// behaviour is preserved for a fail-soft deployment. Uses a non-digit
// identity to prove the guard is inactive when the port is nil.
func TestCreateWhatsAppAPI_NilWriterSkipsOnboarding(t *testing.T) {
	repo := newFakeRepo()
	acc := newFakeAccess(rosterUser("ana", "tenant_atendente"))
	mux := newHandler(t, repo, acc) // nil Associations

	form := url.Values{}
	form.Set("name", "Suporte")
	form.Set("channel_key", "whatsapp")
	form.Set("identity", "+5511999990000") // non-digit, but guard inactive
	rec := do(t, mux, http.MethodPost, "/settings/channels", form)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.created) != 1 {
		t.Fatalf("nil writer must still create the channel, got %d", len(repo.created))
	}
}
