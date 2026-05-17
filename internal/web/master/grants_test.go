package master_test

// SIN-62884 / Fase 2.5 C10 — tests for the cap policy helpers and
// the wallet-backed GrantPort adapter (web/master.NewWalletGrantPort).
// The adapter integrates with wallet.MasterGrantRepository via an
// in-process fake — the postgres adapter exercises the persistence
// path in internal/adapter/db/postgres/wallet_master_grant_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/web/master"
)

// ----- Cap policy helpers ----------------------------------------------

func TestCapEquivalence_ExtraTokensUsesAmountDirectly(t *testing.T) {
	if got := master.CapEquivalence(master.GrantKindExtraTokens, 12345, 0); got != 12345 {
		t.Errorf("CapEquivalence(extra_tokens, 12345) = %d, want 12345", got)
	}
}

func TestCapEquivalence_FreePeriodUsesPerDayEquivalence(t *testing.T) {
	// 30 days × 35_000 tokens/day = 1_050_000 tokens-equivalent.
	if got := master.CapEquivalence(master.GrantKindFreeSubscriptionPeriod, 0, 30); got != 30*master.FreeSubscriptionDayEquivalence {
		t.Errorf("CapEquivalence(free_subscription_period, 30) = %d, want %d", got, 30*master.FreeSubscriptionDayEquivalence)
	}
}

func TestCapEquivalence_UnknownKindReturnsZero(t *testing.T) {
	if got := master.CapEquivalence(master.GrantKind("bogus"), 1000, 30); got != 0 {
		t.Errorf("CapEquivalence(bogus) = %d, want 0", got)
	}
}

func TestEnforceCap_BelowPerGrant(t *testing.T) {
	if err := master.EnforceCap(1_000_000, 0); err != nil {
		t.Errorf("EnforceCap(1M, 0) = %v, want nil", err)
	}
}

func TestEnforceCap_AtPerGrant(t *testing.T) {
	if err := master.EnforceCap(master.PerGrantCap, 0); err != nil {
		t.Errorf("EnforceCap(PerGrantCap, 0) = %v, want nil (cap is inclusive upper bound)", err)
	}
}

func TestEnforceCap_OverPerGrant(t *testing.T) {
	if err := master.EnforceCap(master.PerGrantCap+1, 0); !errors.Is(err, master.ErrPerGrantCapExceeded) {
		t.Errorf("EnforceCap(PerGrantCap+1, 0) = %v, want ErrPerGrantCapExceeded", err)
	}
}

func TestEnforceCap_OverPerTenantWindow(t *testing.T) {
	// cumulative + equivalent > PerTenantWindowCap → ErrPerTenantWindowCapExceeded.
	err := master.EnforceCap(2_000_000, master.PerTenantWindowCap-1_000_000)
	if !errors.Is(err, master.ErrPerTenantWindowCapExceeded) {
		t.Errorf("EnforceCap(2M, cap-1M) = %v, want ErrPerTenantWindowCapExceeded", err)
	}
}

func TestEnforceCap_AtPerTenantWindow(t *testing.T) {
	// cumulative + equivalent == PerTenantWindowCap → allowed.
	err := master.EnforceCap(1_000_000, master.PerTenantWindowCap-1_000_000)
	if err != nil {
		t.Errorf("EnforceCap at window boundary = %v, want nil", err)
	}
}

// ----- Fake wallet.MasterGrantRepository -------------------------------

type fakeWalletRepo struct {
	rows []*wallet.MasterGrant

	createErr error
	listErr   error
	revokeErr error

	createdReplay func(g *wallet.MasterGrant)
}

func (f *fakeWalletRepo) Create(_ context.Context, g *wallet.MasterGrant) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.rows = append([]*wallet.MasterGrant{g}, f.rows...)
	if f.createdReplay != nil {
		f.createdReplay(g)
	}
	return nil
}

func (f *fakeWalletRepo) GetByID(_ context.Context, id uuid.UUID) (*wallet.MasterGrant, error) {
	for _, g := range f.rows {
		if g.ID() == id {
			return g, nil
		}
	}
	return nil, wallet.ErrNotFound
}

