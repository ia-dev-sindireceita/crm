package usecase_test

// SIN-62936 — unit tests for ApplyMasterGrantService. Pure: an
// in-process fake wallet.Repository + fake billing.SubscriptionRepository
// + the existing fakeWalletRepo style for master grants. The
// postgres path is exercised by the integration tests in
// internal/adapter/db/postgres/wallet_master_grant_apply_test.go
// (CA #2 / CA #3 from the parent C10 ticket).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

// ---------- fakes ----------------------------------------------------

type fakeApplyGrantRepo struct {
	mu      sync.Mutex
	rows    map[uuid.UUID]*wallet.MasterGrant
	consume struct {
		id   uuid.UUID
		ref  string
		when time.Time
		err  error
	}
}

func newFakeApplyGrantRepo() *fakeApplyGrantRepo {
	return &fakeApplyGrantRepo{rows: map[uuid.UUID]*wallet.MasterGrant{}}
}

func (f *fakeApplyGrantRepo) seed(g *wallet.MasterGrant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[g.ID()] = g
}

func (f *fakeApplyGrantRepo) Create(_ context.Context, g *wallet.MasterGrant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[g.ID()] = g
	return nil
}

func (f *fakeApplyGrantRepo) GetByID(_ context.Context, id uuid.UUID) (*wallet.MasterGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.rows[id]
	if !ok {
		return nil, wallet.ErrNotFound
	}
	return g, nil
}

func (f *fakeApplyGrantRepo) ListByTenant(_ context.Context, _ uuid.UUID) ([]*wallet.MasterGrant, error) {
	return nil, nil
}

func (f *fakeApplyGrantRepo) Revoke(_ context.Context, id, by uuid.UUID, reason string, when time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.rows[id]
	if !ok {
		return wallet.ErrNotFound
	}
	return g.Revoke(by, reason, when)
}

func (f *fakeApplyGrantRepo) Consume(_ context.Context, id uuid.UUID, ref string, when time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consume.id = id
	f.consume.ref = ref
	f.consume.when = when
	if f.consume.err != nil {
		return f.consume.err
	}
	g, ok := f.rows[id]
	if !ok {
		return wallet.ErrNotFound
	}
	return g.Consume(ref, when)
}

type fakeSubscriptionRepo struct {
	mu          sync.Mutex
	byTenant    map[uuid.UUID]*billing.Subscription
	saved       []*billing.Subscription
	savedActors []uuid.UUID
	getErr      error
	saveErr     error
}

func newFakeSubscriptionRepo() *fakeSubscriptionRepo {
	return &fakeSubscriptionRepo{byTenant: map[uuid.UUID]*billing.Subscription{}}
}

func (f *fakeSubscriptionRepo) seedActive(tenantID, planID uuid.UUID, start, end, now time.Time) *billing.Subscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub, err := billing.NewSubscription(tenantID, planID, start, end, now)
	if err != nil {
		panic(err)
	}
	f.byTenant[tenantID] = sub
	return sub
}

func (f *fakeSubscriptionRepo) GetByTenant(_ context.Context, tenantID uuid.UUID) (*billing.Subscription, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if tenantID == uuid.Nil {
		return nil, billing.ErrZeroTenant
	}
	sub, ok := f.byTenant[tenantID]
	if !ok {
		return nil, billing.ErrNotFound
	}
	return sub, nil
}

func (f *fakeSubscriptionRepo) SaveSubscription(_ context.Context, s *billing.Subscription, actor uuid.UUID) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, s)
	f.savedActors = append(f.savedActors, actor)
	return nil
}

// ---------- helpers --------------------------------------------------

