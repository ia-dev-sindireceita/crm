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
