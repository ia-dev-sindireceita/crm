package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	customdomainhttp "github.com/pericles-luz/crm/internal/adapter/transport/http/customdomain"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
	"github.com/pericles-luz/crm/internal/customdomain/management"
	"github.com/pericles-luz/crm/internal/customdomain/validation"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

// fakeCustomDomainPool satisfies pgstore.PgxRowsConn + Close for the
// happy-path test. List returns no rows; the page renders the empty
// state without any DB chatter.
type fakeCustomDomainPool struct {
	closed bool
}

func (f *fakeCustomDomainPool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errPgxRow{}
}
func (f *fakeCustomDomainPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeCustomDomainPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return emptyRows{}, nil
}
func (f *fakeCustomDomainPool) Close() { f.closed = true }

type errPgxRow struct{}

func (errPgxRow) Scan(_ ...any) error { return pgx.ErrNoRows }

type emptyRows struct{}

func (emptyRows) Close()                                       {}
func (emptyRows) Err() error                                   { return nil }
func (emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (emptyRows) RawValues() [][]byte                          { return nil }
func (emptyRows) Values() ([]any, error)                       { return nil, nil }
func (emptyRows) Conn() *pgx.Conn                              { return nil }
func (emptyRows) Next() bool                                   { return false }
func (emptyRows) Scan(_ ...any) error                          { return errors.New("emptyRows: scan") }

func TestBuildCustomDomainHandler_DisabledByDefault(t *testing.T) {
	t.Parallel()
	getenv := func(string) string { return "" }
	h, cleanup := buildCustomDomainHandler(context.Background(), getenv)
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when flag unset")
	}
}

func TestBuildCustomDomainHandler_RequiresDSN(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		}
		return ""
	}
	h, cleanup := buildCustomDomainHandler(context.Background(), getenv)
	defer cleanup()
	if h != nil {
		t.Fatal("expected nil when DSN missing")
	}
}

func TestBuildCustomDomainHandler_RequiresSecret(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case "DATABASE_URL":
			return "postgres://example/db"
		}
		return ""
	}
	h, cleanup := buildCustomDomainHandler(context.Background(), getenv)
	defer cleanup()
	if h != nil {
		t.Fatal("expected nil when CSRF secret missing")
	}
}

func TestBuildCustomDomainHandler_HappyPathWithStubPool(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case "DATABASE_URL":
			return "postgres://example/db"
		case envCustomDomainCSRF:
			return strings.Repeat("a", 32)
		case envCustomDomainPrimary:
			return "exemplo.com"
		}
		return ""
	}
	dial := func(_ context.Context, _ string) (customDomainPool, error) {
		return &fakeCustomDomainPool{}, nil
	}
	h, cleanup := buildCustomDomainHandlerWith(context.Background(), getenv, dial)
	if h == nil {
		t.Fatal("expected handler when deps satisfied")
	}
	defer cleanup()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil)
	req.Header.Set(envCustomDomainTenantHd, uuid.New().String())
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBuildCustomDomainHandler_DialError(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case "DATABASE_URL":
			return "postgres://example/db"
		case envCustomDomainCSRF:
			return strings.Repeat("a", 32)
		}
		return ""
	}
	dial := func(_ context.Context, _ string) (customDomainPool, error) {
		return nil, errors.New("boom")
	}
	h, cleanup := buildCustomDomainHandlerWith(context.Background(), getenv, dial)
	defer cleanup()
	if h != nil {
		t.Fatal("expected nil handler on dial error")
	}
}

func TestBuildCustomDomainHandler_BadDSN(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case "DATABASE_URL":
			return "not-a-real-dsn"
		case envCustomDomainCSRF:
			return strings.Repeat("a", 32)
		}
		return ""
	}
	h, cleanup := buildCustomDomainHandler(context.Background(), getenv)
	defer cleanup()
	if h != nil {
		t.Fatal("expected nil with unreachable DSN")
	}
}