func newApplyService(t *testing.T, grants wallet.MasterGrantRepository, wr wallet.Repository, subs billing.SubscriptionRepository, now time.Time) *usecase.ApplyMasterGrantService {
	t.Helper()
	svc, err := usecase.NewApplyMasterGrantService(grants, wr, subs, func() time.Time { return now }, uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	if err != nil {
		t.Fatalf("NewApplyMasterGrantService: %v", err)
	}
	return svc
}

func newFreePeriodGrant(t *testing.T, tenant, actor uuid.UUID, days int, createdAt time.Time) *wallet.MasterGrant {
	t.Helper()
	g, err := wallet.NewMasterGrant(tenant, actor, wallet.KindFreeSubscriptionPeriod, map[string]any{"period_days": days}, "free period for integration test", createdAt)
	if err != nil {
		t.Fatalf("NewMasterGrant: %v", err)
	}
	return g
}

func newExtraTokensGrant(t *testing.T, tenant, actor uuid.UUID, amount int64, createdAt time.Time) *wallet.MasterGrant {
	t.Helper()
	g, err := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens, map[string]any{"amount": amount}, "extra tokens for integration test", createdAt)
	if err != nil {
		t.Fatalf("NewMasterGrant: %v", err)
	}
	return g
}

// ---------- constructor ---------------------------------------------

func TestNewApplyMasterGrantService_RejectsBadDeps(t *testing.T) {
	t.Parallel()
	good := newFakeApplyGrantRepo()
	wrepo := newFakeRepo()
	subs := newFakeSubscriptionRepo()
	actor := uuid.New()
	if _, err := usecase.NewApplyMasterGrantService(nil, wrepo, subs, nil, actor); err == nil {
		t.Error("nil grants: want error")
	}
	if _, err := usecase.NewApplyMasterGrantService(good, nil, subs, nil, actor); err == nil {
		t.Error("nil wallet repo: want error")
	}
	if _, err := usecase.NewApplyMasterGrantService(good, wrepo, nil, nil, actor); err == nil {
		t.Error("nil subscriptions: want error")
	}
	if _, err := usecase.NewApplyMasterGrantService(good, wrepo, subs, nil, uuid.Nil); err == nil {
		t.Error("zero actor: want error")
	}
	svc, err := usecase.NewApplyMasterGrantService(good, wrepo, subs, nil, actor)
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if svc == nil {
		t.Fatal("svc is nil")
	}
}

// ---------- free_subscription_period ---------------------------------

func TestApply_FreePeriod_ExtendsSubscriptionWithoutInvoice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)

	tenant := uuid.New()
	actor := uuid.New()
	periodStart := now.Add(-15 * 24 * time.Hour)
	periodEnd := now.Add(15 * 24 * time.Hour)

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	sub := subs.seedActive(tenant, uuid.New(), periodStart, periodEnd, now.Add(-time.Hour))
	originalEnd := sub.CurrentPeriodEnd()

	g := newFreePeriodGrant(t, tenant, actor, 30, now.Add(-time.Minute))
	grants.seed(g)

	svc := newApplyService(t, grants, newFakeRepo(), subs, now)

	applied, err := svc.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied {
		t.Fatal("Apply returned (false, nil) on fresh grant; want (true, nil)")
	}
	if len(subs.saved) != 1 {
		t.Fatalf("SaveSubscription calls = %d, want 1", len(subs.saved))
	}
	saved := subs.saved[0]
	want := originalEnd.Add(30 * 24 * time.Hour)
	if !saved.CurrentPeriodEnd().Equal(want) {
		t.Errorf("current_period_end = %s, want %s", saved.CurrentPeriodEnd(), want)
	}
	if saved.Status() != billing.SubscriptionStatusActive {
		t.Errorf("status = %s, want active", saved.Status())
	}
	if grants.consume.id != g.ID() {
		t.Errorf("consume id = %s, want %s", grants.consume.id, g.ID())
	}
	if grants.consume.ref != sub.ID().String() {
		t.Errorf("consume ref = %s, want %s", grants.consume.ref, sub.ID())
	}
	if !grants.consume.when.Equal(now) {
		t.Errorf("consume when = %s, want %s", grants.consume.when, now)
	}
}

func TestApply_FreePeriod_MissingActiveSubscription(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo() // no seeded subscription
	g := newFreePeriodGrant(t, tenant, actor, 30, now)
	grants.seed(g)

	svc := newApplyService(t, grants, newFakeRepo(), subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if applied {
		t.Error("Apply returned applied=true on missing subscription")
	}
	if err == nil || !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("Apply: got %v, want billing.ErrNotFound", err)
	}
	if grants.consume.id != uuid.Nil {
		t.Error("Consume must not be called when downstream write fails")
	}
}

