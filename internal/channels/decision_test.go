package channels

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// timeZero is a stable created-at for Hydrate in these tests — the value
// is irrelevant to the access decision, so a zero time keeps the fixtures
// terse.
func timeZero() time.Time { return time.Time{} }

// TestDecide exhaustively covers the pure per-resource rule: the eight
// (isGerente, restricted, hasGrant) combinations. Gerente always wins;
// an open channel is visible regardless of grant; a restricted channel
// needs the grant.
func TestDecide(t *testing.T) {
	cases := []struct {
		isGerente  bool
		restricted bool
		hasGrant   bool
		want       bool
	}{
		// Gerente override: allowed in every combination.
		{true, false, false, true},
		{true, false, true, true},
		{true, true, false, true},
		{true, true, true, true},
		// Atendente, open channel: always allowed (grant irrelevant).
		{false, false, false, true},
		{false, false, true, true},
		// Atendente, restricted channel: gated by the explicit grant.
		{false, true, false, false},
		{false, true, true, true},
	}
	for _, c := range cases {
		if got := Decide(c.isGerente, c.restricted, c.hasGrant); got != c.want {
			t.Errorf("Decide(gerente=%v, restricted=%v, grant=%v) = %v, want %v",
				c.isGerente, c.restricted, c.hasGrant, got, c.want)
		}
	}
}

// ---- port fakes (compose-layer tests: AccessService touches no storage,
// it orchestrates the Repository + ChannelAccessPolicy ports, so faking
// the ports exercises the composition without a database) ---------------

type fakePolicyRepo struct {
	chans   map[uuid.UUID]*Channel
	listErr error
	getErr  error
}

func newFakePolicyRepo() *fakePolicyRepo {
	return &fakePolicyRepo{chans: map[uuid.UUID]*Channel{}}
}

func (f *fakePolicyRepo) add(c *Channel) { f.chans[c.ID] = c }