func (f *fakeWalletRepo) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]*wallet.MasterGrant, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*wallet.MasterGrant
	for _, g := range f.rows {
		if g.TenantID() == tenantID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeWalletRepo) Revoke(_ context.Context, id, _ uuid.UUID, _ string, _ time.Time) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	for _, g := range f.rows {
		if g.ID() == id {
			if g.IsConsumed() {
				return wallet.ErrGrantAlreadyConsumed
			}
			if g.IsRevoked() {
				return wallet.ErrGrantAlreadyRevoked
			}
			return nil
		}
	}
	return wallet.ErrNotFound
}

var _ wallet.MasterGrantRepository = (*fakeWalletRepo)(nil)

// ----- WalletGrantPort tests --------------------------------------------

func newClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestNewWalletGrantPort_RejectsNilRepo(t *testing.T) {
	if _, err := master.NewWalletGrantPort(nil, time.Now); err == nil {
		t.Fatal("expected error for nil repo")
	}
}

func TestWalletGrantPort_IssueGrant_FreePeriod_HappyPath(t *testing.T) {
	repo := &fakeWalletRepo{}
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	port, err := master.NewWalletGrantPort(repo, newClock(now))
	if err != nil {
		t.Fatalf("NewWalletGrantPort: %v", err)
	}
	tenant := uuid.New()
	actor := uuid.New()
	res, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: actor,
		TenantID:    tenant,
		Kind:        master.GrantKindFreeSubscriptionPeriod,
		PeriodDays:  30,
		Reason:      "razao valida com mais de dez chars",
	})
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if res.Grant.Kind != master.GrantKindFreeSubscriptionPeriod {
		t.Errorf("kind = %s, want free_subscription_period", res.Grant.Kind)
	}
	if res.Grant.PeriodDays != 30 {
		t.Errorf("period_days = %d, want 30", res.Grant.PeriodDays)
	}
	if res.Grant.ExternalID == "" {
		t.Errorf("ExternalID empty — ULID not generated")
	}
	if !res.Grant.IsRevocable() {
		t.Errorf("fresh grant should be revocable")
	}
	if len(repo.rows) != 1 {
		t.Fatalf("repo.rows len = %d, want 1", len(repo.rows))
	}
}

func TestWalletGrantPort_IssueGrant_ExtraTokens_HappyPath(t *testing.T) {
	repo := &fakeWalletRepo{}
	port, err := master.NewWalletGrantPort(repo, nil)
	if err != nil {
		t.Fatalf("NewWalletGrantPort: %v", err)
	}
	res, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      500_000,
		Reason:      "razao valida com mais de dez chars",
	})
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if res.Grant.Amount != 500_000 {
		t.Errorf("amount = %d, want 500_000", res.Grant.Amount)
	}
}

func TestWalletGrantPort_IssueGrant_PerGrantCapBlocksAbove10M(t *testing.T) {
	repo := &fakeWalletRepo{}
	port, _ := master.NewWalletGrantPort(repo, nil)
	_, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      master.PerGrantCap + 1,
		Reason:      "concessao acima do limite legal",
	})
	if !errors.Is(err, master.ErrPerGrantCapExceeded) {
		t.Fatalf("err = %v, want ErrPerGrantCapExceeded", err)
	}
	if len(repo.rows) != 0 {
		t.Errorf("repo.Create called despite cap; rows=%d", len(repo.rows))
	}
}

func TestWalletGrantPort_IssueGrant_CumulativeCapUsesTrailing365d(t *testing.T) {
	repo := &fakeWalletRepo{}
	tenant := uuid.New()
	actor := uuid.New()

	// Seed 10 non-revoked grants inside the window each = 10M, so
	// cumulative = 100M. Any further grant must trip the
	// per-tenant 365d cap.
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		seed, err := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens,
			map[string]any{"amount": int64(10_000_000)}, "seed grant para teste de cap", now.Add(-time.Duration(i+1)*time.Hour))
		if err != nil {
			t.Fatalf("seed NewMasterGrant: %v", err)
		}
		repo.rows = append(repo.rows, seed)
	}

	port, _ := master.NewWalletGrantPort(repo, newClock(now))

	// New 1M grant: under per-grant cap (10M) but cumulative would
	// be 101M > 100M → ErrPerTenantWindowCapExceeded.
	_, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: actor,
		TenantID:    tenant,
		Kind:        master.GrantKindExtraTokens,
		Amount:      1_000_000,
		Reason:      "concessao adicional sobre o cap acumulado",
	})
	if !errors.Is(err, master.ErrPerTenantWindowCapExceeded) {
		t.Fatalf("err = %v, want ErrPerTenantWindowCapExceeded", err)
	}
}