func TestApply_FreePeriod_InvalidPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	g, _ := wallet.NewMasterGrant(tenant, actor, wallet.KindFreeSubscriptionPeriod, map[string]any{"period_days": 0}, "missing payload integration test", now)
	grants.seed(g)

	svc := newApplyService(t, grants, newFakeRepo(), subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if applied {
		t.Error("Apply returned applied=true on invalid payload")
	}
	if !errors.Is(err, usecase.ErrInvalidGrantPayload) {
		t.Errorf("Apply: got %v, want ErrInvalidGrantPayload", err)
	}
}

func TestApply_FreePeriod_CancelledSubscriptionFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	sub := subs.seedActive(tenant, uuid.New(), now.Add(-24*time.Hour), now.Add(24*time.Hour), now)
	if err := sub.Cancel(now); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	g := newFreePeriodGrant(t, tenant, actor, 30, now)
	grants.seed(g)

	svc := newApplyService(t, grants, newFakeRepo(), subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if applied {
		t.Error("applied = true on cancelled subscription")
	}
	if !errors.Is(err, billing.ErrInvalidTransition) {
		t.Errorf("Apply: got %v, want billing.ErrInvalidTransition", err)
	}
	if grants.consume.id != uuid.Nil {
		t.Error("Consume must not be called when ExtendPeriod fails")
	}
}

// ---------- extra_tokens --------------------------------------------

func TestApply_ExtraTokens_CreditsWalletWithMasterGrantSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)

	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	wrepo := newFakeRepo()
	wrepo.seed(tenant, 0, now)
	subs := newFakeSubscriptionRepo()

	g := newExtraTokensGrant(t, tenant, actor, 1_000_000, now.Add(-time.Minute))
	grants.seed(g)

	svc := newApplyService(t, grants, wrepo, subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !applied {
		t.Fatal("applied = false on fresh grant")
	}
	if wrepo.ledgerCount() != 1 {
		t.Fatalf("ledger rows = %d, want 1", wrepo.ledgerCount())
	}
	entry := wrepo.ledger[0]
	if entry.Kind != wallet.KindGrant {
		t.Errorf("entry.Kind = %s, want grant", entry.Kind)
	}
	if entry.Amount != 1_000_000 {
		t.Errorf("entry.Amount = %d, want 1_000_000", entry.Amount)
	}
	if entry.Source != wallet.SourceMasterGrant {
		t.Errorf("entry.Source = %s, want master_grant", entry.Source)
	}
	if entry.MasterGrantID == nil || *entry.MasterGrantID != g.ID() {
		t.Errorf("entry.MasterGrantID = %v, want %s", entry.MasterGrantID, g.ID())
	}
	if entry.IdempotencyKey != "master_grant:"+g.ExternalID() {
		t.Errorf("entry.IdempotencyKey = %s, want master_grant:%s", entry.IdempotencyKey, g.ExternalID())
	}
	wid := wrepo.byTenant[tenant]
	bal, _, _ := wrepo.snapshotBalance(wid)
	if bal != 1_000_000 {
		t.Errorf("balance = %d, want 1_000_000", bal)
	}
	if grants.consume.ref != entry.ID.String() {
		t.Errorf("consume.ref = %s, want %s", grants.consume.ref, entry.ID)
	}
}

func TestApply_ExtraTokens_MissingWallet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	g := newExtraTokensGrant(t, tenant, actor, 1000, now)
	grants.seed(g)

	svc := newApplyService(t, grants, newFakeRepo(), subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if applied {
		t.Error("applied=true with no wallet")
	}
	if !errors.Is(err, wallet.ErrNotFound) {
		t.Errorf("Apply: got %v, want wallet.ErrNotFound", err)
	}
	if grants.consume.id != uuid.Nil {
		t.Error("Consume must not be called when downstream write fails")
	}
}

func TestApply_ExtraTokens_InvalidAmount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	g, _ := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens, map[string]any{"amount": int64(0)}, "missing amount integration test", now)
	grants.seed(g)

	svc := newApplyService(t, grants, newFakeRepo(), subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if applied {
		t.Error("applied=true on invalid amount")
	}
	if !errors.Is(err, usecase.ErrInvalidGrantPayload) {
		t.Errorf("Apply: got %v, want ErrInvalidGrantPayload", err)
	}
}

