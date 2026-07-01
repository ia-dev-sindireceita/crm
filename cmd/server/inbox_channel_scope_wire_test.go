package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/channels"
	"github.com/pericles-luz/crm/internal/iam"
)

func TestIsGerenteFromSessionContext(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		sess *iam.Session
		want bool
	}{
		{"gerente", &iam.Session{Role: iam.RoleTenantGerente}, true},
		{"atendente", &iam.Session{Role: iam.RoleTenantAtendente}, false},
		{"no session fails safe to false", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.sess != nil {
				r = r.WithContext(middleware.WithSession(r.Context(), *tc.sess))
			}
			if got := isGerenteFromSessionContext(r); got != tc.want {
				t.Errorf("isGerente = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildInboxChannelScope_NilPoolDegrades(t *testing.T) {
	t.Parallel()
	// A nil pool cannot build the channels adapter → soft-degrade to nil
	// (the inbox surface falls back to the pre-P4 tenant-wide list).
	if got := buildInboxChannelScope(nil); got != nil {
		t.Errorf("buildInboxChannelScope(nil) = %v, want nil", got)
	}
}

func TestWireChannelResolver_NilGuards(t *testing.T) {
	t.Parallel()
	// Both nil-receiver and nil-pool are no-ops (must not panic).
	wireChannelResolver(nil, nil)
}

// fakeChannelPorts is a minimal in-memory channels.Repository +
// channels.ChannelAccessPolicy so the inboxChannelScope projection can be
// exercised without Postgres. It implements only what AccessService.
// AccessibleChannels reads (List + ListAccessibleChannelIDs); the other
// methods satisfy the interface with no-ops.
type fakeChannelPorts struct {
	list    []*channels.Channel
	granted []uuid.UUID
}

func (f *fakeChannelPorts) List(context.Context, uuid.UUID) ([]*channels.Channel, error) {
	return f.list, nil
}
func (f *fakeChannelPorts) Create(context.Context, *channels.Channel) error { return nil }
func (f *fakeChannelPorts) Rename(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (f *fakeChannelPorts) SetActive(context.Context, uuid.UUID, uuid.UUID, bool) error {
	return nil
}
func (f *fakeChannelPorts) SetRestricted(context.Context, uuid.UUID, uuid.UUID, bool) error {
	return nil
}
func (f *fakeChannelPorts) Get(context.Context, uuid.UUID, uuid.UUID) (*channels.Channel, error) {
	return nil, channels.ErrNotFound
}
func (f *fakeChannelPorts) CanAccessChannel(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	return false, nil
}
func (f *fakeChannelPorts) ListAccessibleChannelIDs(context.Context, uuid.UUID, uuid.UUID) ([]uuid.UUID, error) {
	return f.granted, nil
}

func TestInboxChannelScope_AccessibleChannelsProjection(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	user := uuid.New()
	open := channels.Hydrate(uuid.New(), tenant, "whatsapp", "a", "Suporte", true, false, time.Time{})

	ports := &fakeChannelPorts{list: []*channels.Channel{open}}
	access, err := channels.NewAccessService(ports, ports)
	if err != nil {
		t.Fatalf("NewAccessService: %v", err)
	}
	scope := inboxChannelScope{access: access}

	got, err := scope.AccessibleChannels(context.Background(), tenant, user, false)
	if err != nil {
		t.Fatalf("AccessibleChannels: %v", err)
	}
	if len(got) != 1 || got[0].ID != open.ID || got[0].DisplayName != "Suporte" {
		t.Errorf("projection = %+v, want [{%v Suporte}]", got, open.ID)
	}
}
