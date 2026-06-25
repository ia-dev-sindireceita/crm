package master_test

// SIN-63987 / [CRM][QUALITY]: coverage for the impersonation SSE feed
// (Feed / streamFeed / writeSSEEvent) plus the still-uncovered Start /
// End / safeRedirect / readMasterSessionID branches. These were at 0% /
// partial coverage, dragging internal/web/master below the >85% bar.
//
// The SSE handlers are exercised via httptest.NewRecorder (which
// implements http.Flusher) and a purpose-built fake repo that lets the
// stream terminate deterministically by flipping the envelope to
// "ended" after the entry check.

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

// ----- feed-specific fake ---------------------------------------------------

// fakeFeedRepo drives the Feed/streamFeed paths. ActiveForSession can be
// configured to return active for the first N calls then flip to
// ErrNoActiveImpersonation, which is how the streaming loop terminates
// without relying on context-cancellation races.
type fakeFeedRepo struct {
	mu sync.Mutex

	active    *impersonation.Session
	activeErr error // non-nil → every ActiveForSession returns this

	// activeErrAfter: once ActiveForSession has been called more than
	// this many times, return ErrNoActiveImpersonation. 0 disables.
	activeErrAfter int
	activeCalls    int

	rows      []audit.SecurityRow
	listErr   error
	listCalls int

	// pollRows, when non-nil, is returned for every ListAuditByCorrelation
	// call after the first (backfill) one — used to drive the in-loop
	// "new rows arrived" (progressed) branch of streamFeed.
	pollRows []audit.SecurityRow
	// listErrAfter: once ListAuditByCorrelation has been called more than
	// this many times, return an error. 0 disables.
	listErrAfter int
}

func (f *fakeFeedRepo) Start(context.Context, impersonation.StartInput) (*impersonation.Session, error) {
	return nil, errors.New("fakeFeedRepo: Start not used")
}

func (f *fakeFeedRepo) End(context.Context, uuid.UUID, uuid.UUID, string, time.Time) error {
	return errors.New("fakeFeedRepo: End not used")
}

func (f *fakeFeedRepo) ActiveForSession(_ context.Context, _ uuid.UUID) (*impersonation.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeCalls++
	if f.activeErr != nil {
		return nil, f.activeErr
	}
	if f.activeErrAfter > 0 && f.activeCalls > f.activeErrAfter {
		return nil, impersonation.ErrNoActiveImpersonation
	}
	if f.active == nil {
		return nil, impersonation.ErrNoActiveImpersonation
	}
	return f.active, nil
}

func (f *fakeFeedRepo) ListAuditByCorrelation(_ context.Context, _ uuid.UUID, _ int) ([]audit.SecurityRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listErrAfter > 0 && f.listCalls > f.listErrAfter {
		return nil, errors.New("audit tail vanished mid-stream (test)")
	}
	if f.listCalls > 1 && f.pollRows != nil {
		return f.pollRows, nil
	}
	return f.rows, nil
}

// nonFlushWriter wraps a recorder but deliberately does NOT expose
// Flush, so the handler's http.Flusher type assertion fails.
type nonFlushWriter struct{ rec *httptest.ResponseRecorder }

func (n nonFlushWriter) Header() http.Header         { return n.rec.Header() }
func (n nonFlushWriter) Write(b []byte) (int, error) { return n.rec.Write(b) }
func (n nonFlushWriter) WriteHeader(code int)        { n.rec.WriteHeader(code) }

// ----- feed helpers ---------------------------------------------------------

func feedHandler(t *testing.T, repo impersonation.Repo, poll, heartbeat time.Duration) *master.ImpersonationHandler {
	t.Helper()
	h, err := master.NewImpersonationHandler(master.ImpersonationDeps{
		Sessions:         repo,
		Auditor:          &fakeAudit{},
		Tenants:          defaultResolver(),
		Logger:           discardLogger(),
		Clock:            func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
		FeedPollInterval: poll,
		FeedHeartbeat:    heartbeat,
	})
	if err != nil {
		t.Fatalf("NewImpersonationHandler: %v", err)
	}
	return h
}