func (f *fakePolicyRepo) List(_ context.Context, _ uuid.UUID) ([]*Channel, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*Channel, 0, len(f.chans))
	for _, c := range f.chans {
		out = append(out, c)
	}
	// deterministic by id string for stable assertions
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].ID.String() < out[i].ID.String() {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func (f *fakePolicyRepo) Create(context.Context, *Channel) error { return nil }
func (f *fakePolicyRepo) Rename(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (f *fakePolicyRepo) SetActive(context.Context, uuid.UUID, uuid.UUID, bool) error {
	return nil
}
func (f *fakePolicyRepo) SetRestricted(context.Context, uuid.UUID, uuid.UUID, bool) error {
	return nil
}
func (f *fakePolicyRepo) Get(_ context.Context, _ uuid.UUID, id uuid.UUID) (*Channel, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.chans[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

type fakeGrants struct {
	grants  map[uuid.UUID]map[uuid.UUID]bool // channelID -> userID -> granted
	canErr  error
	listErr error
}

func newFakeGrants() *fakeGrants {
	return &fakeGrants{grants: map[uuid.UUID]map[uuid.UUID]bool{}}
}

func (f *fakeGrants) grant(channelID, userID uuid.UUID) {
	if f.grants[channelID] == nil {
		f.grants[channelID] = map[uuid.UUID]bool{}
	}
	f.grants[channelID][userID] = true
}

func (f *fakeGrants) CanAccessChannel(_ context.Context, _, userID, channelID uuid.UUID) (bool, error) {
	if f.canErr != nil {
		return false, f.canErr
	}
	return f.grants[channelID][userID], nil
}

func (f *fakeGrants) ListAccessibleChannelIDs(_ context.Context, _, userID uuid.UUID) ([]uuid.UUID, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []uuid.UUID
	for chID, users := range f.grants {
		if users[userID] {
			out = append(out, chID)
		}
	}
	return out, nil
}

func mustService(t *testing.T, repo Repository, grants ChannelAccessPolicy) *AccessService {
	t.Helper()
	s, err := NewAccessService(repo, grants)
	if err != nil {
		t.Fatalf("NewAccessService: %v", err)
	}
	return s
}

func TestNewAccessService_Validation(t *testing.T) {
	if _, err := NewAccessService(nil, newFakeGrants()); !errors.Is(err, ErrNilRepository) {
		t.Errorf("nil repo err = %v, want ErrNilRepository", err)
	}
	if _, err := NewAccessService(newFakePolicyRepo(), nil); !errors.Is(err, ErrNilAccessPolicy) {
		t.Errorf("nil policy err = %v, want ErrNilAccessPolicy", err)
	}
}

func TestAccessService_CanAccessChannel(t *testing.T) {
	tenant := uuid.New()
	atendente := uuid.New()
	granted := uuid.New()

	openCh := Hydrate(uuid.New(), tenant, "whatsapp", "a", "Open", true, false, timeZero())
	restrictedCh := Hydrate(uuid.New(), tenant, "whatsapp", "b", "Restricted", true, true, timeZero())

	repo := newFakePolicyRepo()
	repo.add(openCh)
	repo.add(restrictedCh)
	grants := newFakeGrants()
	grants.grant(restrictedCh.ID, granted)

	svc := mustService(t, repo, grants)
	ctx := context.Background()

	// Open channel: any atendente allowed, even without a grant.
	if ok, err := svc.CanAccessChannel(ctx, tenant, atendente, openCh.ID); err != nil || !ok {
		t.Errorf("open/atendente = (%v,%v), want (true,nil)", ok, err)
	}
	// Restricted channel: atendente without a grant is denied.
	if ok, err := svc.CanAccessChannel(ctx, tenant, atendente, restrictedCh.ID); err != nil || ok {
		t.Errorf("restricted/no-grant = (%v,%v), want (false,nil)", ok, err)
	}
	// Restricted channel: atendente with a grant is allowed.
	if ok, err := svc.CanAccessChannel(ctx, tenant, granted, restrictedCh.ID); err != nil || !ok {
		t.Errorf("restricted/granted = (%v,%v), want (true,nil)", ok, err)
	}
	// Gerente override: allowed on the restricted channel with no grant.
	if ok, err := svc.CanAccessChannelAsGerente(ctx, tenant, restrictedCh.ID); err != nil || !ok {
		t.Errorf("restricted/gerente = (%v,%v), want (true,nil)", ok, err)
	}
	// Unknown channel: deny even for a gerente (no phantom-id access).
	if ok, err := svc.CanAccessChannelAsGerente(ctx, tenant, uuid.New()); err != nil || ok {
		t.Errorf("unknown/gerente = (%v,%v), want (false,nil)", ok, err)
	}
	// Unknown channel: deny for an atendente too.
	if ok, err := svc.CanAccessChannel(ctx, tenant, atendente, uuid.New()); err != nil || ok {
		t.Errorf("unknown/atendente = (%v,%v), want (false,nil)", ok, err)
	}
	// Nil tenant is a clean error.
	if _, err := svc.CanAccessChannel(ctx, uuid.Nil, atendente, openCh.ID); err == nil {
		t.Error("nil tenant err = nil, want error")
	}
}

func TestAccessService_CanAccessChannel_PropagatesErrors(t *testing.T) {
	tenant := uuid.New()
	restrictedCh := Hydrate(uuid.New(), tenant, "whatsapp", "b", "R", true, true, timeZero())

	// Repository.Get error propagates.
	repo := newFakePolicyRepo()
	repo.getErr = errors.New("boom")
	if _, err := mustService(t, repo, newFakeGrants()).CanAccessChannel(context.Background(), tenant, uuid.New(), restrictedCh.ID); err == nil {
		t.Error("repo.Get error swallowed")
	}

	// Grant-lookup error propagates (only reached for restricted + atendente).
	repo2 := newFakePolicyRepo()
	repo2.add(restrictedCh)
	grants := newFakeGrants()
	grants.canErr = errors.New("grant boom")
	if _, err := mustService(t, repo2, grants).CanAccessChannel(context.Background(), tenant, uuid.New(), restrictedCh.ID); err == nil {
		t.Error("grant lookup error swallowed")
	}
}

func TestAccessService_AccessibleChannelIDs(t *testing.T) {
	tenant := uuid.New()
	atendente := uuid.New()

	open1 := Hydrate(uuid.New(), tenant, "whatsapp", "a", "Open1", true, false, timeZero())
	open2 := Hydrate(uuid.New(), tenant, "telegram", "b", "Open2", true, false, timeZero())
	restrictedGranted := Hydrate(uuid.New(), tenant, "whatsapp", "c", "RG", true, true, timeZero())
	restrictedDenied := Hydrate(uuid.New(), tenant, "whatsapp", "d", "RD", true, true, timeZero())

	repo := newFakePolicyRepo()
	for _, c := range []*Channel{open1, open2, restrictedGranted, restrictedDenied} {
		repo.add(c)
	}
	grants := newFakeGrants()
	grants.grant(restrictedGranted.ID, atendente)
	svc := mustService(t, repo, grants)
	ctx := context.Background()

	// Atendente: both open channels + the one restricted grant, never the denied one.
	got, err := svc.AccessibleChannelIDs(ctx, tenant, atendente, false)
	if err != nil {
		t.Fatalf("AccessibleChannelIDs(atendente): %v", err)
	}
	if !containsAll(got, open1.ID, open2.ID, restrictedGranted.ID) {
		t.Errorf("atendente missing expected channels: got %v", got)
	}
	if contains(got, restrictedDenied.ID) {
		t.Errorf("atendente leaked denied restricted channel: got %v", got)
	}
	if len(got) != 3 {
		t.Errorf("atendente accessible count = %d, want 3 (%v)", len(got), got)
	}

	// Gerente: every channel, restricted or not, granted or not.
	gerGot, err := svc.AccessibleChannelIDs(ctx, tenant, uuid.Nil, true)
	if err != nil {
		t.Fatalf("AccessibleChannelIDs(gerente): %v", err)
	}
	if len(gerGot) != 4 {
		t.Errorf("gerente accessible count = %d, want 4 (%v)", len(gerGot), gerGot)
	}

	// Nil tenant is a clean error.
	if _, err := svc.AccessibleChannelIDs(ctx, uuid.Nil, atendente, false); err == nil {
		t.Error("nil tenant err = nil, want error")
	}
}

func TestAccessService_AccessibleChannelIDs_PropagatesErrors(t *testing.T) {
	tenant := uuid.New()
	repo := newFakePolicyRepo()
	repo.listErr = errors.New("list boom")
	if _, err := mustService(t, repo, newFakeGrants()).AccessibleChannelIDs(context.Background(), tenant, uuid.New(), false); err == nil {
		t.Error("repo.List error swallowed")
	}

	repo2 := newFakePolicyRepo()
	repo2.add(Hydrate(uuid.New(), tenant, "whatsapp", "a", "R", true, true, timeZero()))
	grants := newFakeGrants()
	grants.listErr = errors.New("grant list boom")
	if _, err := mustService(t, repo2, grants).AccessibleChannelIDs(context.Background(), tenant, uuid.New(), false); err == nil {
		t.Error("grant list error swallowed")
	}
}

func contains(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func containsAll(ids []uuid.UUID, wants ...uuid.UUID) bool {
	for _, w := range wants {
		if !contains(ids, w) {
			return false
		}
	}
	return true
}
