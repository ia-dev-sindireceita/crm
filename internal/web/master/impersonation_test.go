package master_test

// SIN-63958 / master-impersonation-spec §5.5 — handler tests.
//
// Items covered (spec numbering):
//   1  impersonation_start audit row written on successful Start.
//   2  impersonation_stop  audit row written on successful End.
//   5  audit write failure on Start → 500 + no master_impersonation_session
//      row + tenancy not swapped.
//   6  (Fase C) t.Skip — banner XSS / template escape deferred.
//   7  (Fase C) t.Skip — banner partial table-driven test deferred.
//   9  expires_at field in body is ignored by Start.
//  10  concurrent /start from same master session → second returns 409.
//
// Items 3 (middleware expiry audit row) and 4 (correlation_id propagation
// through middleware) live in the adjacent middleware_test package
// (internal/adapter/httpapi/middleware/impersonation_session_test.go).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/master"
)

// ----- fakes ----------------------------------------------------------------

type fakeImpersonationRepo struct {
	mu      sync.Mutex
	rows    map[uuid.UUID]*impersonation.Session
	byMSess map[uuid.UUID]*impersonation.Session

	startErr error
	endErr   error

	endActors  []uuid.UUID // actor uuids passed to End (must be master_user_id, NOT session id)
	endReasons []string    // reasons passed to End, parallel to endActors
}

func newFakeImpersonationRepo() *fakeImpersonationRepo {
	return &fakeImpersonationRepo{
		rows:    map[uuid.UUID]*impersonation.Session{},
		byMSess: map[uuid.UUID]*impersonation.Session{},
	}
}

func (f *fakeImpersonationRepo) Start(_ context.Context, in impersonation.StartInput) (*impersonation.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	if _, exists := f.byMSess[in.MasterSessionID]; exists {
		return nil, impersonation.ErrAlreadyActive
	}
	now := in.StartedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	sess := &impersonation.Session{
		ID:              uuid.New(),
		MasterUserID:    in.MasterUserID,
		MasterSessionID: in.MasterSessionID,
		TargetTenantID:  in.TargetTenantID,
		Reason:          in.Reason,
		StartedAt:       now,
		ExpiresAt:       now.Add(impersonation.DefaultEnvelopeTTL),
	}
	f.rows[sess.ID] = sess
	f.byMSess[in.MasterSessionID] = sess
	return sess, nil
}

func (f *fakeImpersonationRepo) ActiveForSession(_ context.Context, masterSessionID uuid.UUID) (*impersonation.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byMSess[masterSessionID]
	if !ok || s.EndedAt != nil {
		return nil, impersonation.ErrNoActiveImpersonation
	}
	return s, nil
}

func (f *fakeImpersonationRepo) End(_ context.Context, id uuid.UUID, actor uuid.UUID, reason string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endErr != nil {
		return f.endErr
	}
	s, ok := f.rows[id]
	if !ok || s.EndedAt != nil {
		return impersonation.ErrNoActiveImpersonation
	}
	f.endActors = append(f.endActors, actor)
	f.endReasons = append(f.endReasons, reason)
	s.EndedAt = &at
	s.EndedReason = reason
	delete(f.byMSess, s.MasterSessionID)
	return nil
}

func (f *fakeImpersonationRepo) ListAuditByCorrelation(_ context.Context, _ uuid.UUID, _ int) ([]audit.SecurityRow, error) {
	return nil, nil
}

func (f *fakeImpersonationRepo) activeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byMSess)
}

type fakeAudit struct {
	mu     sync.Mutex
	events []audit.SecurityAuditEvent
	failOn audit.SecurityEvent
}

func (a *fakeAudit) WriteSecurity(_ context.Context, e audit.SecurityAuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.Event == a.failOn {
		return errors.New("audit: write failed (test)")
	}
	a.events = append(a.events, e)
	return nil
}

func (a *fakeAudit) WriteData(_ context.Context, _ audit.DataAuditEvent) error {
	panic("impersonation handler must not write DataAuditEvent")
}

func (a *fakeAudit) snapshot() []audit.SecurityAuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]audit.SecurityAuditEvent, len(a.events))
	copy(out, a.events)
	return out
}

type fakeTenantResolver struct {
	tenants map[uuid.UUID]*tenancy.Tenant
}

func (f *fakeTenantResolver) ResolveByID(_ context.Context, id uuid.UUID) (*tenancy.Tenant, error) {
	t, ok := f.tenants[id]
	if !ok {
		return nil, tenancy.ErrTenantNotFound
	}
	return t, nil
}