func TestWalletGrantPort_IssueGrant_OldGrantsDoNotCountTowardCap(t *testing.T) {
	repo := &fakeWalletRepo{}
	tenant := uuid.New()
	actor := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	// Seed a grant OUTSIDE the trailing window (now-400d). Should be
	// ignored by the cap calculator (per-grant 10M is the legal max,
	// so any single grant within the cap can sit OUT of the window
	// without tripping the per-grant gate).
	old, _ := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens,
		map[string]any{"amount": int64(10_000_000)}, "concessao antiga fora do window", now.Add(-400*24*time.Hour))
	repo.rows = append(repo.rows, old)

	port, _ := master.NewWalletGrantPort(repo, newClock(now))
	res, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: actor,
		TenantID:    tenant,
		Kind:        master.GrantKindExtraTokens,
		Amount:      5_000_000,
		Reason:      "concessao normal pos cap window",
	})
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if res.Grant.Amount != 5_000_000 {
		t.Errorf("amount = %d, want 5M", res.Grant.Amount)
	}
}

func TestWalletGrantPort_IssueGrant_ListErrorBubblesUp(t *testing.T) {
	repo := &fakeWalletRepo{listErr: errors.New("boom")}
	port, _ := master.NewWalletGrantPort(repo, nil)
	_, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      100,
		Reason:      "razao valida com mais de dez chars",
	})
	if err == nil {
		t.Fatal("expected list error to bubble up")
	}
}

func TestWalletGrantPort_IssueGrant_ReasonTooShortFromDomain(t *testing.T) {
	repo := &fakeWalletRepo{}
	port, _ := master.NewWalletGrantPort(repo, nil)
	_, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      100,
		Reason:      "curto",
	})
	if err == nil {
		t.Fatal("expected reason-too-short error from domain")
	}
}

func TestWalletGrantPort_IssueGrant_RepoCreateErrorBubblesUp(t *testing.T) {
	repo := &fakeWalletRepo{createErr: errors.New("conflict")}
	port, _ := master.NewWalletGrantPort(repo, nil)
	_, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      100,
		Reason:      "razao valida com mais de dez chars",
	})
	if err == nil {
		t.Fatal("expected repo create error to bubble up")
	}
}

