package management_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// fakeStore is the in-memory Store the use-case tests run against. It
// matches the SQL adapter's contract: rows are keyed by id and indexed
// by tenant; soft-deleted rows are filtered out of List() but still
// returned by GetByID() (deletion preserves the row for audit/RLS).
type fakeStore struct {
	mu                 sync.Mutex
	rows               map[uuid.UUID]management.Domain
	beforeMarkVerified func(s *fakeStore) // SIN-63104: lets a test rotate the token between Get and the CAS write
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[uuid.UUID]management.Domain{}}
}

func (s *fakeStore) List(_ context.Context, tenantID uuid.UUID) ([]management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]management.Domain, 0)
	for _, d := range s.rows {
		if d.TenantID != tenantID {
			continue
		}
		if d.DeletedAt != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *fakeStore) GetByID(_ context.Context, id uuid.UUID) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok {
		return management.Domain{}, management.ErrStoreNotFound
	}
	return d, nil
}

func (s *fakeStore) Insert(_ context.Context, d management.Domain) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.rows[d.ID]; exists {
		return management.Domain{}, errors.New("duplicate id")
	}
	s.rows[d.ID] = d
	return d, nil
}

func (s *fakeStore) MarkVerified(_ context.Context, id uuid.UUID, expectedToken string, at time.Time, withDNSSEC bool, logID *uuid.UUID) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beforeMarkVerified != nil {
		s.beforeMarkVerified(s)
	}
	d, ok := s.rows[id]
	if !ok {
		return management.Domain{}, management.ErrStoreNotFound
	}
	if d.VerificationToken != expectedToken {
		return management.Domain{}, management.ErrTokenRotated
	}
	t := at
	d.VerifiedAt = &t
	d.VerifiedWithDNSSEC = withDNSSEC
	d.DNSResolutionLogID = logID
	d.UpdatedAt = at
	s.rows[id] = d
	return d, nil
}

func (s *fakeStore) SetPaused(_ context.Context, id uuid.UUID, pausedAt *time.Time) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok {
		return management.Domain{}, management.ErrStoreNotFound
	}
	d.TLSPausedAt = pausedAt
	if pausedAt != nil {
		d.UpdatedAt = *pausedAt
	}
	s.rows[id] = d
	return d, nil
}

func (s *fakeStore) SoftDelete(_ context.Context, id uuid.UUID, at time.Time) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok {
		return management.Domain{}, management.ErrStoreNotFound
	}
	t := at
	d.DeletedAt = &t
	d.UpdatedAt = at
	s.rows[id] = d
	return d, nil
}

func (s *fakeStore) RotateToken(_ context.Context, id uuid.UUID, newToken string, issuedAt time.Time) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.DeletedAt != nil {
		return management.Domain{}, management.ErrStoreNotFound
	}
	if d.VerifiedAt != nil {
		return management.Domain{}, management.ErrAlreadyVerified
	}
	d.VerificationToken = newToken
	d.TokenIssuedAt = issuedAt
	d.UpdatedAt = issuedAt
	s.rows[id] = d
	return d, nil
}

// fakeGate stubs the enrollment quota gate. Allow returns the next
// queued decision; tests with multiple Enroll calls queue a slice.
type fakeGate struct {
	decisions []management.EnrollmentDecision
	calls     int
}

func (g *fakeGate) Allow(_ context.Context, _ uuid.UUID) management.EnrollmentDecision {
	if g.calls >= len(g.decisions) {
		return management.EnrollmentDecision{Allowed: true}
	}
	d := g.decisions[g.calls]
	g.calls++
	return d
}

// fakeValidator returns a queued error per Validate call.
type fakeValidator struct {
	err error
}

func (v *fakeValidator) Validate(_ context.Context, _ string) error { return v.err }

// fakeDNS stubs DNSChecker. calls counts invocations so SIN-63104
// regression tests can assert the TTL gate short-circuits before DNS.
type fakeDNS struct {
	result management.DNSCheckResult
	err    error
	calls  int
}

func (f *fakeDNS) Check(_ context.Context, _, _ string) (management.DNSCheckResult, error) {
	f.calls++
	return f.result, f.err
}

// fakeSlug captures release calls.
type fakeSlug struct {
	released []string
	err      error
}

func (s *fakeSlug) ReleaseSlug(_ context.Context, slug string, _ uuid.UUID) error {
	s.released = append(s.released, slug)
	return s.err
}

// fakeAudit collects events for assertion.
type fakeAudit struct {
	events []management.AuditEvent
}

func (a *fakeAudit) LogManagement(_ context.Context, ev management.AuditEvent) {
	a.events = append(a.events, ev)
}

func fixedNow(t time.Time) management.Clock { return func() time.Time { return t } }

func detTokenGen(token string) management.TokenGenerator {
	return func() (string, error) { return token, nil }
}