// ----- helpers --------------------------------------------------------------

var (
	testMasterUserID    = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	testMasterSessionID = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	testTargetTenantID  = uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
)

func impersonationHandler(t *testing.T, repo impersonation.Repo, aud audit.SplitLogger, resolver tenancy.ByIDResolver) *master.ImpersonationHandler {
	t.Helper()
	h, err := master.NewImpersonationHandler(master.ImpersonationDeps{
		Sessions: repo,
		Auditor:  aud,
		Tenants:  resolver,
		Logger:   discardLogger(),
		Clock:    func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewImpersonationHandler: %v", err)
	}
	return h
}

func masterSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:  "__Host-sess-master",
		Value: testMasterSessionID.String(),
	}
}

func impersonateRequest(t *testing.T, tenantID uuid.UUID, reason string) *http.Request {
	t.Helper()
	form := url.Values{"reason": {reason}}
	r := httptest.NewRequest(http.MethodPost,
		"/master/tenants/"+tenantID.String()+"/impersonate",
		strings.NewReader(form.Encode()),
	)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(masterSessionCookie())
	r.SetPathValue("id", tenantID.String())
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	return r.WithContext(iam.WithPrincipal(r.Context(), p))
}

func endRequest(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/master/impersonation/end", nil)
	r.AddCookie(masterSessionCookie())
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	return r.WithContext(iam.WithPrincipal(r.Context(), p))
}

func defaultResolver() *fakeTenantResolver {
	return &fakeTenantResolver{
		tenants: map[uuid.UUID]*tenancy.Tenant{
			testTargetTenantID: {
				ID:   testTargetTenantID,
				Name: "acme",
				Host: "acme.crm.local",
			},
		},
	}
}

// ----- tests ----------------------------------------------------------------

// Spec §5.5 #1: impersonation_start audit row written on successful Start.
func TestImpersonationHandler_Start_AuditRowWritten(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	aud := &fakeAudit{}
	h := impersonationHandler(t, repo, aud, defaultResolver())

	rec := httptest.NewRecorder()
	h.Start(rec, impersonateRequest(t, testTargetTenantID, "incident response for tenant"))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	events := aud.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events=%d, want 1", len(events))
	}
	if events[0].Event != audit.SecurityEventImpersonationStart {
		t.Errorf("event=%q, want impersonation_start", events[0].Event)
	}
	if events[0].CorrelationID == nil {
		t.Error("CorrelationID is nil, want non-nil session id")
	}
}

// Spec §5.5 #2: impersonation_stop audit row written on successful End.
func TestImpersonationHandler_End_AuditRowWritten(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	aud := &fakeAudit{}
	h := impersonationHandler(t, repo, aud, defaultResolver())

	// Start an envelope first.
	startRec := httptest.NewRecorder()
	h.Start(startRec, impersonateRequest(t, testTargetTenantID, "setup for end test"))
	if startRec.Code != http.StatusSeeOther {
		t.Fatalf("start: status=%d", startRec.Code)
	}

	endRec := httptest.NewRecorder()
	h.End(endRec, endRequest(t))

	if endRec.Code != http.StatusSeeOther {
		t.Fatalf("end: status=%d, want 303; body=%q", endRec.Code, endRec.Body.String())
	}
	events := aud.snapshot()
	var stopEvents []audit.SecurityAuditEvent
	for _, e := range events {
		if e.Event == audit.SecurityEventImpersonationStop {
			stopEvents = append(stopEvents, e)
		}
	}
	if len(stopEvents) != 1 {
		t.Fatalf("impersonation_stop events=%d, want 1", len(stopEvents))
	}

	// Regression for CTO PR #284 finding: End MUST be called with the
	// master_user_id as actor, NOT the impersonation row id. The
	// postgres adapter threads actor into postgres.WithMasterOps so
	// master_ops_audit.actor_user_id records the human, not the row.
	if len(repo.endActors) == 0 {
		t.Fatal("repo.End actor not recorded")
	}
	if repo.endActors[0] != testMasterUserID {
		t.Errorf("end actor=%v, want master_user_id=%v (NOT session id)", repo.endActors[0], testMasterUserID)
	}
}