// TestRegisterCustomDomainRoutes_ServesStatic exercises the
// /static/ + /tenant/custom-domains routes against the registered mux
// using a stubbed-in handler so we don't need real DB.
func TestRegisterCustomDomainRoutes_ServesStatic(t *testing.T) {
	t.Parallel()
	uc := &fakeStubUseCase{}
	h, err := customdomainhttp.New(customdomainhttp.Config{
		UseCase: uc,
		CSRF:    customdomainhttp.CSRFConfig{Secret: []byte(strings.Repeat("a", 32))},
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	mux := registerCustomDomainRoutes(h)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil)
	req = req.WithContext(customdomainhttp.WithTenantID(req.Context(), uuid.New()))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWrapWithDevTenantHeader_Parses(t *testing.T) {
	t.Parallel()
	called := false
	target := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		if customdomainhttp.TenantIDFromContext(r.Context()) == uuid.Nil {
			t.Errorf("tenant context missing despite header")
		}
	})
	mw := wrapWithDevTenantHeader(target, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(envCustomDomainTenantHd, uuid.New().String())
	mw.ServeHTTP(rec, req)
	if !called {
		t.Fatal("downstream not invoked")
	}
}

func TestWrapWithDevTenantHeader_BadUUIDPasses(t *testing.T) {
	t.Parallel()
	target := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if customdomainhttp.TenantIDFromContext(r.Context()) != uuid.Nil {
			t.Error("expected no tenant on bad header")
		}
	})
	mw := wrapWithDevTenantHeader(target, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(envCustomDomainTenantHd, "not-a-uuid")
	mw.ServeHTTP(rec, req)
}

func TestWrapWithDevTenantHeader_NoHeader(t *testing.T) {
	t.Parallel()
	target := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
	mw := wrapWithDevTenantHeader(target, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mw.ServeHTTP(rec, req)
}

func TestEnrollmentGateAdapter_Decisions(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	cases := []struct {
		name       string
		store      enrollment.CountStore
		counter    enrollment.WindowCounter
		breaker    enrollment.Breaker
		quota      enrollment.Quota
		wantAllow  bool
		wantReason management.Reason
	}{
		{"allowed", zeroCount{}, passWindowCounter{}, zeroBreaker{}, enrollment.DefaultQuota(), true, management.ReasonNone},
		{"hard cap", overCount{n: 50}, passWindowCounter{}, zeroBreaker{}, enrollment.Quota{Hourly: 5, Daily: 20, Monthly: 50, HardCap: 25}, false, management.ReasonRateLimited},
		{"hourly", zeroCount{}, overCounter{n: 99}, zeroBreaker{}, enrollment.DefaultQuota(), false, management.ReasonRateLimited},
		{"breaker", zeroCount{}, passWindowCounter{}, openBreaker{}, enrollment.DefaultQuota(), false, management.ReasonInternal},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate := enrollmentGateAdapter{gate: enrollment.New(tc.store, tc.counter, tc.breaker, nil, time.Now, tc.quota)}
			dec := gate.Allow(context.Background(), tenant)
			if dec.Allowed != tc.wantAllow {
				t.Errorf("allow = %v, want %v", dec.Allowed, tc.wantAllow)
			}
			if dec.Reason != tc.wantReason {
				t.Errorf("reason = %v, want %v", dec.Reason, tc.wantReason)
			}
		})
	}
}

func TestEnrollmentGateAdapter_PortError(t *testing.T) {
	t.Parallel()
	gate := enrollmentGateAdapter{gate: enrollment.New(errCount{}, passWindowCounter{}, zeroBreaker{}, nil, time.Now, enrollment.DefaultQuota())}
	dec := gate.Allow(context.Background(), uuid.New())
	if dec.Err == nil {
		t.Fatal("expected wrapped error")
	}
}

func TestSlugReleaseAdapter_NilSafe(t *testing.T) {
	t.Parallel()
	if err := (slugReleaseAdapter{}).ReleaseSlug(context.Background(), "x", uuid.New()); err != nil {
		t.Fatalf("nil svc should be no-op, got %v", err)
	}
}