func mustNew(t *testing.T, cfg management.Config) *management.UseCase {
	t.Helper()
	uc, err := management.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return uc
}

func TestNew_RequiresStoreAndGate(t *testing.T) {
	t.Parallel()
	if _, err := management.New(management.Config{}); err == nil {
		t.Fatal("expected error when Store is missing")
	}
	if _, err := management.New(management.Config{Store: newFakeStore()}); err == nil {
		t.Fatal("expected error when Gate is missing")
	}
}

func TestNormalizeHost_Cases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"valid lowercase", "shop.example.com", "shop.example.com", true},
		{"trims trailing dot", "shop.example.com.", "shop.example.com", true},
		{"uppercases collapse", "Shop.Example.COM", "shop.example.com", true},
		{"empty rejected", "", "", false},
		{"too long rejected", string(make([]byte, 254)), "", false},
		{"missing dot rejected", "localhost", "", false},
		{"ip literal rejected", "127.0.0.1", "", false},
		{"label leading hyphen", "-bad.example.com", "", false},
		{"label trailing hyphen", "bad-.example.com", "", false},
		{"empty label", "shop..example.com", "", false},
		{"label too long", "a-very-very-very-very-very-very-very-very-very-very-very-long-label.example.com", "", false},
		{"underscore rejected", "shop_test.example.com", "", false},
		{"unicode rejected", "shöp.example.com", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := management.NormalizeHost(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("NormalizeHost(%q) err: %v", tc.in, err)
				}
				if got != tc.want {
					t.Fatalf("NormalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("NormalizeHost(%q) expected error, got %q", tc.in, got)
			}
			if !errors.Is(err, management.ErrInvalidHost) {
				t.Fatalf("NormalizeHost(%q) err is not ErrInvalidHost: %v", tc.in, err)
			}
		})
	}
}

func TestEnroll_AllowedHappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	gate := &fakeGate{}
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{
		Store:    store,
		Gate:     gate,
		Audit:    audit,
		TokenGen: detTokenGen("abc123"),
		Now:      fixedNow(now),
	})
	tenant := uuid.New()
	res, err := uc.Enroll(context.Background(), tenant, "Shop.Example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.Reason != management.ReasonNone {
		t.Fatalf("reason = %v", res.Reason)
	}
	if res.Domain.Host != "shop.example.com" {
		t.Fatalf("host = %q", res.Domain.Host)
	}
	if res.Domain.VerificationToken != "abc123" {
		t.Fatalf("token = %q", res.Domain.VerificationToken)
	}
	if res.TXTRecord != "_crm-verify.shop.example.com" {
		t.Fatalf("TXT record = %q", res.TXTRecord)
	}
	if res.TXTValue != "crm-verify=abc123" {
		t.Fatalf("TXT value = %q", res.TXTValue)
	}
	if !res.Domain.CreatedAt.Equal(now) {
		t.Fatalf("createdAt = %v, want %v", res.Domain.CreatedAt, now)
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != "ok" {
		t.Fatalf("audit events = %+v", audit.events)
	}
}

func TestEnroll_InvalidHost(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	gate := &fakeGate{}
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{Store: store, Gate: gate, Audit: audit, Now: fixedNow(time.Now())})
	res, err := uc.Enroll(context.Background(), uuid.New(), "127.0.0.1")
	if !errors.Is(err, management.ErrInvalidHost) {
		t.Fatalf("err = %v, want ErrInvalidHost", err)
	}
	if res.Reason != management.ReasonInvalidHost {
		t.Fatalf("reason = %v", res.Reason)
	}
	if len(audit.events) != 1 || audit.events[0].Reason != management.ReasonInvalidHost {
		t.Fatalf("audit events = %+v", audit.events)
	}
}

func TestEnroll_RateLimitedReturnsRetry(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	gate := &fakeGate{decisions: []management.EnrollmentDecision{
		{Allowed: false, Reason: management.ReasonRateLimited, RetryAfter: 17 * time.Minute},
	}}
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{Store: store, Gate: gate, Audit: audit, Now: fixedNow(time.Now())})
	res, err := uc.Enroll(context.Background(), uuid.New(), "shop.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.Reason != management.ReasonRateLimited {
		t.Fatalf("reason = %v", res.Reason)
	}
	if res.RetryAfter != 17*time.Minute {
		t.Fatalf("retryAfter = %v", res.RetryAfter)
	}
}

