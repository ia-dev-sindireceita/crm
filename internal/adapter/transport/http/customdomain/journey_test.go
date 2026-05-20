package customdomain_test

// Chained user-journey integration test for the SIN-62259 custom-domain
// management UI. SIN-62314 acceptance criterion: a single Go test that
// runs add → verify → pause → resume → delete → reservation lock against
// the real management.UseCase and the real slugreservation.Service so
// the chain catches state-transition regressions per-handler tests with
// a faked use-case cannot cover.
//
// Stub layout (all in-memory, no Postgres / no Redis required):
//
//   - journeyDomainStore        — management.Store, mirrors the Postgres
//                                  contract (soft-deleted rows hidden
//                                  from List, kept for GetByID).
//   - journeySlugStore          — slugreservation.Store with
//                                  partial-unique-index semantics on
//                                  active reservations and expiry-aware
//                                  Active() filtering.
//   - journeyRedirectStore      — slugreservation.RedirectStore stub
//                                  (the journey does not exercise
//                                  redirects).
//   - journeyAlwaysAllowGate    — management.EnrollmentGate that always
//                                  permits enrolment (per-tenant quota
//                                  has its own per-handler tests).
//   - journeyDNS                — management.DNSChecker stub that
//                                  always succeeds (Verify reads the
//                                  stored token, the stub only confirms
//                                  the lookup path executes).
//   - journeySlugValidator      — management.HostValidator that
//                                  consults slugreservation.Service.
//                                  CheckAvailable and wraps
//                                  *ReservedError as
//                                  management.ErrSlugReserved so the
//                                  use-case classifies it as
//                                  ReasonSlugReserved.
//   - journeySlugReleaser       — management.SlugReleaser that calls
//                                  slugreservation.Service.ReleaseSlug
//                                  on host first-label (mirrors
//                                  cmd/server slugReleaseAdapter).
//
// Extending the harness: add a new field to the test setup and reach
// for the existing stubs above unless you need a new contract surface.
// Do not import the integration build-tag harness in this file — the
// chain runs in `go test ./...` (no Postgres) so the unit suite catches
// regressions on every CI run.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	cd "github.com/pericles-luz/crm/internal/adapter/transport/http/customdomain"
	"github.com/pericles-luz/crm/internal/customdomain/management"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

