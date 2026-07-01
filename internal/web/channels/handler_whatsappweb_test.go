package channels_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	webchannels "github.com/pericles-luz/crm/internal/web/channels"
)

// newHandlerFlag builds a channels handler with the WhatsApp-Web feature
// flag set explicitly, so the flag-gated create form / guard can be
// exercised in both states. It mirrors newHandler (flag OFF) but threads
// WhatsAppWebEnabled through Deps.
func newHandlerFlag(t *testing.T, repo *fakeRepo, acc *fakeAccess, wsWeb bool) http.Handler {
	t.Helper()
	h, err := webchannels.New(webchannels.Deps{
		Channels:           repo,
		Access:             acc,
		WhatsAppWebEnabled: wsWeb,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

// TestNewForm_WhatsAppWebFlagGate pins AC 1/2: the create <select> shows
// "WhatsApp API" always, and offers "WhatsApp Web" only when the flag is
// on. Table-driven over the flag state × the option strings expected.
func TestNewForm_WhatsAppWebFlagGate(t *testing.T) {
	cases := []struct {
		name       string
		wsWeb      bool
		wantWebOpt bool
	}{
		{name: "flag off hides whatsapp_web", wsWeb: false, wantWebOpt: false},
		{name: "flag on shows whatsapp_web", wsWeb: true, wantWebOpt: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := newHandlerFlag(t, newFakeRepo(), newFakeAccess(rosterUser("ana", "tenant_atendente")), tc.wsWeb)
			rec := do(t, mux, http.MethodGet, "/settings/channels/new", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d", rec.Code)
			}
			body := rec.Body.String()
			// "WhatsApp API" label is always present (relabelled legacy key).
			if !strings.Contains(body, "WhatsApp API") {
				t.Fatalf("expected WhatsApp API option, body=%s", body)
			}
			gotWebOpt := strings.Contains(body, `value="whatsapp_web"`)
			if gotWebOpt != tc.wantWebOpt {
				t.Fatalf("whatsapp_web option present=%v, want %v\nbody=%s", gotWebOpt, tc.wantWebOpt, body)
			}
			// The human "WhatsApp Web" label tracks the option presence.
			if strings.Contains(body, ">WhatsApp Web<") != tc.wantWebOpt {
				t.Fatalf("WhatsApp Web label present=%v, want %v", !tc.wantWebOpt, tc.wantWebOpt)
			}
		})
	}
}

// TestCreate_WhatsAppWebFlagGate pins AC 2/3: with the flag OFF a POST
// forging channel_key=whatsapp_web is rejected (deny-by-default, nothing
// persisted); with the flag ON it persists a channel whose stored
// channel_key is exactly "whatsapp_web". The legacy "whatsapp" key keeps
// working in both states (zero data migration).
func TestCreate_WhatsAppWebFlagGate(t *testing.T) {
	cases := []struct {
		name       string
		wsWeb      bool
		key        string
		wantCreate bool
		wantKey    string
	}{
		{name: "flag off rejects whatsapp_web", wsWeb: false, key: "whatsapp_web", wantCreate: false},
		{name: "flag on accepts whatsapp_web", wsWeb: true, key: "whatsapp_web", wantCreate: true, wantKey: "whatsapp_web"},
		{name: "flag off still accepts whatsapp", wsWeb: false, key: "whatsapp", wantCreate: true, wantKey: "whatsapp"},
		{name: "flag on still accepts whatsapp", wsWeb: true, key: "whatsapp", wantCreate: true, wantKey: "whatsapp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			mux := newHandlerFlag(t, repo, newFakeAccess(rosterUser("ana", "tenant_atendente")), tc.wsWeb)
			form := url.Values{}
			form.Set("name", "Canal")
			form.Set("channel_key", tc.key)
			form.Set("identity", "+5511900000000")
			rec := do(t, mux, http.MethodPost, "/settings/channels", form)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
			}
			if tc.wantCreate {
				if len(repo.created) != 1 {
					t.Fatalf("want 1 channel created, got %d", len(repo.created))
				}
				if got := repo.created[0].ChannelKey; got != tc.wantKey {
					t.Fatalf("stored channel_key=%q, want %q", got, tc.wantKey)
				}
			} else {
				if len(repo.created) != 0 {
					t.Fatalf("flag-gated type must not create, got %d", len(repo.created))
				}
				if !strings.Contains(rec.Body.String(), "tipo de canal válido") {
					t.Fatalf("expected type-validation error, body=%s", rec.Body.String())
				}
			}
		})
	}
}