func TestEnroll_GateError(t *testing.T) {
	t.Parallel()
	want := errors.New("redis down")
	store := newFakeStore()
	gate := &fakeGate{decisions: []management.EnrollmentDecision{{Err: want}}}
	uc := mustNew(t, management.Config{Store: store, Gate: gate, Now: fixedNow(time.Now())})
	_, err := uc.Enroll(context.Background(), uuid.New(), "shop.example.com")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestEnroll_TokenGenError(t *testing.T) {
	t.Parallel()
	want := errors.New("rand failure")
	store := newFakeStore()
	gate := &fakeGate{}
	uc := mustNew(t, management.Config{
		Store:    store,
		Gate:     gate,
		TokenGen: func() (string, error) { return "", want },
		Now:      fixedNow(time.Now()),
	})
	res, err := uc.Enroll(context.Background(), uuid.New(), "shop.example.com")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if res.Reason != management.ReasonInternal {
		t.Fatalf("reason = %v", res.Reason)
	}
}

func TestEnroll_PrivateIPViaValidator(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	gate := &fakeGate{}
	uc := mustNew(t, management.Config{
		Store:     store,
		Gate:      gate,
		Validator: &fakeValidator{err: management.ErrPrivateIP},
		Now:       fixedNow(time.Now()),
	})
	res, err := uc.Enroll(context.Background(), uuid.New(), "private.example.com")
	if !errors.Is(err, management.ErrPrivateIP) {
		t.Fatalf("err = %v, want ErrPrivateIP", err)
	}
	if res.Reason != management.ReasonPrivateIP {
		t.Fatalf("reason = %v", res.Reason)
	}
}

func TestEnroll_TenantNilRejected(t *testing.T) {
	t.Parallel()
	uc := mustNew(t, management.Config{Store: newFakeStore(), Gate: &fakeGate{}})
	_, err := uc.Enroll(context.Background(), uuid.Nil, "shop.example.com")
	if !errors.Is(err, management.ErrTenantMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_HappyPath(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	now := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	store.rows[domainID] = management.Domain{
		ID: domainID, TenantID: tenant, Host: "shop.example.com",
		VerificationToken: "tok", TokenIssuedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	logID := uuid.New()
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		DNS: &fakeDNS{result: management.DNSCheckResult{WithDNSSEC: true, LogID: &logID}},
		Now: fixedNow(now),
	})
	out, err := uc.Verify(context.Background(), tenant, domainID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !out.Verified {
		t.Fatal("expected verified=true")
	}
	if out.Domain.VerifiedAt == nil || !out.Domain.VerifiedAt.Equal(now) {
		t.Fatalf("verifiedAt = %v", out.Domain.VerifiedAt)
	}
	if !out.Domain.VerifiedWithDNSSEC {
		t.Fatal("expected DNSSEC flag set")
	}
	if out.Domain.DNSResolutionLogID == nil || *out.Domain.DNSResolutionLogID != logID {
		t.Fatalf("logID = %v", out.Domain.DNSResolutionLogID)
	}
}

func TestVerify_TokenMismatch(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	now := time.Now()
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com", VerificationToken: "tok", TokenIssuedAt: now}
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		DNS: &fakeDNS{err: management.ErrTokenMismatch},
		Now: fixedNow(now),
	})
	out, err := uc.Verify(context.Background(), tenant, domainID)
	if !errors.Is(err, management.ErrTokenMismatch) {
		t.Fatalf("err = %v", err)
	}
	if out.Reason != management.ReasonTokenMismatch {
		t.Fatalf("reason = %v", out.Reason)
	}
}

func TestVerify_AlreadyVerifiedShortCircuits(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	verified := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com", VerifiedAt: &verified}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, DNS: &fakeDNS{err: errors.New("should not be called")}, Now: fixedNow(time.Now())})
	out, err := uc.Verify(context.Background(), tenant, domainID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !out.Verified {
		t.Fatal("expected verified=true (idempotent)")
	}
	if out.Reason != management.ReasonAlreadyVerified {
		t.Fatalf("reason = %v", out.Reason)
	}
}

func TestVerify_NotFound(t *testing.T) {
	t.Parallel()
	uc := mustNew(t, management.Config{Store: newFakeStore(), Gate: &fakeGate{}, DNS: &fakeDNS{}, Now: fixedNow(time.Now())})
	_, err := uc.Verify(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_TenantMismatch(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	owner := uuid.New()
	other := uuid.New()
	domainID := uuid.New()
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: owner, Host: "shop.example.com", VerificationToken: "tok"}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, DNS: &fakeDNS{}, Now: fixedNow(time.Now())})
	_, err := uc.Verify(context.Background(), other, domainID)
	if !errors.Is(err, management.ErrTenantMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify_DNSCheckerMissing(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	now := time.Now()
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com", VerificationToken: "tok", TokenIssuedAt: now}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, Now: fixedNow(now)})
	_, err := uc.Verify(context.Background(), tenant, domainID)
	if err == nil {
		t.Fatal("expected error when DNSChecker is nil")
	}
}

func TestSetPaused_TogglesAndAudits(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com"}
	now := time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC)
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, Audit: audit, Now: fixedNow(now)})
	d, err := uc.SetPaused(context.Background(), tenant, domainID, true)
	if err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	if d.TLSPausedAt == nil || !d.TLSPausedAt.Equal(now) {
		t.Fatalf("pausedAt = %v", d.TLSPausedAt)
	}
	d, err = uc.SetPaused(context.Background(), tenant, domainID, false)
	if err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	if d.TLSPausedAt != nil {
		t.Fatalf("expected pausedAt cleared, got %v", d.TLSPausedAt)
	}
	if len(audit.events) != 2 {
		t.Fatalf("audit events = %d", len(audit.events))
	}
	if audit.events[0].Action != "pause" || audit.events[1].Action != "resume" {
		t.Fatalf("actions = %s,%s", audit.events[0].Action, audit.events[1].Action)
	}
}