// TestUserJourney_AddVerifyPauseResumeDeleteReservation drives the full
// SIN-62259 chain end-to-end:
//
//  1. POST /enroll        — wizard step 2 with the TXT record
//  2. POST /verify        — flips status to "verified"
//  3. PATCH ?paused=true  — flips status to "paused"
//  4. PATCH ?paused=false — flips status back to "verified"
//  5. DELETE              — soft-deletes + reserves the slug for 12mo
//  6. POST /enroll (same) — blocked by the SIN-62244 lock with PT-BR copy
//
// The reservation leg uses the real slugreservation.Service so the
// chained test gives defense-in-depth coverage of the F46 lock — the
// gate is not mocked away.
func TestUserJourney_AddVerifyPauseResumeDeleteReservation(t *testing.T) {
	t.Parallel()

	journeyTenant := uuid.New()
	fixedNow := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	clock := journeyClock{now: fixedNow}

	slugStore := newJourneySlugStore(clock)
	slugSvc, err := slugreservation.NewService(
		slugStore,
		journeyRedirectStore{},
		journeyAudit{},
		journeySlack{},
		clock,
	)
	if err != nil {
		t.Fatalf("slugreservation.NewService: %v", err)
	}

	domainStore := newJourneyDomainStore()
	uc, err := management.New(management.Config{
		Store:     domainStore,
		Gate:      journeyAlwaysAllowGate{},
		Validator: journeySlugValidator{svc: slugSvc},
		DNS:       journeyDNS{},
		Slug:      journeySlugReleaser{svc: slugSvc},
		TokenGen:  func() (string, error) { return "tok-journey-001", nil },
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("management.New: %v", err)
	}

	h, err := cd.New(cd.Config{
		UseCase:       uc,
		CSRF:          cd.CSRFConfig{Secret: []byte(testCSRFSecret)},
		PrimaryDomain: "exemplo.com",
		Now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	const host = "shop.example.com"
	ctx := context.Background()

	// Step 1 — POST /tenant/custom-domains: enroll renders wizard step 2.
	rec1 := journeyFormPost(t, mux, journeyTenant, "/tenant/custom-domains", "host="+host)
	if rec1.Code != http.StatusOK {
		t.Fatalf("step1 enroll: status = %d body=%s", rec1.Code, rec1.Body.String())
	}
	body1 := rec1.Body.String()
	if !strings.Contains(body1, "_crm-verify."+host) {
		t.Fatalf("step1: missing TXT record name in step2 body: %s", body1)
	}
	if !strings.Contains(body1, "crm-verify=tok-journey-001") {
		t.Fatalf("step1: missing TXT value in step2 body: %s", body1)
	}

	// Resolve the freshly-inserted domain through the real use-case so
	// later steps can address it directly. The wizard step 2 template
	// also embeds the ID, but parsing HTML for the assertion would make
	// the test brittle to template tweaks.
	domains, err := uc.List(ctx, journeyTenant)
	if err != nil {
		t.Fatalf("step1 list: %v", err)
	}
	if len(domains) != 1 {
		t.Fatalf("step1: expected 1 domain after enroll, got %d", len(domains))
	}
	domainID := domains[0].ID

	// Step 2 — POST /api/customdomains/{id}/verify: flip to verified.
	verifyPath := "/api/customdomains/" + domainID.String() + "/verify"
	rec2 := journeyHTMXRequest(t, mux, journeyTenant, http.MethodPost, verifyPath)
	if rec2.Code != http.StatusOK {
		t.Fatalf("step2 verify: status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "Verificado") {
		t.Fatalf("step2: row missing verified badge: %s", body2)
	}
	if d, err := domainStore.GetByID(ctx, domainID); err != nil || d.VerifiedAt == nil {
		t.Fatalf("step2: store row not flipped to verified_at (err=%v)", err)
	}

	// Step 3 — PATCH ?paused=true: tls_paused_at set, badge "Pausado".
	pausePath := "/api/customdomains/" + domainID.String()
	rec3 := journeyHTMXRequest(t, mux, journeyTenant, http.MethodPatch, pausePath+"?paused=true")
	if rec3.Code != http.StatusOK {
		t.Fatalf("step3 pause: status = %d body=%s", rec3.Code, rec3.Body.String())
	}
	if !strings.Contains(rec3.Body.String(), "Pausado") {
		t.Fatalf("step3: row missing paused badge: %s", rec3.Body.String())
	}
	if d, _ := domainStore.GetByID(ctx, domainID); d.TLSPausedAt == nil {
		t.Fatal("step3: store row missing tls_paused_at")
	}

	// Step 4 — PATCH ?paused=false: tls_paused_at cleared, back to verified.
	rec4 := journeyHTMXRequest(t, mux, journeyTenant, http.MethodPatch, pausePath+"?paused=false")
	if rec4.Code != http.StatusOK {
		t.Fatalf("step4 resume: status = %d body=%s", rec4.Code, rec4.Body.String())
	}
	if !strings.Contains(rec4.Body.String(), "Verificado") {
		t.Fatalf("step4: row missing verified badge after resume: %s", rec4.Body.String())
	}
	if strings.Contains(rec4.Body.String(), "Pausado") {
		t.Fatalf("step4: row still has paused badge: %s", rec4.Body.String())
	}
	if d, _ := domainStore.GetByID(ctx, domainID); d.TLSPausedAt != nil {
		t.Fatal("step4: store row tls_paused_at not cleared")
	}

	// Step 5 — DELETE: soft-delete + 12-month slug reservation lock.
	rec5 := journeyHTMXRequest(t, mux, journeyTenant, http.MethodDelete, pausePath)
	if rec5.Code != http.StatusOK {
		t.Fatalf("step5 delete: status = %d body=%s", rec5.Code, rec5.Body.String())
	}
	domainsAfterDelete, err := uc.List(ctx, journeyTenant)
	if err != nil {
		t.Fatalf("step5 list: %v", err)
	}
	if len(domainsAfterDelete) != 0 {
		t.Fatalf("step5: expected 0 domains after delete, got %d", len(domainsAfterDelete))
	}
	if d, _ := domainStore.GetByID(ctx, domainID); d.DeletedAt == nil {
		t.Fatal("step5: store row missing deleted_at")
	}
	// The slug is reserved for the next 12 months; SIN-62244 store
	// returns *ReservedError wrapping ErrSlugReserved.
	checkErr := slugSvc.CheckAvailable(ctx, journeyFirstLabel(host))
	if !errors.Is(checkErr, slugreservation.ErrSlugReserved) {
		t.Fatalf("step5: slug reservation lock not engaged: err=%v", checkErr)
	}

	// Step 6 — POST /tenant/custom-domains for the same host: blocked.
	rec6 := journeyFormPost(t, mux, journeyTenant, "/tenant/custom-domains", "host="+host)
	if rec6.Code != http.StatusUnprocessableEntity {
		t.Fatalf("step6 reserved enroll: status = %d (want 422), body=%s", rec6.Code, rec6.Body.String())
	}
	body6 := rec6.Body.String()
	if !strings.Contains(body6, "reservado") {
		t.Fatalf("step6: missing PT-BR reservation copy: %s", body6)
	}
	if !strings.Contains(body6, "Escolha outro") {
		t.Fatalf("step6: missing PT-BR reservation suffix: %s", body6)
	}
}

// TestServeEnroll_SlugReservedRendersError covers the
// management.ErrSlugReserved error branch in serveEnroll directly via
// the fakeUseCase. The chained TestUserJourney exercises this path
// through the real management.UseCase; this case adds an isolated
// per-handler assertion that confirms ReservedUntil flows into the
// PT-BR copy when the use-case populates it.
func TestServeEnroll_SlugReservedRendersError(t *testing.T) {
	t.Parallel()
	until := time.Date(2027, 5, 6, 0, 0, 0, 0, time.UTC)
	uc := &fakeUseCase{
		enrollErr: management.ErrSlugReserved,
		enrollResp: management.EnrollResult{
			Reason:        management.ReasonSlugReserved,
			ReservedUntil: &until,
		},
	}
	mux := newServeMux(newHandlerForTest(t, uc))
	rec := formPostWithCSRF(t, mux, "/tenant/custom-domains", "host=shop.example.com")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "reservado até 06/05/2027") {
		t.Fatalf("missing PT-BR reserved-until date: %s", body)
	}
	if !strings.Contains(body, "Escolha outro") {
		t.Fatalf("missing PT-BR slug-reserved suffix: %s", body)
	}
}

// journeyDomainStore is the in-memory management.Store used by the
// chained test. Mirrors the SQL adapter's contract: rows live by id;
// soft-deleted rows are filtered from List but still returned by
// GetByID (deletion preserves the row for audit/RLS).
type journeyDomainStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]management.Domain
}

func newJourneyDomainStore() *journeyDomainStore {
	return &journeyDomainStore{rows: map[uuid.UUID]management.Domain{}}
}

func (s *journeyDomainStore) List(_ context.Context, tenantID uuid.UUID) ([]management.Domain, error) {
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

func (s *journeyDomainStore) GetByID(_ context.Context, id uuid.UUID) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok {
		return management.Domain{}, management.ErrStoreNotFound
	}
	return d, nil
}

func (s *journeyDomainStore) Insert(_ context.Context, d management.Domain) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[d.ID] = d
	return d, nil
}