func TestWalletGrantPort_RevokeGrant_TranslatesSentinels(t *testing.T) {
	cases := []struct {
		name     string
		walletEr error
		wantErr  error
	}{
		{"not_found", wallet.ErrNotFound, master.ErrGrantNotFound},
		{"already_consumed", wallet.ErrGrantAlreadyConsumed, master.ErrGrantAlreadyConsumed},
		{"already_revoked", wallet.ErrGrantAlreadyRevoked, master.ErrGrantAlreadyRevoked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeWalletRepo{revokeErr: tc.walletEr}
			port, _ := master.NewWalletGrantPort(repo, nil)
			err := port.RevokeGrant(context.Background(), master.RevokeGrantInput{
				ActorUserID: uuid.New(),
				GrantID:     uuid.New(),
				Reason:      "razao valida bem longa",
			})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestWalletGrantPort_RevokeGrant_PassthroughOnUnknownError(t *testing.T) {
	other := errors.New("transient")
	repo := &fakeWalletRepo{revokeErr: other}
	port, _ := master.NewWalletGrantPort(repo, nil)
	err := port.RevokeGrant(context.Background(), master.RevokeGrantInput{
		ActorUserID: uuid.New(),
		GrantID:     uuid.New(),
		Reason:      "razao valida bem longa",
	})
	if !errors.Is(err, other) {
		t.Errorf("err = %v, want transient", err)
	}
}

func TestWalletGrantPort_RevokeGrant_HappyPath(t *testing.T) {
	repo := &fakeWalletRepo{}
	g, _ := wallet.NewMasterGrant(uuid.New(), uuid.New(), wallet.KindExtraTokens,
		map[string]any{"amount": int64(100)}, "razao valida bem longa", time.Now())
	repo.rows = append(repo.rows, g)
	port, _ := master.NewWalletGrantPort(repo, nil)
	if err := port.RevokeGrant(context.Background(), master.RevokeGrantInput{
		ActorUserID: uuid.New(),
		GrantID:     g.ID(),
		Reason:      "outra razao valida bem longa",
	}); err != nil {
		t.Errorf("RevokeGrant: %v", err)
	}
}

func TestWalletGrantPort_ListGrants_HydratesRows(t *testing.T) {
	repo := &fakeWalletRepo{}
	tenant := uuid.New()
	g, _ := wallet.NewMasterGrant(tenant, uuid.New(), wallet.KindExtraTokens,
		map[string]any{"amount": int64(2500)}, "razao valida bem longa", time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	repo.rows = append(repo.rows, g)

	port, _ := master.NewWalletGrantPort(repo, nil)
	rows, err := port.ListGrants(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Amount != 2500 {
		t.Errorf("amount = %d, want 2500", rows[0].Amount)
	}
	if rows[0].Kind != master.GrantKindExtraTokens {
		t.Errorf("kind = %s, want extra_tokens", rows[0].Kind)
	}
	if rows[0].ExternalID == "" {
		t.Errorf("ExternalID empty after hydrate")
	}
}

func TestWalletGrantPort_ListGrants_BubblesError(t *testing.T) {
	repo := &fakeWalletRepo{listErr: errors.New("boom")}
	port, _ := master.NewWalletGrantPort(repo, nil)
	if _, err := port.ListGrants(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected list error")
	}
}

// ----- hydrateGrantRow with consumed/revoked exercised via WalletGrantPort -----

func TestWalletGrantPort_ListGrants_HydratesConsumedAndRevoked(t *testing.T) {
	repo := &fakeWalletRepo{}
	tenant := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	// One consumed grant (Consume sets consumed_at + ref).
	consumed, _ := wallet.NewMasterGrant(tenant, uuid.New(), wallet.KindFreeSubscriptionPeriod,
		map[string]any{"period_days": 30}, "consumed grant para hydrate test", now.Add(-2*time.Hour))
	if err := consumed.Consume("subscription/abc", now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	// One revoked grant.
	revoked, _ := wallet.NewMasterGrant(tenant, uuid.New(), wallet.KindExtraTokens,
		map[string]any{"amount": int64(500)}, "revoked grant para hydrate test", now.Add(-3*time.Hour))
	if err := revoked.Revoke(uuid.New(), "razao de revogacao bem longa", now.Add(-30*time.Minute)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	repo.rows = append(repo.rows, consumed, revoked)

	port, _ := master.NewWalletGrantPort(repo, nil)
	rows, err := port.ListGrants(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	var sawConsumed, sawRevoked bool
	for _, r := range rows {
		if r.Consumed && !r.ConsumedAt.IsZero() {
			sawConsumed = true
		}
		if r.Revoked && !r.RevokedAt.IsZero() && r.RevokeBy != uuid.Nil {
			sawRevoked = true
		}
	}
	if !sawConsumed {
		t.Errorf("did not hydrate consumed grant")
	}
	if !sawRevoked {
		t.Errorf("did not hydrate revoked grant (RevokeBy/RevokedAt missing)")
	}
}

// Revoked grants must not contribute to the cumulative cap even if
// they fall inside the trailing 365d window.
func TestWalletGrantPort_IssueGrant_RevokedGrantsExcludedFromCap(t *testing.T) {
	repo := &fakeWalletRepo{}
	tenant := uuid.New()
	actor := uuid.New()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	// Seed 11 × 10M grants ALL revoked. Cumulative including these
	// would be 110M (over 100M cap); with the revoke filter, the
	// effective cumulative is 0.
	for i := 0; i < 11; i++ {
		g, _ := wallet.NewMasterGrant(tenant, actor, wallet.KindExtraTokens,
			map[string]any{"amount": int64(10_000_000)}, "seed grant para teste de cap", now.Add(-time.Duration(i+1)*time.Hour))
		if err := g.Revoke(actor, "revogada cedo demais para contar no cap", now.Add(-time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("Revoke seed: %v", err)
		}
		repo.rows = append(repo.rows, g)
	}

	port, _ := master.NewWalletGrantPort(repo, newClock(now))
	res, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: actor,
		TenantID:    tenant,
		Kind:        master.GrantKindExtraTokens,
		Amount:      1_000_000,
		Reason:      "concessao apos revogacoes anteriores",
	})
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if res.Grant.Amount != 1_000_000 {
		t.Errorf("amount = %d, want 1M", res.Grant.Amount)
	}
}

// ----- ensureGrantPresent idempotency ----------------------------------

func TestEnsureGrantPresent_NoDuplicateWhenRowAlreadyListed(t *testing.T) {
	existing := master.GrantRow{
		ID:       uuid.MustParse("88888888-8888-8888-8888-888888888888"),
		TenantID: uuid.MustParse(fakeTenantID),
		Kind:     master.GrantKindExtraTokens,
		Amount:   100,
	}
	out := master.ExportEnsureGrantPresent([]master.GrantRow{existing}, master.GrantRow{ID: existing.ID})
	if len(out) != 1 {
		t.Errorf("len = %d, want 1 (already-present branch should be a no-op)", len(out))
	}
}

func TestEnsureGrantPresent_PrependsWhenNotListed(t *testing.T) {
	row := master.GrantRow{
		ID:       uuid.MustParse("99999999-9999-9999-9999-999999999999"),
		TenantID: uuid.MustParse(fakeTenantID),
	}
	out := master.ExportEnsureGrantPresent(nil, row)
	if len(out) != 1 || out[0].ID != row.ID {
		t.Errorf("did not prepend new row: %v", out)
	}
}

// ----- Tiny template helpers ------------------------------------------

func TestInt64ToStr_Cases(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{12345, "12345"},
		{-42, "-42"},
	}
	for _, tc := range cases {
		if got := master.ExportInt64ToStr(tc.in); got != tc.want {
			t.Errorf("int64ToStr(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatGrantTime_ZeroAndNonZero(t *testing.T) {
	if got := master.ExportFormatGrantTime(time.Time{}); got != "—" {
		t.Errorf("zero time = %q, want em-dash", got)
	}
	tt := time.Date(2026, 5, 16, 12, 30, 0, 0, time.UTC)
	if got := master.ExportFormatGrantTime(tt); got != "2026-05-16 12:30 UTC" {
		t.Errorf("formatGrantTime(2026-05-16) = %q", got)
	}
}

func TestGrantKindLabel_Cases(t *testing.T) {
	cases := []struct {
		in   master.GrantKind
		want string
	}{
		{master.GrantKindFreeSubscriptionPeriod, "Período grátis"},
		{master.GrantKindExtraTokens, "Tokens extras"},
		{master.GrantKind("custom"), "custom"},
	}
	for _, tc := range cases {
		if got := master.ExportGrantKindLabel(tc.in); got != tc.want {
			t.Errorf("grantKindLabel(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadInt64Payload_Cases(t *testing.T) {
	if v := master.ExportReadInt64Payload(nil, "amount"); v != 0 {
		t.Errorf("nil payload = %d", v)
	}
	if v := master.ExportReadInt64Payload(map[string]any{"x": int64(5)}, "amount"); v != 0 {
		t.Errorf("missing key = %d", v)
	}
	for _, tc := range []struct {
		name string
		p    map[string]any
		want int64
	}{
		{"int", map[string]any{"amount": int(42)}, 42},
		{"int32", map[string]any{"amount": int32(43)}, 43},
		{"int64", map[string]any{"amount": int64(44)}, 44},
		{"float32", map[string]any{"amount": float32(45)}, 45},
		{"float64", map[string]any{"amount": float64(46)}, 46},
		{"string_unsupported", map[string]any{"amount": "47"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := master.ExportReadInt64Payload(tc.p, "amount"); got != tc.want {
				t.Errorf("got %d want %d", got, tc.want)
			}
		})
	}
}

// ----- GrantRow.IsRevocable --------------------------------------------

func TestGrantRow_IsRevocable(t *testing.T) {
	cases := []struct {
		name string
		row  master.GrantRow
		want bool
	}{
		{"fresh", master.GrantRow{}, true},
		{"consumed", master.GrantRow{Consumed: true}, false},
		{"revoked", master.GrantRow{Revoked: true}, false},
		{"both", master.GrantRow{Consumed: true, Revoked: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.IsRevocable(); got != tc.want {
				t.Errorf("IsRevocable() = %v, want %v", got, tc.want)
			}
		})
	}
}