func TestDelete_SoftDeleteAndSlugRelease(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com"}
	slug := &fakeSlug{}
	now := time.Date(2026, 5, 6, 16, 0, 0, 0, time.UTC)
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, Slug: slug, Audit: audit, Now: fixedNow(now)})
	if err := uc.Delete(context.Background(), tenant, domainID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if d := store.rows[domainID]; d.DeletedAt == nil || !d.DeletedAt.Equal(now) {
		t.Fatalf("deletedAt = %v", d.DeletedAt)
	}
	if len(slug.released) != 1 || slug.released[0] != "shop.example.com" {
		t.Fatalf("released = %+v", slug.released)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "delete" {
		t.Fatalf("audit events = %+v", audit.events)
	}
}

func TestDelete_TenantMismatch(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	owner := uuid.New()
	other := uuid.New()
	domainID := uuid.New()
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: owner, Host: "shop.example.com"}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, Slug: &fakeSlug{}, Now: fixedNow(time.Now())})
	if err := uc.Delete(context.Background(), other, domainID); !errors.Is(err, management.ErrTenantMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestList_FiltersByTenantAndExcludesDeleted(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	tenant := uuid.New()
	other := uuid.New()
	now := time.Now()
	store.rows[uuid.New()] = management.Domain{ID: uuid.New(), TenantID: tenant, Host: "a.example.com"}
	store.rows[uuid.New()] = management.Domain{ID: uuid.New(), TenantID: other, Host: "b.example.com"}
	deletedID := uuid.New()
	store.rows[deletedID] = management.Domain{ID: deletedID, TenantID: tenant, Host: "c.example.com", DeletedAt: &now}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}})
	out, err := uc.List(context.Background(), tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 || out[0].Host != "a.example.com" {
		t.Fatalf("out = %+v", out)
	}
}