// ---------- idempotency ----------------------------------------------

func TestApply_AlreadyConsumedIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	subs.seedActive(tenant, uuid.New(), now.Add(-time.Hour), now.Add(24*time.Hour), now)
	wrepo := newFakeRepo()
	wrepo.seed(tenant, 0, now)

	g := newExtraTokensGrant(t, tenant, actor, 1000, now)
	if err := g.Consume("prior-ref", now); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	grants.seed(g)

	svc := newApplyService(t, grants, wrepo, subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("Apply on consumed: %v", err)
	}
	if applied {
		t.Error("applied=true on already-consumed grant")
	}
	if wrepo.ledgerCount() != 0 {
		t.Errorf("ledger rows on consumed grant = %d, want 0", wrepo.ledgerCount())
	}
	if len(subs.saved) != 0 {
		t.Errorf("subscriptions saved on consumed grant = %d, want 0", len(subs.saved))
	}
}

func TestApply_RevokedGrantIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	wrepo := newFakeRepo()

	g := newExtraTokensGrant(t, tenant, actor, 1000, now)
	if err := g.Revoke(actor, "test reason for revoke", now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	grants.seed(g)

	svc := newApplyService(t, grants, wrepo, subs, now)
	applied, err := svc.Apply(ctx, g.ID())
	if err != nil {
		t.Fatalf("Apply on revoked: %v", err)
	}
	if applied {
		t.Error("applied=true on revoked grant")
	}
}

func TestApply_MissingGrantReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)

	svc := newApplyService(t, newFakeApplyGrantRepo(), newFakeRepo(), newFakeSubscriptionRepo(), now)
	applied, err := svc.Apply(ctx, uuid.New())
	if applied {
		t.Error("applied=true on missing grant")
	}
	if !errors.Is(err, wallet.ErrNotFound) {
		t.Errorf("Apply: got %v, want wallet.ErrNotFound", err)
	}
}

func TestApply_ZeroGrantIDReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)

	svc := newApplyService(t, newFakeApplyGrantRepo(), newFakeRepo(), newFakeSubscriptionRepo(), now)
	applied, err := svc.Apply(ctx, uuid.Nil)
	if applied {
		t.Error("applied=true on zero grant id")
	}
	if !errors.Is(err, wallet.ErrNotFound) {
		t.Errorf("Apply: got %v, want wallet.ErrNotFound", err)
	}
}

// ---------- second-pass retry safety on extra_tokens path ------------

// If Consume fails after the downstream write, a retry MUST collapse
// the ledger insert via idempotency rather than double-crediting the
// wallet.
func TestApply_ExtraTokens_ConsumeFailureRetryIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 21, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	actor := uuid.New()

	grants := newFakeApplyGrantRepo()
	subs := newFakeSubscriptionRepo()
	wrepo := newFakeRepo()
	wrepo.seed(tenant, 0, now)

	g := newExtraTokensGrant(t, tenant, actor, 500_000, now)
	grants.seed(g)

	svc := newApplyService(t, grants, wrepo, subs, now)

	// First pass: Consume fails. The downstream ledger row IS written.
	grants.consume.err = errors.New("transient consume failure")
	if _, err := svc.Apply(ctx, g.ID()); err == nil {
		t.Fatal("expected error from failed Consume")
	}
	if wrepo.ledgerCount() != 1 {
		t.Fatalf("first pass ledger rows = %d, want 1", wrepo.ledgerCount())
	}

	// Second pass: Consume works. The retry MUST NOT double-credit.
	grants.consume.err = nil
	_, err := svc.Apply(ctx, g.ID())
	if err == nil {
		t.Fatal("expected ErrIdempotencyConflict on retry")
	}
	if !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Errorf("retry err = %v, want ErrIdempotencyConflict", err)
	}
	if wrepo.ledgerCount() != 1 {
		t.Errorf("ledger rows after retry = %d, want 1 (idempotency collapse)", wrepo.ledgerCount())
	}
	wid := wrepo.byTenant[tenant]
	bal, _, _ := wrepo.snapshotBalance(wid)
	if bal != 500_000 {
		t.Errorf("balance after retry = %d, want 500_000 (no double-credit)", bal)
	}
}
