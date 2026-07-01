package main

// SIN-66391 (P2) — channels wire tests. The handler covers its own
// behaviour exhaustively in internal/web/channels; these tests pin the
// composition root: buildWebChannelsHandler returns (nil, no-op) when the
// DSN is unset, the pure assembly seam rejects a nil store, and the
// assembled mux mounts every route the surface lists.

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
)

// memChannelsStore satisfies the channelsStore union with empty results.
type memChannelsStore struct{}

func (memChannelsStore) List(_ context.Context, _ uuid.UUID) ([]*channels.Channel, error) {
	return nil, nil
}
func (memChannelsStore) Create(_ context.Context, _ *channels.Channel) error { return nil }
func (memChannelsStore) Rename(_ context.Context, _, _ uuid.UUID, _ string) error {
	return channels.ErrNotFound
}
func (memChannelsStore) SetActive(_ context.Context, _, _ uuid.UUID, _ bool) error {
	return channels.ErrNotFound
}
func (memChannelsStore) SetRestricted(_ context.Context, _, _ uuid.UUID, _ bool) error {
	return channels.ErrNotFound
}
func (memChannelsStore) Get(_ context.Context, _, _ uuid.UUID) (*channels.Channel, error) {
	return nil, channels.ErrNotFound
}
func (memChannelsStore) ListRosterUsers(_ context.Context, _ uuid.UUID) ([]channels.RosterUser, error) {
	return nil, nil
}
func (memChannelsStore) ChannelUserIDs(_ context.Context, _, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (memChannelsStore) ReplaceAccess(_ context.Context, _, _ uuid.UUID, _ []uuid.UUID) error {
	return nil
}

func TestBuildWebChannelsHandler_DegradesWhenDSNUnset(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebChannelsHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when DATABASE_URL unset; got %T", h)
	}
}

func TestAssembleWebChannelsHandler_RejectsNilStore(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebChannelsHandler(nil, nil, nil); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestAssembleWebChannelsHandler_MountsRegistry(t *testing.T) {
	t.Parallel()
	h, err := assembleWebChannelsHandler(memChannelsStore{}, nil, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	r := httptest.NewRequest(http.MethodGet, "/settings/channels", nil)
	r = r.WithContext(tenancy.WithContext(r.Context(), tenant))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/channels status=%d, want 200", rec.Code)
	}
}

// TestWhatsAppWebEnabled_ParsesFlag pins the boot-time env parse: only the
// exact "1" (trimmed) enables the flag; everything else (unset, "0",
// "true", nil getenv) is OFF, matching the FEATURE_WEBCHAT_ENABLED
// convention and keeping prod deny-by-default.
func TestWhatsAppWebEnabled_ParsesFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{name: "unset", val: "", want: false},
		{name: "one enables", val: "1", want: true},
		{name: "padded one enables", val: " 1 ", want: true},
		{name: "zero disables", val: "0", want: false},
		{name: "true string disables", val: "true", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := whatsappWebEnabled(func(k string) string {
				if k == EnvWhatsAppWebEnabled {
					return tc.val
				}
				return ""
			})
			if got != tc.want {
				t.Fatalf("whatsappWebEnabled(%q)=%v, want %v", tc.val, got, tc.want)
			}
		})
	}
	if whatsappWebEnabled(nil) {
		t.Fatal("nil getenv must be OFF")
	}
}

// recordingChannelsStore embeds the no-op memChannelsStore and records
// whether Create was called, so the wire test can assert that the WhatsApp
// Web functional-readiness guard persists NOTHING when the flag is OFF.
type recordingChannelsStore struct {
	memChannelsStore
	createCalls int
	lastKey     string
}

func (s *recordingChannelsStore) Create(_ context.Context, ch *channels.Channel) error {
	s.createCalls++
	if ch != nil {
		s.lastKey = ch.ChannelKey
	}
	return nil
}

// TestAssembleWebChannelsHandlerFlagged_ThreadsFlag pins that the boot flag
// gates FUNCTIONAL readiness, not visibility (SIN-66459/66468):
//
//   - The "whatsapp_web" option is ALWAYS offered in the create form,
//     regardless of the flag — the WhatsApp API vs WhatsApp Web distinction
//     is the deliverable and must be visible at all times (AC #1).
//   - The flag still threads through the functional create path: a POST of
//     channel_key=whatsapp_web with the flag OFF is bounced with the
//     "em implementação" message and persists nothing (AC #3); with the flag
//     ON it falls through to the real create and persists the channel.
func TestAssembleWebChannelsHandlerFlagged_ThreadsFlag(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		store := &recordingChannelsStore{}
		h, err := assembleWebChannelsHandlerFlagged(store, nil, nil, on)
		if err != nil {
			t.Fatalf("assemble(on=%v): %v", on, err)
		}
		tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}

		// (a) The option is always visible, independent of the flag.
		rNew := httptest.NewRequest(http.MethodGet, "/settings/channels/new", nil)
		rNew = rNew.WithContext(tenancy.WithContext(rNew.Context(), tenant))
		recNew := httptest.NewRecorder()
		h.ServeHTTP(recNew, rNew)
		if recNew.Code != http.StatusOK {
			t.Fatalf("on=%v: GET /new status=%d, want 200", on, recNew.Code)
		}
		if !strings.Contains(recNew.Body.String(), `value="whatsapp_web"`) {
			t.Fatalf("on=%v: whatsapp_web option must always be present in the create form", on)
		}

		// (b) The flag threads through the functional create path.
		form := url.Values{}
		form.Set("name", "Suporte WhatsApp Web")
		form.Set("channel_key", "whatsapp_web")
		form.Set("identity", "+5511999999999")
		rPost := httptest.NewRequest(http.MethodPost, "/settings/channels", strings.NewReader(form.Encode()))
		rPost.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rPost = rPost.WithContext(tenancy.WithContext(rPost.Context(), tenant))
		recPost := httptest.NewRecorder()
		h.ServeHTTP(recPost, rPost)

		if on {
			if store.createCalls != 1 {
				t.Fatalf("on=true: whatsapp_web submit must persist a channel; Create calls=%d, want 1", store.createCalls)
			}
			if store.lastKey != "whatsapp_web" {
				t.Fatalf("on=true: persisted channel_key=%q, want whatsapp_web", store.lastKey)
			}
		} else {
			if store.createCalls != 0 {
				t.Fatalf("on=false: whatsapp_web submit must persist NOTHING; Create calls=%d, want 0", store.createCalls)
			}
			if !strings.Contains(recPost.Body.String(), "em implementação") {
				t.Fatalf("on=false: whatsapp_web submit must bounce with the 'em implementação' readiness message; body=%q", recPost.Body.String())
			}
		}
	}
}