func TestSlugReleaseAdapter_PassesThrough(t *testing.T) {
	t.Parallel()
	store := &memSlugStore{}
	svc, err := slugreservation.NewService(store, &memRedirectStore{}, nopAudit{}, nopSlack{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	a := slugReleaseAdapter{svc: svc}
	if err := a.ReleaseSlug(context.Background(), "shop.example.com", uuid.New()); err != nil {
		t.Fatalf("ReleaseSlug: %v", err)
	}
}

func TestSlugReleaseAdapter_PropagatesError(t *testing.T) {
	t.Parallel()
	store := &memSlugStore{insertErr: errors.New("dup")}
	svc, _ := slugreservation.NewService(store, &memRedirectStore{}, nopAudit{}, nopSlack{}, nil)
	a := slugReleaseAdapter{svc: svc}
	if err := a.ReleaseSlug(context.Background(), "shop.example.com", uuid.New()); err == nil {
		t.Fatal("expected error")
	}
}

func TestManagementAuditLogManagement_Smoke(t *testing.T) {
	t.Parallel()
	managementAudit{logger: slog.Default()}.LogManagement(context.Background(), management.AuditEvent{
		TenantID: uuid.New(), DomainID: uuid.New(), Host: "shop.example.com",
		Action: "enroll", Outcome: "ok", Reason: management.ReasonNone, At: time.Now(),
	})
}

func TestFirstLabel_Cases(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"shop.example.com": "shop",
		"shop":             "shop",
		".leading":         "",
		"a.b":              "a",
		"":                 "",
	}
	for in, want := range cases {
		if got := firstLabel(in); got != want {
			t.Errorf("firstLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultCustomDomainDial_BadDSN(t *testing.T) {
	t.Parallel()
	if _, err := defaultCustomDomainDial(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty DSN")
	}
	if _, err := defaultCustomDomainDial(context.Background(), "not-a-dsn"); err == nil {
		t.Fatal("expected error on malformed DSN")
	}
}

// TestDefaultDial_BadInput exercises the existing pre-flag dialFn so
// the cmd/server package coverage stays above the CTO bar even though
// the underlying function predates this ticket. Existing tests left it
// at 0%; this only adds, never modifies, an existing test.
func TestDefaultDial_BadInput(t *testing.T) {
	t.Parallel()
	if _, err := defaultDial(context.Background(), func(string) string { return "" }); err == nil {
		t.Fatal("expected error on missing DSN/REDIS_URL")
	}
}

func TestEnrollmentGateAdapter_DefaultAllowed(t *testing.T) {
	t.Parallel()
	gate := enrollmentGateAdapter{gate: enrollment.New(zeroCount{}, passWindowCounter{}, zeroBreaker{}, nil, time.Now, enrollment.DefaultQuota())}
	dec := gate.Allow(context.Background(), uuid.New())
	if !dec.Allowed {
		t.Fatalf("dec = %+v", dec)
	}
}

func TestPlaceholderAdapters(t *testing.T) {
	t.Parallel()
	if n, err := (zeroCount{}).ActiveCount(context.Background(), uuid.Nil); err != nil || n != 0 {
		t.Errorf("zeroCount: n=%d err=%v", n, err)
	}
	if n, err := (passWindowCounter{}).CountAndRecord(context.Background(), uuid.Nil, enrollment.WindowHour, time.Now()); err != nil || n != 0 {
		t.Errorf("passWindowCounter: n=%d err=%v", n, err)
	}
	if open, err := (zeroBreaker{}).IsOpen(context.Background(), uuid.Nil, time.Now()); err != nil || open {
		t.Errorf("zeroBreaker: open=%v err=%v", open, err)
	}
	if err := (nopAudit{}).LogMasterOverride(context.Background(), slugreservation.MasterOverrideEvent{}); err != nil {
		t.Errorf("nopAudit: %v", err)
	}
	if err := (nopSlack{}).NotifyAlert(context.Background(), "hi"); err != nil {
		t.Errorf("nopSlack: %v", err)
	}
}

// fakeStubUseCase satisfies customdomainhttp.UseCase for the
// registerCustomDomainRoutes test. Methods return zero values so the
// list page renders an empty table.
type fakeStubUseCase struct{}

func (fakeStubUseCase) List(context.Context, uuid.UUID) ([]management.Domain, error) {
	return nil, nil
}
func (fakeStubUseCase) Get(context.Context, uuid.UUID, uuid.UUID) (management.Domain, error) {
	return management.Domain{}, management.ErrStoreNotFound
}
func (fakeStubUseCase) Enroll(context.Context, uuid.UUID, string) (management.EnrollResult, error) {
	return management.EnrollResult{}, nil
}
func (fakeStubUseCase) Verify(context.Context, uuid.UUID, uuid.UUID) (management.VerifyOutcome, error) {
	return management.VerifyOutcome{}, nil
}
func (fakeStubUseCase) SetPaused(context.Context, uuid.UUID, uuid.UUID, bool) (management.Domain, error) {
	return management.Domain{}, nil
}
func (fakeStubUseCase) Delete(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (fakeStubUseCase) RegenerateToken(context.Context, uuid.UUID, uuid.UUID) (management.Domain, error) {
	return management.Domain{}, nil
}

// overCount drives the hard-cap branch.
type overCount struct{ n int }

func (o overCount) ActiveCount(context.Context, uuid.UUID) (int, error) { return o.n, nil }

type errCount struct{}

func (errCount) ActiveCount(context.Context, uuid.UUID) (int, error) {
	return 0, errors.New("pg down")
}

// overCounter drives a quota-window denial.
type overCounter struct{ n int }

func (o overCounter) CountAndRecord(context.Context, uuid.UUID, enrollment.Window, time.Time) (int, error) {
	return o.n, nil
}

// openBreaker drives the circuit-breaker branch.
type openBreaker struct{}

func (openBreaker) IsOpen(context.Context, uuid.UUID, time.Time) (bool, error) { return true, nil }

// memSlugStore is a tiny in-memory slugreservation.Store for the
// adapter tests. It is intentionally simpler than the integration
// fixtures used in slugreservation/service_test.go because the wiring
// tests only need pass-through and error paths.
type memSlugStore struct {
	insertErr error
}

func (m *memSlugStore) Active(context.Context, string) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, slugreservation.ErrNotReserved
}
func (m *memSlugStore) Insert(_ context.Context, slug string, byTenant uuid.UUID, releasedAt, expiresAt time.Time) (slugreservation.Reservation, error) {
	if m.insertErr != nil {
		return slugreservation.Reservation{}, m.insertErr
	}
	return slugreservation.Reservation{ID: uuid.New(), Slug: slug, ReleasedAt: releasedAt, ReleasedByTenantID: byTenant, ExpiresAt: expiresAt, CreatedAt: releasedAt}, nil
}
func (m *memSlugStore) SoftDelete(context.Context, string, time.Time) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, slugreservation.ErrNotReserved
}

type memRedirectStore struct{}

func (memRedirectStore) Active(context.Context, string) (slugreservation.Redirect, error) {
	return slugreservation.Redirect{}, slugreservation.ErrNotReserved
}
func (memRedirectStore) Upsert(context.Context, string, string, time.Time) (slugreservation.Redirect, error) {
	return slugreservation.Redirect{}, nil
}

// fakeWireResolver is a deterministic in-memory dnsresolver.Resolver for
// the SIN-62313 wire-up tests. It mirrors the validation package's test
// fake but lives here so cmd/server can drive the production wire-up
// (validator → adapters → management.UseCase) without a network round-trip.
type fakeWireResolver struct {
	ipAnswers map[string][]dnsresolver.IPAnswer
	ipErrs    map[string]error
	txtAns    map[string][]string
	txtErrs   map[string]error
}

func newFakeWireResolver() *fakeWireResolver {
	return &fakeWireResolver{
		ipAnswers: map[string][]dnsresolver.IPAnswer{},
		ipErrs:    map[string]error{},
		txtAns:    map[string][]string{},
		txtErrs:   map[string]error{},
	}
}

func (f *fakeWireResolver) LookupIP(_ context.Context, host string) ([]dnsresolver.IPAnswer, error) {
	if e, ok := f.ipErrs[host]; ok {
		return nil, e
	}
	return f.ipAnswers[host], nil
}

func (f *fakeWireResolver) LookupTXT(_ context.Context, host string) ([]string, error) {
	if e, ok := f.txtErrs[host]; ok {
		return nil, e
	}
	return f.txtAns[host], nil
}

func newWireValidator(r dnsresolver.Resolver) *validation.Validator {
	return validation.New(r, nil, validation.SystemClock{})
}

// TestDefaultCustomDomainResolverFactory_ConstructsResolver verifies the
// factory respects the env vars; we don't call LookupIP because that
// would touch the network.
func TestDefaultCustomDomainResolverFactory_ConstructsResolver(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainDNSSrv:
			return "unbound:5353"
		case envCustomDomainDNSSEC:
			return "true"
		}
		return ""
	}
	r := defaultCustomDomainResolverFactory(getenv)
	if r == nil {
		t.Fatal("resolver factory returned nil")
	}
}

// TestDefaultCustomDomainResolverFactory_DefaultsAndDNSSECOff exercises
// the default-server + DNSSEC-toggle paths so the factory's two
// explicit branches stay covered.
func TestDefaultCustomDomainResolverFactory_DefaultsAndDNSSECOff(t *testing.T) {
	t.Parallel()
	if r := defaultCustomDomainResolverFactory(func(string) string { return "" }); r == nil {
		t.Fatal("resolver factory returned nil for empty env")
	}
	if r := defaultCustomDomainResolverFactory(func(k string) string {
		if k == envCustomDomainDNSSEC {
			return "false"
		}
		return ""
	}); r == nil {
		t.Fatal("resolver factory returned nil with DNSSEC=false")
	}
	if r := defaultCustomDomainResolverFactory(func(k string) string {
		if k == envCustomDomainDNSSEC {
			return "not-a-bool"
		}
		return ""
	}); r == nil {
		t.Fatal("resolver factory returned nil with malformed DNSSEC env (should fall back to default)")
	}
}

// TestHostValidatorAdapter_PassesAllowlist arms a single public IP and
// asserts the adapter returns nil so management.NormalizeHost +
// hostValidatorAdapter form a complete enrollment pre-flight.
func TestHostValidatorAdapter_PassesAllowlist(t *testing.T) {
	t.Parallel()
	r := newFakeWireResolver()
	r.ipAnswers["shop.example.com"] = []dnsresolver.IPAnswer{
		{IP: netip.MustParseAddr("203.0.113.10"), VerifiedWithDNSSEC: true},
	}
	a := hostValidatorAdapter{v: newWireValidator(r)}
	if err := a.Validate(context.Background(), "shop.example.com"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestHostValidatorAdapter_BlockedSSRF asserts the validator's
// ErrPrivateIP is wrapped in management.ErrPrivateIP so
// classifyValidationError can map it to ReasonPrivateIP at the
// boundary.
func TestHostValidatorAdapter_BlockedSSRF(t *testing.T) {
	t.Parallel()
	r := newFakeWireResolver()
	r.ipAnswers["evil.example.com"] = []dnsresolver.IPAnswer{
		{IP: netip.MustParseAddr("127.0.0.1")},
	}
	a := hostValidatorAdapter{v: newWireValidator(r)}
	err := a.Validate(context.Background(), "evil.example.com")
	if !errors.Is(err, management.ErrPrivateIP) {
		t.Fatalf("err = %v, want management.ErrPrivateIP", err)
	}
	if !errors.Is(err, validation.ErrPrivateIP) {
		t.Fatalf("err must still chain to validation.ErrPrivateIP for audit: %v", err)
	}
}

// TestHostValidatorAdapter_EmptyHostMapsToInvalidHost asserts the
// validator's ErrEmptyHost is wrapped in management.ErrInvalidHost so
// the boundary returns ReasonInvalidHost.
func TestHostValidatorAdapter_EmptyHostMapsToInvalidHost(t *testing.T) {
	t.Parallel()
	a := hostValidatorAdapter{v: newWireValidator(newFakeWireResolver())}
	err := a.Validate(context.Background(), "")
	if !errors.Is(err, management.ErrInvalidHost) {
		t.Fatalf("err = %v, want management.ErrInvalidHost", err)
	}
}

// TestHostValidatorAdapter_OtherErrorPropagates asserts non-sentinel
// resolver errors propagate without wrap so the boundary maps them to
// ReasonDNSResolutionFailed.
func TestHostValidatorAdapter_OtherErrorPropagates(t *testing.T) {
	t.Parallel()
	r := newFakeWireResolver()
	r.ipErrs["bork.example.com"] = dnsresolver.ErrTimeout
	a := hostValidatorAdapter{v: newWireValidator(r)}
	err := a.Validate(context.Background(), "bork.example.com")
	if !errors.Is(err, dnsresolver.ErrTimeout) {
		t.Fatalf("err must wrap dnsresolver.ErrTimeout, got %v", err)
	}
}

// TestDNSCheckerAdapter_HappyPathSetsDNSSEC is the SIN-62313 acceptance
// criterion at the adapter level: when validation succeeds the DNSSEC
// flag flows through to the management.DNSCheckResult.
func TestDNSCheckerAdapter_HappyPathSetsDNSSEC(t *testing.T) {
	t.Parallel()
	r := newFakeWireResolver()
	r.ipAnswers["shop.example.com"] = []dnsresolver.IPAnswer{
		{IP: netip.MustParseAddr("203.0.113.10"), VerifiedWithDNSSEC: true},
	}
	r.txtAns["_crm-verify.shop.example.com"] = []string{"crm-verify=tok"}
	a := dnsCheckerAdapter{v: newWireValidator(r)}
	res, err := a.Check(context.Background(), "shop.example.com", "crm-verify=tok")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if !res.WithDNSSEC {
		t.Fatalf("expected WithDNSSEC=true when resolver signals AD bit")
	}
}

// TestDNSCheckerAdapter_TokenMismatchWrapsManagementSentinel asserts
// validation.ErrTokenMismatch is rewrapped so the boundary's
// classifyValidationError returns ReasonTokenMismatch (not the generic
// ReasonDNSResolutionFailed).
func TestDNSCheckerAdapter_TokenMismatchWrapsManagementSentinel(t *testing.T) {
	t.Parallel()
	r := newFakeWireResolver()
	r.ipAnswers["shop.example.com"] = []dnsresolver.IPAnswer{
		{IP: netip.MustParseAddr("203.0.113.10")},
	}
	r.txtAns["_crm-verify.shop.example.com"] = []string{"crm-verify=other"}
	a := dnsCheckerAdapter{v: newWireValidator(r)}
	_, err := a.Check(context.Background(), "shop.example.com", "crm-verify=tok")
	if !errors.Is(err, management.ErrTokenMismatch) {
		t.Fatalf("err = %v, want management.ErrTokenMismatch", err)
	}
	if !errors.Is(err, validation.ErrTokenMismatch) {
		t.Fatalf("err must still chain to validation.ErrTokenMismatch: %v", err)
	}
}

// TestDNSCheckerAdapter_PrivateIPWrapsManagementSentinel mirrors the
// TokenMismatch test for the SSRF branch.
func TestDNSCheckerAdapter_PrivateIPWrapsManagementSentinel(t *testing.T) {
	t.Parallel()
	r := newFakeWireResolver()
	r.ipAnswers["evil.example.com"] = []dnsresolver.IPAnswer{
		{IP: netip.MustParseAddr("127.0.0.1")},
	}
	a := dnsCheckerAdapter{v: newWireValidator(r)}
	_, err := a.Check(context.Background(), "evil.example.com", "crm-verify=tok")
	if !errors.Is(err, management.ErrPrivateIP) {
		t.Fatalf("err = %v, want management.ErrPrivateIP", err)
	}
}

// TestDNSCheckerAdapter_OtherErrorPropagates ensures resolver-level
// failures (timeouts, no record) flow through unwrapped so they map to
// ReasonDNSResolutionFailed at the boundary.
func TestDNSCheckerAdapter_OtherErrorPropagates(t *testing.T) {
	t.Parallel()
	r := newFakeWireResolver()
	r.ipErrs["bork.example.com"] = dnsresolver.ErrTimeout
	a := dnsCheckerAdapter{v: newWireValidator(r)}
	_, err := a.Check(context.Background(), "bork.example.com", "crm-verify=tok")
	if !errors.Is(err, dnsresolver.ErrTimeout) {
		t.Fatalf("err must wrap dnsresolver.ErrTimeout, got %v", err)
	}
}

// TestValidationAuditRecord_Smoke ensures the slog adapter does not panic
// on either the no-Detail or has-Detail path. We do not assert log output
// because the production logger is global; coverage of the branch is
// enough.
func TestValidationAuditRecord_Smoke(t *testing.T) {
	t.Parallel()
	a := validationAudit{logger: slog.Default()}
	a.Record(context.Background(), validation.AuditEvent{Event: "x", Host: "shop.example.com", At: time.Now()})
	a.Record(context.Background(), validation.AuditEvent{
		Event:  "y",
		Host:   "shop.example.com",
		At:     time.Now(),
		Detail: map[string]string{"phase": "host_only"},
	})
}

// TestBuildCustomDomainHandler_WithFakeResolver_HappyPath wires the full
// stack with a stub pool + fake resolver and exercises a GET to confirm
// the resolver factory is invoked exactly when the feature flag enables
// the handler. (Verify-path end-to-end coverage lives in handler_test.go
// where the management.UseCase fake is more ergonomic.)
func TestBuildCustomDomainHandler_WithFakeResolver_HappyPath(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envCustomDomainUI:
			return "1"
		case "DATABASE_URL":
			return "postgres://example/db"
		case envCustomDomainCSRF:
			return strings.Repeat("a", 32)
		}
		return ""
	}
	dial := func(_ context.Context, _ string) (customDomainPool, error) {
		return &fakeCustomDomainPool{}, nil
	}
	resolverCalled := false
	resolverFactory := func(getenv func(string) string) dnsresolver.Resolver {
		resolverCalled = true
		return newFakeWireResolver()
	}
	h, cleanup := buildCustomDomainHandlerWithDeps(context.Background(), getenv, dial, resolverFactory)
	if h == nil {
		t.Fatal("expected handler when deps satisfied")
	}
	defer cleanup()
	if !resolverCalled {
		t.Fatal("resolver factory must be invoked once when handler is wired")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil)
	req.Header.Set(envCustomDomainTenantHd, uuid.New().String())
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