func TestList_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	uc := mustNew(t, management.Config{Store: newFakeStore(), Gate: &fakeGate{}})
	if _, err := uc.List(context.Background(), uuid.Nil); !errors.Is(err, management.ErrTenantMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusOf_Permutations(t *testing.T) {
	t.Parallel()
	verified := time.Now()
	paused := time.Now()
	if got := management.StatusOf(management.Domain{}, nil); got != management.StatusPending {
		t.Errorf("empty = %v", got)
	}
	if got := management.StatusOf(management.Domain{}, errors.New("dns")); got != management.StatusError {
		t.Errorf("error = %v", got)
	}
	if got := management.StatusOf(management.Domain{VerifiedAt: &verified}, nil); got != management.StatusVerified {
		t.Errorf("verified = %v", got)
	}
	if got := management.StatusOf(management.Domain{VerifiedAt: &verified, TLSPausedAt: &paused}, nil); got != management.StatusPaused {
		t.Errorf("paused = %v", got)
	}
}

func TestCopyPTBR_SpecStrings(t *testing.T) {
	t.Parallel()
	if got := management.CopyPTBR(management.ReasonPrivateIP, 0, nil); got != "Domínio aponta para IP privado. Use um IP público." {
		t.Errorf("private ip copy = %q", got)
	}
	if got := management.CopyPTBR(management.ReasonTokenMismatch, 0, nil); got != "Registro TXT não encontrado ou valor incorreto. Verifique propagação DNS." {
		t.Errorf("token mismatch copy = %q", got)
	}
	if got := management.CopyPTBR(management.ReasonRateLimited, 17*time.Minute, nil); got != "Limite de domínios cadastrados por hora atingido. Tente novamente em 17 minutos." {
		t.Errorf("rate limited copy = %q", got)
	}
	reservedUntil := time.Date(2027, 5, 6, 0, 0, 0, 0, time.UTC)
	if got := management.CopyPTBR(management.ReasonSlugReserved, 0, &reservedUntil); got != "Este slug está reservado até 06/05/2027. Escolha outro." {
		t.Errorf("slug reserved copy = %q", got)
	}
	// retry-after under one minute clamps to 1
	if got := management.CopyPTBR(management.ReasonRateLimited, 30*time.Second, nil); got != "Limite de domínios cadastrados por hora atingido. Tente novamente em 1 minutos." {
		t.Errorf("rate limited <1m clamp = %q", got)
	}
	if got := management.CopyPTBR(management.ReasonSlugReserved, 0, nil); got != "Este slug está reservado até —. Escolha outro." {
		t.Errorf("slug reserved no date = %q", got)
	}
	if got := management.CopyPTBR(management.ReasonNotFound, 0, nil); got == "" {
		t.Errorf("not found copy empty")
	}
	if got := management.CopyPTBR(management.ReasonNone, 0, nil); got != "" {
		t.Errorf("none copy = %q, want empty", got)
	}
}

func TestStatusBadgeColor(t *testing.T) {
	t.Parallel()
	cases := map[management.Status]string{
		management.StatusPending:  "yellow",
		management.StatusVerified: "green",
		management.StatusPaused:   "gray",
		management.StatusError:    "red",
		management.StatusUnknown:  "gray",
	}
	for s, want := range cases {
		if got := management.StatusBadgeColor(s); got != want {
			t.Errorf("StatusBadgeColor(%v) = %q, want %q", s, got, want)
		}
	}
}

func TestStatusLabelPTBR(t *testing.T) {
	t.Parallel()
	for s, want := range map[management.Status]string{
		management.StatusPending:  "Pendente",
		management.StatusVerified: "Verificado",
		management.StatusPaused:   "Pausado",
		management.StatusError:    "Erro",
		management.StatusUnknown:  "Desconhecido",
	} {
		if got := management.StatusLabelPTBR(s); got != want {
			t.Errorf("StatusLabelPTBR(%v) = %q, want %q", s, got, want)
		}
	}
}

func TestReasonString_AllCases(t *testing.T) {
	t.Parallel()
	cases := map[management.Reason]string{
		management.ReasonNone:                "none",
		management.ReasonInvalidHost:         "invalid_host",
		management.ReasonPrivateIP:           "private_ip",
		management.ReasonTokenMismatch:       "token_mismatch",
		management.ReasonDNSResolutionFailed: "dns_resolution_failed",
		management.ReasonRateLimited:         "rate_limited",
		management.ReasonSlugReserved:        "slug_reserved",
		management.ReasonNotFound:            "not_found",
		management.ReasonAlreadyVerified:     "already_verified",
		management.ReasonForbidden:           "forbidden",
		management.ReasonInternal:            "internal",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("%v = %q, want %q", int(r), got, want)
		}
	}
}

func TestStatusString_AllCases(t *testing.T) {
	t.Parallel()
	cases := map[management.Status]string{
		management.StatusUnknown:  "unknown",
		management.StatusPending:  "pending",
		management.StatusVerified: "verified",
		management.StatusPaused:   "paused",
		management.StatusError:    "error",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("%v = %q", int(s), got)
		}
	}
}

func TestTXTHelpers(t *testing.T) {
	t.Parallel()
	if got := management.TXTRecordFor("shop.example.com"); got != "_crm-verify.shop.example.com" {
		t.Errorf("record = %q", got)
	}
	if got := management.TXTValueFor("abc"); got != "crm-verify=abc" {
		t.Errorf("value = %q", got)
	}
}

// errStore lets a single Store method fail on demand, surfacing errors
// that happy-path fakes hide.
type errStore struct {
	*fakeStore
	failInsert       error
	failGet          error
	failMarkVerified error
	failSetPaused    error
	failSoftDelete   error
}

func (s *errStore) Insert(ctx context.Context, d management.Domain) (management.Domain, error) {
	if s.failInsert != nil {
		return management.Domain{}, s.failInsert
	}
	return s.fakeStore.Insert(ctx, d)
}

func (s *errStore) GetByID(ctx context.Context, id uuid.UUID) (management.Domain, error) {
	if s.failGet != nil {
		return management.Domain{}, s.failGet
	}
	return s.fakeStore.GetByID(ctx, id)
}

func (s *errStore) MarkVerified(ctx context.Context, id uuid.UUID, expectedToken string, at time.Time, withDNSSEC bool, logID *uuid.UUID) (management.Domain, error) {
	if s.failMarkVerified != nil {
		return management.Domain{}, s.failMarkVerified
	}
	return s.fakeStore.MarkVerified(ctx, id, expectedToken, at, withDNSSEC, logID)
}

func (s *errStore) SetPaused(ctx context.Context, id uuid.UUID, p *time.Time) (management.Domain, error) {
	if s.failSetPaused != nil {
		return management.Domain{}, s.failSetPaused
	}
	return s.fakeStore.SetPaused(ctx, id, p)
}

func (s *errStore) SoftDelete(ctx context.Context, id uuid.UUID, at time.Time) (management.Domain, error) {
	if s.failSoftDelete != nil {
		return management.Domain{}, s.failSoftDelete
	}
	return s.fakeStore.SoftDelete(ctx, id, at)
}

