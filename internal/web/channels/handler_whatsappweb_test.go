package channels_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	webchannels "github.com/pericles-luz/crm/internal/web/channels"
)

// newHandlerFlag builds a channels handler with the WhatsApp-Web feature
// flag set explicitly, so the flag-gated create guard can be exercised in
// both states. It mirrors newHandler (flag OFF) but threads
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

// TestNewForm_WhatsAppWebAlwaysVisible pins AC #1 (SIN-66459/66468): the
// "WhatsApp Web" option is ALWAYS offered in the create <select>, separate
// from "WhatsApp API", regardless of the FEATURE_WHATSAPP_WEB_ENABLED flag.
// Visibility is decoupled from functional readiness — the flag gates the
// create guard (see TestCreate_WhatsAppWebFunctionalGate), never the picker.
func TestNewForm_WhatsAppWebAlwaysVisible(t *testing.T) {
	cases := []struct {
		name  string
		wsWeb bool
	}{
		{name: "flag off still shows whatsapp_web", wsWeb: false},
		{name: "flag on shows whatsapp_web", wsWeb: true},
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
			// "WhatsApp Web" option + human label are present in both flag states.
			if !strings.Contains(body, `value="whatsapp_web"`) {
				t.Fatalf("expected whatsapp_web option regardless of flag, body=%s", body)
			}
			if !strings.Contains(body, ">WhatsApp Web<") {
				t.Fatalf("expected WhatsApp Web label regardless of flag, body=%s", body)
			}
		})
	}
}

// TestCreate_WhatsAppWebFunctionalGate pins AC #2/#3 (SIN-66468): with the
// flag OFF, a POST for channel_key=whatsapp_web is bounced with a clear
// "em implementação" message and NOTHING is persisted (no broken/half-wired
// QR channel silently created); with the flag ON it persists a channel whose
// stored channel_key is exactly "whatsapp_web". The legacy "whatsapp" key
// keeps working in both states (zero data migration).
func TestCreate_WhatsAppWebFunctionalGate(t *testing.T) {
	cases := []struct {
		name       string
		wsWeb      bool
		key        string
		wantCreate bool
		wantKey    string
	}{
		{name: "flag off bounces whatsapp_web", wsWeb: false, key: "whatsapp_web", wantCreate: false},
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
					t.Fatalf("flag-gated whatsapp_web must not create, got %d", len(repo.created))
				}
				// The bounce shows the readiness message, NOT the invalid-type
				// error (whatsapp_web is a valid, visible type — it's just not
				// functionally ready yet).
				if !strings.Contains(rec.Body.String(), "em implementação") {
					t.Fatalf("expected 'em implementação' readiness message, body=%s", rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "tipo de canal válido") {
					t.Fatalf("whatsapp_web must not read as an invalid type, body=%s", rec.Body.String())
				}
			}
		})
	}
}

// TestCreate_ForgedUnknownType keeps deny-by-default coverage: a channel_key
// outside the closed set is still rejected as an invalid type and nothing is
// persisted, independent of the WhatsApp-Web flag.
func TestCreate_ForgedUnknownType(t *testing.T) {
	for _, wsWeb := range []bool{false, true} {
		repo := newFakeRepo()
		mux := newHandlerFlag(t, repo, newFakeAccess(rosterUser("ana", "tenant_atendente")), wsWeb)
		form := url.Values{}
		form.Set("name", "Canal")
		form.Set("channel_key", "sms_forged")
		form.Set("identity", "+5511900000000")
		rec := do(t, mux, http.MethodPost, "/settings/channels", form)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		if len(repo.created) != 0 {
			t.Fatalf("forged type must not create (wsWeb=%v), got %d", wsWeb, len(repo.created))
		}
		if !strings.Contains(rec.Body.String(), "tipo de canal válido") {
			t.Fatalf("expected invalid-type error for forged key (wsWeb=%v), body=%s", wsWeb, rec.Body.String())
		}
	}
}