func feedRequest(t *testing.T, withCookie bool, roles ...iam.Role) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/master/impersonation/feed", nil)
	if withCookie {
		r.AddCookie(masterSessionCookie())
	}
	p := iam.Principal{UserID: testMasterUserID, Roles: roles}
	return r.WithContext(iam.WithPrincipal(r.Context(), p))
}

func ownedEnvelope() *impersonation.Session {
	return &impersonation.Session{
		ID:              uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd"),
		MasterUserID:    testMasterUserID,
		MasterSessionID: testMasterSessionID,
		TargetTenantID:  testTargetTenantID,
		StartedAt:       time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		ExpiresAt:       time.Date(2026, 6, 1, 12, 15, 0, 0, time.UTC),
	}
}

// ----- Feed: guard branches -------------------------------------------------

func TestImpersonationHandler_Feed_PrincipalMissing(t *testing.T) {
	t.Parallel()
	h := feedHandler(t, &fakeFeedRepo{}, time.Millisecond, time.Hour)
	r := httptest.NewRequest(http.MethodGet, "/master/impersonation/feed", nil)
	rec := httptest.NewRecorder()
	h.Feed(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestImpersonationHandler_Feed_NotMaster_403(t *testing.T) {
	t.Parallel()
	h := feedHandler(t, &fakeFeedRepo{}, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	h.Feed(rec, feedRequest(t, true, iam.RoleTenantGerente))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}

func TestImpersonationHandler_Feed_NoMasterSession_503(t *testing.T) {
	t.Parallel()
	h := feedHandler(t, &fakeFeedRepo{}, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	// Master principal but no master cookie on the request.
	h.Feed(rec, feedRequest(t, false, iam.RoleMaster))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestImpersonationHandler_Feed_NoActiveEnvelope_204(t *testing.T) {
	t.Parallel()
	h := feedHandler(t, &fakeFeedRepo{active: nil}, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	h.Feed(rec, feedRequest(t, true, iam.RoleMaster))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", rec.Code)
	}
}

func TestImpersonationHandler_Feed_LookupError_503(t *testing.T) {
	t.Parallel()
	repo := &fakeFeedRepo{activeErr: errors.New("db down (test)")}
	h := feedHandler(t, repo, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	h.Feed(rec, feedRequest(t, true, iam.RoleMaster))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestImpersonationHandler_Feed_OwnerMismatch_403(t *testing.T) {
	t.Parallel()
	env := ownedEnvelope()
	env.MasterUserID = uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee") // someone else
	h := feedHandler(t, &fakeFeedRepo{active: env}, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	h.Feed(rec, feedRequest(t, true, iam.RoleMaster))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 (only the opener may watch the feed)", rec.Code)
	}
}

func TestImpersonationHandler_Feed_StreamingUnsupported_500(t *testing.T) {
	t.Parallel()
	h := feedHandler(t, &fakeFeedRepo{active: ownedEnvelope()}, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	w := nonFlushWriter{rec: rec}
	h.Feed(w, feedRequest(t, true, iam.RoleMaster))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (writer is not an http.Flusher)", rec.Code)
	}
}

// ----- Feed: streaming success + writeSSEEvent ------------------------------

func TestImpersonationHandler_Feed_StreamsBackfillThenEnds(t *testing.T) {
	t.Parallel()
	tenantID := testTargetTenantID
	corr := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	rows := []audit.SecurityRow{
		{
			ID:            uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			TenantID:      &tenantID,
			ActorUserID:   testMasterUserID,
			Event:         audit.SecurityEventImpersonationStart,
			CorrelationID: &corr,
			Target:        map[string]any{"k": "v"},
			OccurredAt:    time.Date(2026, 6, 1, 11, 0, 1, 0, time.UTC),
		},
		{
			// No TenantID / CorrelationID → exercises the nil branches
			// in writeSSEEvent.
			ID:          uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			ActorUserID: testMasterUserID,
			Event:       audit.SecurityEventImpersonationStop,
			OccurredAt:  time.Date(2026, 6, 1, 11, 0, 2, 0, time.UTC),
		},
	}
	repo := &fakeFeedRepo{
		active:         ownedEnvelope(),
		activeErrAfter: 1, // entry check active; first in-loop poll ends it
		rows:           rows,
	}
	// Both tickers tiny so the loop fires (covering heartbeat + poll
	// branches) and terminates on the first poll's ActiveForSession.
	h := feedHandler(t, repo, time.Millisecond, time.Millisecond)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.Feed(rec, feedRequest(t, true, iam.RoleMaster))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Feed did not terminate after envelope ended")
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type=%q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: audit") {
		t.Errorf("body missing SSE audit event; got=%q", body)
	}
	if !strings.Contains(body, "11111111-1111-1111-1111-111111111111") {
		t.Errorf("body missing first backfill row id; got=%q", body)
	}
	if !strings.Contains(body, `"correlation_id"`) {
		t.Errorf("body missing correlation_id for row with CorrelationID set; got=%q", body)
	}
	if !strings.Contains(body, `"tenant_id"`) {
		t.Errorf("body missing tenant_id for row with TenantID set; got=%q", body)
	}
}

func TestImpersonationHandler_Feed_BackfillListError_Returns(t *testing.T) {
	t.Parallel()
	repo := &fakeFeedRepo{
		active:  ownedEnvelope(),
		listErr: errors.New("audit tail unavailable (test)"),
	}
	h := feedHandler(t, repo, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.Feed(rec, feedRequest(t, true, iam.RoleMaster))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Feed did not return after backfill list error")
	}
	// Stream header (200) was already written before the backfill error.
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (header flushed before backfill error)", rec.Code)
	}
}

// TestImpersonationHandler_Feed_PollDeliversNewRows covers the in-loop
// "progressed" branch: a fresh audit row arrives on the first poll tick
// and is streamed before the envelope ends.
func TestImpersonationHandler_Feed_PollDeliversNewRows(t *testing.T) {
	t.Parallel()
	backfill := []audit.SecurityRow{{
		ID:          uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		ActorUserID: testMasterUserID,
		Event:       audit.SecurityEventImpersonationStart,
		OccurredAt:  time.Date(2026, 6, 1, 11, 0, 1, 0, time.UTC),
	}}
	poll := []audit.SecurityRow{
		backfill[0], // dup — must be skipped
		{
			ID:          uuid.MustParse("44444444-4444-4444-4444-444444444444"),
			ActorUserID: testMasterUserID,
			Event:       audit.SecurityEventImpersonationStop,
			OccurredAt:  time.Date(2026, 6, 1, 11, 0, 3, 0, time.UTC),
		},
	}
	repo := &fakeFeedRepo{
		active:         ownedEnvelope(),
		activeErrAfter: 2, // entry + first in-loop check active; ends on 2nd in-loop
		rows:           backfill,
		pollRows:       poll,
	}
	h := feedHandler(t, repo, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.Feed(rec, feedRequest(t, true, iam.RoleMaster))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Feed did not terminate")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "44444444-4444-4444-4444-444444444444") {
		t.Errorf("body missing the poll-delivered row; got=%q", body)
	}
}

// TestImpersonationHandler_Feed_PollListError_Returns covers the in-loop
// poll-time ListAuditByCorrelation error branch (stream returns).
func TestImpersonationHandler_Feed_PollListError_Returns(t *testing.T) {
	t.Parallel()
	repo := &fakeFeedRepo{
		active:       ownedEnvelope(),
		rows:         nil, // empty backfill
		listErrAfter: 1,   // backfill OK, first poll list errors → return
	}
	h := feedHandler(t, repo, time.Millisecond, time.Hour)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.Feed(rec, feedRequest(t, true, iam.RoleMaster))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Feed did not return after poll list error")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (header flushed before poll error)", rec.Code)
	}
}

// ----- HandlerFunc accessors ------------------------------------------------

func TestImpersonationHandler_Accessors(t *testing.T) {
	t.Parallel()
	h := feedHandler(t, &fakeFeedRepo{active: nil}, time.Millisecond, time.Hour)
	if h.StartHandler() == nil || h.EndHandler() == nil || h.FeedHandler() == nil {
		t.Fatal("a HandlerFunc accessor returned nil")
	}
	// FeedHandler should route to Feed → 204 when no active envelope.
	rec := httptest.NewRecorder()
	h.FeedHandler().ServeHTTP(rec, feedRequest(t, true, iam.RoleMaster))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("FeedHandler status=%d, want 204", rec.Code)
	}
}

// ----- Start: remaining branches --------------------------------------------

func TestImpersonationHandler_Start_PrincipalMissing_500(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	r := httptest.NewRequest(http.MethodPost, "/master/tenants/"+testTargetTenantID.String()+"/impersonate", nil)
	r.SetPathValue("id", testTargetTenantID.String())
	rec := httptest.NewRecorder()
	h.Start(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestImpersonationHandler_Start_InvalidTenantID_400(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	r := httptest.NewRequest(http.MethodPost, "/master/tenants/not-a-uuid/impersonate", nil)
	r.AddCookie(masterSessionCookie())
	r.SetPathValue("id", "not-a-uuid")
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	rec := httptest.NewRecorder()
	h.Start(rec, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestImpersonationHandler_Start_NoMasterSession_503(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	r := httptest.NewRequest(http.MethodPost, "/master/tenants/"+testTargetTenantID.String()+"/impersonate", nil)
	r.SetPathValue("id", testTargetTenantID.String())
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	rec := httptest.NewRecorder()
	h.Start(rec, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 (no master cookie)", rec.Code)
	}
}

func TestImpersonationHandler_Start_InvalidMasterCookie_503(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	r := httptest.NewRequest(http.MethodPost, "/master/tenants/"+testTargetTenantID.String()+"/impersonate", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-sess-master", Value: "not-a-uuid"})
	r.SetPathValue("id", testTargetTenantID.String())
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	rec := httptest.NewRecorder()
	h.Start(rec, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 (unparseable master cookie)", rec.Code)
	}
}

func TestImpersonationHandler_Start_ReasonTooShort_422(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	rec := httptest.NewRecorder()
	h.Start(rec, impersonateRequest(t, testTargetTenantID, "short"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

func TestImpersonationHandler_Start_TenantNotFound_404(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	unknown := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	rec := httptest.NewRecorder()
	h.Start(rec, impersonateRequest(t, unknown, "valid reason for unknown tenant"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

// errResolver returns a non-ErrTenantNotFound error to drive the 503
// branch of the tenant resolve.
type errResolver struct{ err error }

func (e errResolver) ResolveByID(context.Context, uuid.UUID) (*tenancy.Tenant, error) {
	return nil, e.err
}

func TestImpersonationHandler_Start_TenantResolveError_503(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, errResolver{err: errors.New("resolve down (test)")})
	rec := httptest.NewRecorder()
	h.Start(rec, impersonateRequest(t, testTargetTenantID, "valid reason but resolver errors"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestImpersonationHandler_Start_InvalidReasonSentinel_422(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	repo.startErr = impersonation.ErrInvalidReason
	h := impersonationHandler(t, repo, &fakeAudit{}, defaultResolver())
	rec := httptest.NewRecorder()
	h.Start(rec, impersonateRequest(t, testTargetTenantID, "passes boundary, trips CHECK"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422 (defense in depth)", rec.Code)
	}
}

func TestImpersonationHandler_Start_RepoError_500(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	repo.startErr = errors.New("insert failed (test)")
	h := impersonationHandler(t, repo, &fakeAudit{}, defaultResolver())
	rec := httptest.NewRecorder()
	h.Start(rec, impersonateRequest(t, testTargetTenantID, "valid reason but insert errors"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// ----- Start: safeRedirect via Referer --------------------------------------

func startWithReferer(t *testing.T, h *master.ImpersonationHandler, referer string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"reason": {"valid reason for referer test"}}
	r := httptest.NewRequest(http.MethodPost,
		"/master/tenants/"+testTargetTenantID.String()+"/impersonate",
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if referer != "" {
		r.Header.Set("Referer", referer)
	}
	r.AddCookie(masterSessionCookie())
	r.SetPathValue("id", testTargetTenantID.String())
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	rec := httptest.NewRecorder()
	h.Start(rec, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	return rec
}

func TestImpersonationHandler_Start_SafeRedirect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		referer  string
		wantDest string
	}{
		{"empty referer falls back", "", "/master/tenants"},
		{"in-console relative referer honoured", "/master/tenants/abc", "/master/tenants/abc"},
		{"non-master path falls back", "/other/page", "/master/tenants"},
		{"absolute url falls back", "http://evil.example/master/x", "/master/tenants"},
		{"scheme-relative falls back", "//evil.example/master/x", "/master/tenants"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
			rec := startWithReferer(t, h, tc.referer)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status=%d, want 303; body=%q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Location"); got != tc.wantDest {
				t.Errorf("Location=%q, want %q", got, tc.wantDest)
			}
		})
	}
}

// ----- End: remaining branches ----------------------------------------------

func TestImpersonationHandler_End_PrincipalMissing_500(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	r := httptest.NewRequest(http.MethodPost, "/master/impersonation/end", nil)
	rec := httptest.NewRecorder()
	h.End(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestImpersonationHandler_End_NoMasterSession_503(t *testing.T) {
	t.Parallel()
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	r := httptest.NewRequest(http.MethodPost, "/master/impersonation/end", nil)
	p := iam.Principal{UserID: testMasterUserID, Roles: []iam.Role{iam.RoleMaster}}
	rec := httptest.NewRecorder()
	h.End(rec, r.WithContext(iam.WithPrincipal(r.Context(), p)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestImpersonationHandler_End_NoActiveEnvelope_Idempotent303(t *testing.T) {
	t.Parallel()
	// Fresh repo: ActiveForSession returns ErrNoActiveImpersonation.
	h := impersonationHandler(t, newFakeImpersonationRepo(), &fakeAudit{}, defaultResolver())
	rec := httptest.NewRecorder()
	h.End(rec, endRequest(t))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303 (idempotent no-op)", rec.Code)
	}
}

// endLookupErrRepo wraps the standard fake but forces ActiveForSession to
// return a generic (non-sentinel) error so End hits its 503 branch.
type endLookupErrRepo struct {
	*fakeImpersonationRepo
	lookupErr error
}

func (r endLookupErrRepo) ActiveForSession(context.Context, uuid.UUID) (*impersonation.Session, error) {
	return nil, r.lookupErr
}

func TestImpersonationHandler_End_LookupError_503(t *testing.T) {
	t.Parallel()
	repo := endLookupErrRepo{
		fakeImpersonationRepo: newFakeImpersonationRepo(),
		lookupErr:             errors.New("lookup down (test)"),
	}
	h := impersonationHandler(t, repo, &fakeAudit{}, defaultResolver())
	rec := httptest.NewRecorder()
	h.End(rec, endRequest(t))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", rec.Code)
	}
}

func TestImpersonationHandler_End_EndError_500(t *testing.T) {
	t.Parallel()
	repo := newFakeImpersonationRepo()
	aud := &fakeAudit{}
	h := impersonationHandler(t, repo, aud, defaultResolver())

	// Open an envelope so ActiveForSession finds one.
	startRec := httptest.NewRecorder()
	h.Start(startRec, impersonateRequest(t, testTargetTenantID, "setup for end-error test"))
	if startRec.Code != http.StatusSeeOther {
		t.Fatalf("start: status=%d", startRec.Code)
	}
	// Now trip a generic (non-sentinel) End error.
	repo.endErr = errors.New("update failed (test)")
	endRec := httptest.NewRecorder()
	h.End(endRec, endRequest(t))
	if endRec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", endRec.Code)
	}
}