func TestEnroll_StoreInsertError(t *testing.T) {
	t.Parallel()
	store := &errStore{fakeStore: newFakeStore(), failInsert: errors.New("pg down")}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, TokenGen: detTokenGen("tok"), Now: fixedNow(time.Now())})
	res, err := uc.Enroll(context.Background(), uuid.New(), "shop.example.com")
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Reason != management.ReasonInternal {
		t.Fatalf("reason = %v", res.Reason)
	}
}

func TestVerify_GetError(t *testing.T) {
	t.Parallel()
	store := &errStore{fakeStore: newFakeStore(), failGet: errors.New("pg down")}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, DNS: &fakeDNS{}, Now: fixedNow(time.Now())})
	_, err := uc.Verify(context.Background(), uuid.New(), uuid.New())
	if err == nil {
		t.Fatal("expected error from Get path")
	}
}

func TestVerify_MarkVerifiedError(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	domainID := uuid.New()
	base := newFakeStore()
	now := time.Now()
	base.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com", VerificationToken: "tok", TokenIssuedAt: now}
	store := &errStore{fakeStore: base, failMarkVerified: errors.New("pg down")}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, DNS: &fakeDNS{}, Now: fixedNow(now)})
	_, err := uc.Verify(context.Background(), tenant, domainID)
	if err == nil {
		t.Fatal("expected error from MarkVerified")
	}
}

func TestSetPaused_StoreError(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	domainID := uuid.New()
	base := newFakeStore()
	base.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com"}
	store := &errStore{fakeStore: base, failSetPaused: errors.New("pg down")}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, Now: fixedNow(time.Now())})
	_, err := uc.SetPaused(context.Background(), tenant, domainID, true)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSetPaused_NotFound(t *testing.T) {
	t.Parallel()
	uc := mustNew(t, management.Config{Store: newFakeStore(), Gate: &fakeGate{}, Now: fixedNow(time.Now())})
	_, err := uc.SetPaused(context.Background(), uuid.New(), uuid.New(), true)
	if !errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestDelete_NotFound(t *testing.T) {
	t.Parallel()
	uc := mustNew(t, management.Config{Store: newFakeStore(), Gate: &fakeGate{}, Slug: &fakeSlug{}, Now: fixedNow(time.Now())})
	if err := uc.Delete(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestDelete_SoftDeleteError(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	domainID := uuid.New()
	base := newFakeStore()
	base.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com"}
	store := &errStore{fakeStore: base, failSoftDelete: errors.New("pg down")}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, Slug: &fakeSlug{}, Now: fixedNow(time.Now())})
	if err := uc.Delete(context.Background(), tenant, domainID); err == nil {
		t.Fatal("expected error")
	}
}

func TestDelete_SlugReleaseError(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	domainID := uuid.New()
	store := newFakeStore()
	store.rows[domainID] = management.Domain{ID: domainID, TenantID: tenant, Host: "shop.example.com"}
	slug := &fakeSlug{err: errors.New("dup")}
	uc := mustNew(t, management.Config{Store: store, Gate: &fakeGate{}, Slug: slug, Now: fixedNow(time.Now())})
	if err := uc.Delete(context.Background(), tenant, domainID); err == nil {
		t.Fatal("expected error from slug release")
	}
}

// TestEnroll_DefaultTokenGen exercises the default crypto/rand-backed
// token generator when no override is supplied.
func TestEnroll_DefaultTokenGen(t *testing.T) {
	t.Parallel()
	uc := mustNew(t, management.Config{Store: newFakeStore(), Gate: &fakeGate{}, Now: fixedNow(time.Now())})
	res, err := uc.Enroll(context.Background(), uuid.New(), "shop.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if len(res.Domain.VerificationToken) != 32 {
		t.Fatalf("token len = %d, want 32", len(res.Domain.VerificationToken))
	}
}

// --- SIN-63104 regression tests ---------------------------------------------
//
// These tests fail against pre-remediation code (no TTL gate, no CAS in
// MarkVerified, no TokenFingerprint on AuditEvent) and pass against the
// new code. See plan in /SIN/issues/SIN-63102#document-plan §7.

// TestVerify_RejectsExpiredToken asserts the TTL gate short-circuits
// Verify before DNS when the token has aged past TokenTTL. The DNS
// lookup must not happen so an attacker who has briefly captured the
// tenant's DNS zone after a months-old enrollment cannot replay.
func TestVerify_RejectsExpiredToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	store.rows[domainID] = management.Domain{
		ID: domainID, TenantID: tenant, Host: "shop.example.com",
		VerificationToken: "tok",
		// Issued two hours ago — outside the 1h TTL configured below.
		TokenIssuedAt: now.Add(-2 * time.Hour),
	}
	dns := &fakeDNS{}
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{}, DNS: dns, Audit: audit,
		Now:      fixedNow(now),
		TokenTTL: time.Hour,
	})
	out, err := uc.Verify(context.Background(), tenant, domainID)
	if !errors.Is(err, management.ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
	if out.Reason != management.ReasonTokenExpired {
		t.Fatalf("reason = %v, want ReasonTokenExpired", out.Reason)
	}
	if dns.calls != 0 {
		t.Fatalf("dns.calls = %d, want 0 (TTL must short-circuit DNS)", dns.calls)
	}
	if d := store.rows[domainID]; d.VerifiedAt != nil {
		t.Fatalf("VerifiedAt = %v, want nil (no side effect on expiry)", d.VerifiedAt)
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(audit.events))
	}
	ev := audit.events[0]
	if ev.Action != "verify" || ev.Outcome != "denied:token_expired" || ev.Reason != management.ReasonTokenExpired {
		t.Fatalf("audit event = %+v", ev)
	}
}