// Spec §5.5 #5: audit write failure on Start → 500 + no session row.
func TestImpersonationHandler_Start_AuditFailureRollsBack(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	aud := &fakeAudit{failOn: audit.SecurityEventImpersonationStart}
	h := impersonationHandler(t, repo, aud, defaultResolver())

	rec := httptest.NewRecorder()
	h.Start(rec, impersonateRequest(t, testTargetTenantID, "this will fail on audit"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	if repo.activeCount() != 0 {
		t.Errorf("active sessions=%d, want 0 (envelope must be rolled back)", repo.activeCount())
	}
}

// Spec §5.5 #6: Fase C — banner XSS test (template escape is implicit in html/template).
func TestImpersonationHandler_Start_BannerXSS_FaseC(t *testing.T) {
	t.Skip("Fase C — SIN-63947: banner template not yet wired")
}

// Spec §5.5 #7: Fase C — banner partial table-driven test deferred.
func TestImpersonationHandler_BannerPartial_FaseC(t *testing.T) {
	t.Skip("Fase C — SIN-63947: banner partial not yet wired")
}

// Spec §5.5 #9: expires_at body field is ignored by Start.
func TestImpersonationHandler_Start_IgnoresExpiresAtBody(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	aud := &fakeAudit{}
	h := impersonationHandler(t, repo, aud, defaultResolver())

	// Include a hostile expires_at in the body — the handler must ignore it.
	form := url.Values{
		"reason":     {"legitimate reason text"},
		"expires_at": {"2000-01-01T00:00:00Z"},
	}
	r := httptest.NewRequest(http.MethodPost,
		"/master/tenants/"+testTargetTenantID.String()+"/impersonate",
		strings.NewReader(form.Encode()),
	)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(masterSessionCookie())
	r.SetPathValue("id", testTargetTenantID.String())
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	r = r.WithContext(iam.WithPrincipal(r.Context(), p))

	rec := httptest.NewRecorder()
	h.Start(rec, r)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	// Verify the stored session has a future expires_at, not the hostile past value.
	events := aud.snapshot()
	if len(events) == 0 {
		t.Fatal("no audit events, want impersonation_start")
	}
	expiresRaw, ok := events[0].Target["expires_at"].(string)
	if !ok {
		t.Fatal("audit target missing expires_at string")
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresRaw)
	if err != nil {
		t.Fatalf("parse expires_at: %v", err)
	}
	if !expires.After(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("expires_at=%v is not in the future — hostile value may have been accepted", expires)
	}
}

// Spec §5.5 #10: concurrent /start from same master session → second returns 409.
func TestImpersonationHandler_Start_Concurrent409(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	aud := &fakeAudit{}
	h := impersonationHandler(t, repo, aud, defaultResolver())

	// First start succeeds.
	rec1 := httptest.NewRecorder()
	h.Start(rec1, impersonateRequest(t, testTargetTenantID, "first envelope for this session"))
	if rec1.Code != http.StatusSeeOther {
		t.Fatalf("first start: status=%d, want 303; body=%q", rec1.Code, rec1.Body.String())
	}

	// Second start from the same master session must 409.
	rec2 := httptest.NewRecorder()
	h.Start(rec2, impersonateRequest(t, testTargetTenantID, "second envelope same session"))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second start: status=%d, want 409; body=%q", rec2.Code, rec2.Body.String())
	}
}

// Regression for CTO PR #284 nit: End MUST NOT emit a duplicate
// impersonation_stop audit row when the envelope was ended between our
// lookup and our End — the racing branch (expiry middleware or a
// concurrent /end) already wrote its own stop row, so a second one here
// would double-count.
func TestImpersonationHandler_End_RaceWithExpire_NoDuplicateAudit(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	aud := &fakeAudit{}
	h := impersonationHandler(t, repo, aud, defaultResolver())

	startRec := httptest.NewRecorder()
	h.Start(startRec, impersonateRequest(t, testTargetTenantID, "setup for end-race test"))
	if startRec.Code != http.StatusSeeOther {
		t.Fatalf("start: status=%d", startRec.Code)
	}

	// Simulate the race: ActiveForSession will return the envelope
	// (still in byMSess since startErr/endErr weren't tripped), but
	// the End call will see ErrNoActiveImpersonation as if a
	// concurrent expirer beat us to the UPDATE.
	repo.endErr = impersonation.ErrNoActiveImpersonation

	endRec := httptest.NewRecorder()
	h.End(endRec, endRequest(t))
	if endRec.Code != http.StatusSeeOther {
		t.Fatalf("end: status=%d, want 303 (idempotent); body=%q", endRec.Code, endRec.Body.String())
	}

	stopCount := 0
	for _, e := range aud.snapshot() {
		if e.Event == audit.SecurityEventImpersonationStop {
			stopCount++
		}
	}
	if stopCount != 0 {
		t.Errorf("impersonation_stop events=%d, want 0 (race branch must not duplicate)", stopCount)
	}
}