func (s *journeyDomainStore) MarkVerified(_ context.Context, id uuid.UUID, expectedToken string, at time.Time, withDNSSEC bool, logID *uuid.UUID) (management.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *journeyDomainStore) SetPaused(_ context.Context, id uuid.UUID, pausedAt *time.Time) (management.Domain, error) {
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

func (s *journeyDomainStore) SoftDelete(_ context.Context, id uuid.UUID, at time.Time) (management.Domain, error) {
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

func (s *journeyDomainStore) RotateToken(_ context.Context, id uuid.UUID, newToken string, issuedAt time.Time) (management.Domain, error) {
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

// journeySlugStore is the in-memory slugreservation.Store. Insert
// returns slugreservation.ErrSlugReserved when an active reservation
// already exists for the slug — same partial-unique-index semantics
// the Postgres adapter exposes. Active filters by expiry against the
// injected clock so the production "reservation expires after 12
// months" behaviour is observable in the test.
type journeySlugStore struct {
	mu    sync.Mutex
	rows  map[string]slugreservation.Reservation
	clock journeyClock
}

func newJourneySlugStore(clock journeyClock) *journeySlugStore {
	return &journeySlugStore{
		rows:  map[string]slugreservation.Reservation{},
		clock: clock,
	}
}

func (s *journeySlugStore) Active(_ context.Context, slug string) (slugreservation.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.rows[slug]
	if !ok {
		return slugreservation.Reservation{}, slugreservation.ErrNotReserved
	}
	if !res.ExpiresAt.After(s.clock.Now()) {
		return slugreservation.Reservation{}, slugreservation.ErrNotReserved
	}
	return res, nil
}

func (s *journeySlugStore) Insert(_ context.Context, slug string, byTenant uuid.UUID, releasedAt, expiresAt time.Time) (slugreservation.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.rows[slug]; ok && existing.ExpiresAt.After(s.clock.Now()) {
		return slugreservation.Reservation{}, slugreservation.ErrSlugReserved
	}
	res := slugreservation.Reservation{
		ID:                 uuid.New(),
		Slug:               slug,
		ReleasedAt:         releasedAt,
		ReleasedByTenantID: byTenant,
		ExpiresAt:          expiresAt,
		CreatedAt:          releasedAt,
	}
	s.rows[slug] = res
	return res, nil
}

func (s *journeySlugStore) SoftDelete(_ context.Context, _ string, _ time.Time) (slugreservation.Reservation, error) {
	return slugreservation.Reservation{}, slugreservation.ErrNotReserved
}

type journeyRedirectStore struct{}

func (journeyRedirectStore) Active(_ context.Context, _ string) (slugreservation.Redirect, error) {
	return slugreservation.Redirect{}, slugreservation.ErrNotReserved
}

func (journeyRedirectStore) Upsert(_ context.Context, _, _ string, _ time.Time) (slugreservation.Redirect, error) {
	return slugreservation.Redirect{}, nil
}

type journeyAudit struct{}

func (journeyAudit) LogMasterOverride(_ context.Context, _ slugreservation.MasterOverrideEvent) error {
	return nil
}

type journeySlack struct{}

func (journeySlack) NotifyAlert(_ context.Context, _ string) error { return nil }

type journeyClock struct{ now time.Time }

func (j journeyClock) Now() time.Time { return j.now }

// journeyAlwaysAllowGate skips the per-tenant quota and circuit-breaker
// path. Quota behaviour has its own per-handler tests; the chain only
// asserts the state-transition contract.
type journeyAlwaysAllowGate struct{}

func (journeyAlwaysAllowGate) Allow(_ context.Context, _ uuid.UUID) management.EnrollmentDecision {
	return management.EnrollmentDecision{Allowed: true}
}

// journeyDNS is the management.DNSChecker stub. It always succeeds —
// the use-case reads the verification token from the persisted Domain
// row, so a no-op stub is sufficient to flip status to "verified".
type journeyDNS struct{}

func (journeyDNS) Check(_ context.Context, _, _ string) (management.DNSCheckResult, error) {
	return management.DNSCheckResult{}, nil
}

// journeySlugValidator is the management.HostValidator that consults
// the SIN-62244 slugreservation.Service before allowing enrollment.
// The wire-up in cmd/server today does not yet plumb this gate into
// the validator; the chained test uses the real Service so the F46
// lock is exercised against production-equivalent code paths.
type journeySlugValidator struct {
	svc *slugreservation.Service
}

func (j journeySlugValidator) Validate(ctx context.Context, host string) error {
	slug := journeyFirstLabel(host)
	err := j.svc.CheckAvailable(ctx, slug)
	if err == nil {
		return nil
	}
	if errors.Is(err, slugreservation.ErrSlugReserved) {
		return fmt.Errorf("%w: %w", management.ErrSlugReserved, err)
	}
	return err
}

// journeySlugReleaser delegates to slugreservation.Service.ReleaseSlug
// and trims the host to its first label, matching the
// slugReleaseAdapter shim in cmd/server/customdomain_wire.go.
type journeySlugReleaser struct {
	svc *slugreservation.Service
}

func (j journeySlugReleaser) ReleaseSlug(ctx context.Context, host string, byTenantID uuid.UUID) error {
	slug := journeyFirstLabel(host)
	if _, err := j.svc.ReleaseSlug(ctx, slug, byTenantID); err != nil {
		return err
	}
	return nil
}

func journeyFirstLabel(host string) string {
	for i := 0; i < len(host); i++ {
		if host[i] == '.' {
			return host[:i]
		}
	}
	return host
}

// journeyFormPost performs the CSRF-prime + form-POST ritual the
// existing per-handler helpers use. Duplicated here so the chained
// test stays self-contained and the helper is parameterised on the
// tenant id (handler_test.go's helpers hard-wire testTenant).
func journeyFormPost(t *testing.T, mux http.Handler, tenant uuid.UUID, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	primer := httptest.NewRecorder()
	primerReq := httptest.NewRequest(http.MethodGet, "/tenant/custom-domains/new", nil)
	primerReq = primerReq.WithContext(cd.WithTenantID(primerReq.Context(), tenant))
	mux.ServeHTTP(primer, primerReq)
	cookies := primer.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("primer did not set CSRF cookie")
	}
	token := cookies[0].Value

	form := url.Values{}
	for _, kv := range strings.Split(body, "&") {
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		key := parts[0]
		val := ""
		if len(parts) == 2 {
			val = parts[1]
		}
		form.Add(key, val)
	}
	form.Set("_csrf", token)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookies[0])
	req = req.WithContext(cd.WithTenantID(req.Context(), tenant))
	mux.ServeHTTP(rec, req)
	return rec
}

// journeyHTMXRequest is the HTMX-style state-changing request helper
// (POST/PATCH/DELETE with the X-CSRF-Token header). Mirrors
// htmxRequest in handler_test.go but accepts a tenant parameter.
func journeyHTMXRequest(t *testing.T, mux http.Handler, tenant uuid.UUID, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	primer := httptest.NewRecorder()
	primerReq := httptest.NewRequest(http.MethodGet, "/tenant/custom-domains", nil)
	primerReq = primerReq.WithContext(cd.WithTenantID(primerReq.Context(), tenant))
	mux.ServeHTTP(primer, primerReq)
	cookies := primer.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("primer did not set CSRF cookie")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(cd.CSRFHeader, cookies[0].Value)
	req.AddCookie(cookies[0])
	req = req.WithContext(cd.WithTenantID(req.Context(), tenant))
	mux.ServeHTTP(rec, req)
	return rec
}