// TestVerify_TokenAtTTLBoundary nails down the comparison semantics:
// exactly-at-TTL is still acceptable (>, not >=); one nanosecond past
// the TTL is rejected.
func TestVerify_TokenAtTTLBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour
	cases := []struct {
		name      string
		issuedAt  time.Time
		wantError bool
	}{
		{"exactly at TTL", now.Add(-ttl), false},
		{"one nanosecond past TTL", now.Add(-ttl - time.Nanosecond), true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			tenant := uuid.New()
			domainID := uuid.New()
			store.rows[domainID] = management.Domain{
				ID: domainID, TenantID: tenant, Host: "shop.example.com",
				VerificationToken: "tok",
				TokenIssuedAt:     tc.issuedAt,
			}
			dns := &fakeDNS{}
			uc := mustNew(t, management.Config{
				Store: store, Gate: &fakeGate{}, DNS: dns,
				Now:      fixedNow(now),
				TokenTTL: ttl,
			})
			_, err := uc.Verify(context.Background(), tenant, domainID)
			if tc.wantError {
				if !errors.Is(err, management.ErrTokenExpired) {
					t.Fatalf("err = %v, want ErrTokenExpired", err)
				}
				return
			}
			if errors.Is(err, management.ErrTokenExpired) {
				t.Fatalf("unexpected ErrTokenExpired at exact TTL boundary")
			}
		})
	}
}

// TestVerify_RejectsRotatedToken exercises the compare-and-swap path:
// when the row's verification_token changes between Get and
// MarkVerified, the use-case must surface ReasonTokenRotated, refuse
// to flip verified_at, and emit a denied:token_rotated audit event.
func TestVerify_RejectsRotatedToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	store.rows[domainID] = management.Domain{
		ID: domainID, TenantID: tenant, Host: "shop.example.com",
		VerificationToken: "T1",
		TokenIssuedAt:     now,
	}
	// Simulate a concurrent rotation that swaps the token between
	// Get and MarkVerified. fakeStore.beforeMarkVerified runs under
	// the same mutex so the swap is atomic with the CAS check.
	store.beforeMarkVerified = func(s *fakeStore) {
		d := s.rows[domainID]
		d.VerificationToken = "T2"
		s.rows[domainID] = d
	}
	dns := &fakeDNS{}
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{}, DNS: dns, Audit: audit,
		Now: fixedNow(now),
	})
	out, err := uc.Verify(context.Background(), tenant, domainID)
	if !errors.Is(err, management.ErrTokenRotated) {
		t.Fatalf("err = %v, want ErrTokenRotated", err)
	}
	if out.Reason != management.ReasonTokenRotated {
		t.Fatalf("reason = %v, want ReasonTokenRotated", out.Reason)
	}
	if d := store.rows[domainID]; d.VerifiedAt != nil {
		t.Fatalf("VerifiedAt = %v, want nil (CAS must prevent the write)", d.VerifiedAt)
	}
	var rotated *management.AuditEvent
	for i := range audit.events {
		if audit.events[i].Outcome == "denied:token_rotated" {
			rotated = &audit.events[i]
			break
		}
	}
	if rotated == nil {
		t.Fatalf("audit events missing denied:token_rotated: %+v", audit.events)
	}
	if rotated.Reason != management.ReasonTokenRotated {
		t.Fatalf("audit reason = %v", rotated.Reason)
	}
}

