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

// TestAssembleWebChannelsHandlerFlagged_ThreadsFlag pins that the boot flag
// reaches the rendered create form: the whatsapp_web option appears only
// when the assembled handler is flagged on.
func TestAssembleWebChannelsHandlerFlagged_ThreadsFlag(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{false, true} {
		h, err := assembleWebChannelsHandlerFlagged(memChannelsStore{}, nil, nil, on)
		if err != nil {
			t.Fatalf("assemble(on=%v): %v", on, err)
		}
		tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
		r := httptest.NewRequest(http.MethodGet, "/settings/channels/new", nil)
		r = r.WithContext(tenancy.WithContext(r.Context(), tenant))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("on=%v: status=%d, want 200", on, rec.Code)
		}
		gotOpt := strings.Contains(rec.Body.String(), `value="whatsapp_web"`)
		if gotOpt != on {
			t.Fatalf("on=%v: whatsapp_web option present=%v, want %v", on, gotOpt, on)
		}
	}
}