// TestVerify_AuditFingerprintRedacted guards the rule that the audit
// trail must NEVER carry the raw verification_token. The fingerprint
// is present, non-empty, but does not contain the token substring.
func TestVerify_AuditFingerprintRedacted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	rawToken := "deadbeefcafef00d1122334455667788"
	store := newFakeStore()
	tenant := uuid.New()
	domainID := uuid.New()
	store.rows[domainID] = management.Domain{
		ID: domainID, TenantID: tenant, Host: "shop.example.com",
		VerificationToken: rawToken,
		TokenIssuedAt:     now,
	}
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		DNS:   &fakeDNS{result: management.DNSCheckResult{WithDNSSEC: false}},
		Audit: audit,
		Now:   fixedNow(now),
	})
	if _, err := uc.Verify(context.Background(), tenant, domainID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(audit.events) == 0 {
		t.Fatal("audit events empty")
	}
	verifyEvent := audit.events[len(audit.events)-1]
	if verifyEvent.Action != "verify" {
		t.Fatalf("last audit action = %q, want verify", verifyEvent.Action)
	}
	if verifyEvent.TokenFingerprint == "" {
		t.Fatal("TokenFingerprint must be set on verify events that bind a token")
	}
	if strings.Contains(verifyEvent.TokenFingerprint, rawToken) {
		t.Fatalf("TokenFingerprint must not contain raw token: %q", verifyEvent.TokenFingerprint)
	}
	// SHA-256 first 8 bytes → 16 hex chars per the spec.
	if len(verifyEvent.TokenFingerprint) != 16 {
		t.Fatalf("TokenFingerprint length = %d, want 16", len(verifyEvent.TokenFingerprint))
	}
}

// TestEnroll_SetsTokenIssuedAt asserts Enroll stamps TokenIssuedAt to
// the clock's now, so a fresh enrollment begins its TTL window now and
// not at some zero-time epoch.
func TestEnroll_SetsTokenIssuedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		TokenGen: detTokenGen("tok"), Now: fixedNow(now),
	})
	res, err := uc.Enroll(context.Background(), uuid.New(), "shop.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if !res.Domain.TokenIssuedAt.Equal(now) {
		t.Fatalf("TokenIssuedAt = %v, want %v", res.Domain.TokenIssuedAt, now)
	}
	// And the persisted row must agree.
	persisted := store.rows[res.Domain.ID]
	if !persisted.TokenIssuedAt.Equal(now) {
		t.Fatalf("persisted TokenIssuedAt = %v, want %v", persisted.TokenIssuedAt, now)
	}
}

func TestRegenerateToken_HappyPath(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	now := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	store := newFakeStore()
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		TokenGen: detTokenGen("oldtok"), Now: fixedNow(now),
	})
	// Enroll to get a pending domain.
	res, err := uc.Enroll(context.Background(), tenantID, "regen.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	oldToken := res.Domain.VerificationToken

	// Regenerate with a new token generator.
	newNow := now.Add(time.Hour)
	uc2 := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		TokenGen: detTokenGen("newtok"), Now: fixedNow(newNow),
	})
	d, err := uc2.RegenerateToken(context.Background(), tenantID, res.Domain.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	if d.VerificationToken == oldToken {
		t.Fatalf("token unchanged after regeneration")
	}
	if !d.TokenIssuedAt.Equal(newNow) {
		t.Fatalf("TokenIssuedAt = %v, want %v", d.TokenIssuedAt, newNow)
	}
}

func TestRegenerateToken_AlreadyVerified(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	now := time.Now().UTC()
	store := newFakeStore()
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		TokenGen: detTokenGen("tok"), Now: fixedNow(now),
	})
	res, err := uc.Enroll(context.Background(), tenantID, "verified.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	// Mark verified directly in the store.
	d := store.rows[res.Domain.ID]
	t2 := now
	d.VerifiedAt = &t2
	store.rows[res.Domain.ID] = d

	_, err = uc.RegenerateToken(context.Background(), tenantID, res.Domain.ID)
	if !errors.Is(err, management.ErrAlreadyVerified) {
		t.Fatalf("expected ErrAlreadyVerified, got %v", err)
	}
}

func TestRegenerateToken_AuditFingerprint(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	now := time.Now().UTC()
	store := newFakeStore()
	audit := &fakeAudit{}
	uc := mustNew(t, management.Config{
		Store: store, Gate: &fakeGate{},
		TokenGen: detTokenGen("audittok"), Now: fixedNow(now),
		Audit: audit,
	})
	res, err := uc.Enroll(context.Background(), tenantID, "audit.example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	_, err = uc.RegenerateToken(context.Background(), tenantID, res.Domain.ID)
	if err != nil {
		t.Fatalf("RegenerateToken: %v", err)
	}
	var regenEv *management.AuditEvent
	for i := range audit.events {
		if audit.events[i].Action == "regenerate_token" {
			ev := audit.events[i]
			regenEv = &ev
		}
	}
	if regenEv == nil {
		t.Fatal("no regenerate_token audit event emitted")
	}
	if regenEv.Outcome != "ok" {
		t.Fatalf("Outcome = %q, want ok", regenEv.Outcome)
	}
	if len(regenEv.TokenFingerprint) != 16 {
		t.Fatalf("TokenFingerprint length = %d, want 16", len(regenEv.TokenFingerprint))
	}
}
