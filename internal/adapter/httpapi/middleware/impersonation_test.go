package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type fakeMasterChecker struct {
	masters map[uuid.UUID]bool
	err     error
	calls   int
}

func (f *fakeMasterChecker) IsMaster(_ context.Context, userID uuid.UUID) (bool, error) {
	f.calls++
	if f.err != nil {
		return false, f.err
	}
	return f.masters[userID], nil
}

type fakeByIDResolver struct {
	tenants map[uuid.UUID]*tenancy.Tenant
	err     error
	calls   int
}

func (f *fakeByIDResolver) ResolveByID(_ context.Context, id uuid.UUID) (*tenancy.Tenant, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	t, ok := f.tenants[id]
	if !ok {
		return nil, tenancy.ErrTenantNotFound
	}
	return t, nil
}

// recordingLogger is the in-test fake of audit.SplitLogger. It implements
// the full port (WriteSecurity + WriteData) so it can be passed to
// middleware.Impersonation, but only WriteSecurity is exercised — the
// impersonation middleware is a security-event emitter and never writes
// data events. WriteData panics so a future regression that mistakenly
// routes an impersonation row into the data ledger fails loudly in test.
type recordingLogger struct {
	mu     sync.Mutex
	events []audit.SecurityAuditEvent
	// failOn returns an error for the first matching event. Other
	// calls succeed. Use to model "start write fails" without also
	// poisoning the deferred "stop" call.
	failOn func(event audit.SecurityEvent) error
}

func (l *recordingLogger) WriteSecurity(_ context.Context, e audit.SecurityAuditEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failOn != nil {
		if err := l.failOn(e.Event); err != nil {
			return err
		}
	}
	l.events = append(l.events, e)
	return nil
}

// WriteData is intentionally a poison pill: impersonation events are
// security-relevant only, and accidentally routing them to
// audit_log_data would expand the LGPD retention surface. A real test
// failure beats a silent regression.
func (l *recordingLogger) WriteData(_ context.Context, _ audit.DataAuditEvent) error {
	panic("middleware: impersonation must not emit DataAuditEvent")
}

func (l *recordingLogger) snapshot() []audit.SecurityAuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]audit.SecurityAuditEvent, len(l.events))
	copy(out, l.events)
	return out
}

// requestWithSession builds a request whose context carries (a) a
// source tenant (the user's own tenant) and (b) an authenticated
// session for sourceTenantID/userID. Mirrors how Auth+TenantScope
// stack the context in production.
func requestWithSession(t *testing.T, sourceTenantID, userID uuid.UUID, target string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{
		ID:   sourceTenantID,
		Name: "src",
		Host: "src.crm.local",
	})
	ctx = middleware.WithSession(ctx, iam.Session{
		ID:       uuid.New(),
		UserID:   userID,
		TenantID: sourceTenantID,
	})
	return r.WithContext(ctx)
}

// captureTenantHandler stores the tenant on the inbound context for
// later assertion. Returns an "ok" response so the middleware finishes
// cleanly and the deferred audit row is written.
func captureTenantHandler(out **tenancy.Tenant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, _ := tenancy.FromContext(r.Context())
		*out = t
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestImpersonation_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		checker  middleware.MasterChecker
		resolver tenancy.ByIDResolver
		logger   audit.SplitLogger
	}{
		{"nil checker", nil, &fakeByIDResolver{}, &recordingLogger{}},
		{"nil resolver", &fakeMasterChecker{}, nil, &recordingLogger{}},
		{"nil logger", &fakeMasterChecker{}, &fakeByIDResolver{}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for %s", tc.name)
				}
			}()
			middleware.Impersonation(tc.checker, tc.resolver, tc.logger)
		})
	}
}

func TestImpersonation_NoHeaderPassesThrough(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	userID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{userID: true}}
	resolver := &fakeByIDResolver{}
	logger := &recordingLogger{}

	var seen *tenancy.Tenant
	h := middleware.Impersonation(checker, resolver, logger)(captureTenantHandler(&seen))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, userID, "/")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if seen == nil || seen.ID != srcID {
		t.Fatalf("downstream tenant=%v, want source %v", seen, srcID)
	}
	if checker.calls != 0 {
		t.Fatalf("checker calls=%d, want 0 (header not present)", checker.calls)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls=%d, want 0", resolver.calls)
	}
	if got := logger.snapshot(); len(got) != 0 {
		t.Fatalf("audit calls=%d, want 0", len(got))
	}
}

func TestImpersonation_MasterCanRead(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	tgtID := uuid.New()
	masterID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	target := &tenancy.Tenant{ID: tgtID, Name: "target", Host: "target.crm.local"}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{tgtID: target}}
	logger := &recordingLogger{}

	var seen *tenancy.Tenant
	h := middleware.Impersonation(checker, resolver, logger)(captureTenantHandler(&seen))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, masterID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	r.Header.Set(middleware.HeaderImpersonationReason, "incident-INC-7")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if seen == nil || seen.ID != tgtID {
		t.Fatalf("downstream tenant=%v, want target %v", seen, tgtID)
	}
}

func TestImpersonation_NonMasterIgnored(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	tgtID := uuid.New()
	agentID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{}}
	target := &tenancy.Tenant{ID: tgtID, Name: "target", Host: "target.crm.local"}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{tgtID: target}}
	logger := &recordingLogger{}

	var seen *tenancy.Tenant
	h := middleware.Impersonation(checker, resolver, logger)(captureTenantHandler(&seen))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, agentID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	r.Header.Set(middleware.HeaderImpersonationReason, "should-be-ignored")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if seen == nil || seen.ID != srcID {
		t.Fatalf("downstream tenant=%v, want source %v (header MUST be ignored for non-master)", seen, srcID)
	}
	if got := logger.snapshot(); len(got) != 0 {
		t.Fatalf("audit calls=%d, want 0 — non-master must NOT produce an audit row", len(got))
	}
}

func TestImpersonation_RequiresReason(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	tgtID := uuid.New()
	masterID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	target := &tenancy.Tenant{ID: tgtID, Name: "target", Host: "target.crm.local"}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{tgtID: target}}
	logger := &recordingLogger{}

	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream handler must not run when reason header is absent")
	}))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, masterID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	// deliberately NO X-Impersonation-Reason

	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if got := logger.snapshot(); len(got) != 0 {
		t.Fatalf("audit calls=%d, want 0 — missing reason must NOT emit audit", len(got))
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls=%d, want 0 — reason check happens before tenant lookup", resolver.calls)
	}
}

func TestImpersonation_AuditLogPaired(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	tgtID := uuid.New()
	masterID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	target := &tenancy.Tenant{ID: tgtID, Name: "target", Host: "target.crm.local"}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{tgtID: target}}
	logger := &recordingLogger{}

	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, masterID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	r.Header.Set(middleware.HeaderImpersonationReason, "billing-rollup")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	events := logger.snapshot()
	if len(events) != 2 {
		t.Fatalf("audit row count=%d, want 2 (started+ended)", len(events))
	}
	if events[0].Event != audit.SecurityEventImpersonationStart {
		t.Fatalf("events[0]=%q, want %q", events[0].Event, audit.SecurityEventImpersonationStart)
	}
	if events[1].Event != audit.SecurityEventImpersonationStop {
		t.Fatalf("events[1]=%q, want %q", events[1].Event, audit.SecurityEventImpersonationStop)
	}
	for i, ev := range events {
		if ev.ActorUserID != masterID {
			t.Fatalf("events[%d].ActorUserID=%v, want master %v", i, ev.ActorUserID, masterID)
		}
		if ev.TenantID == nil || *ev.TenantID != tgtID {
			t.Fatalf("events[%d].TenantID=%v, want target %v", i, ev.TenantID, tgtID)
		}
		if ev.Target["reason"] != "billing-rollup" {
			t.Fatalf("events[%d].Target.reason=%v, want billing-rollup", i, ev.Target["reason"])
		}
		if ev.Target["tenant_id"] != tgtID.String() {
			t.Fatalf("events[%d].Target.tenant_id=%v, want %v", i, ev.Target["tenant_id"], tgtID.String())
		}
	}
	// Only the "_ended" row carries duration_ms; the "_started" row
	// records the moment the operation began and has no notion of its
	// own duration yet.
	if _, ok := events[1].Target["duration_ms"]; !ok {
		t.Fatalf("events[1].Target.duration_ms missing — required for closed-trail forensics")
	}
	if _, ok := events[0].Target["duration_ms"]; ok {
		t.Fatalf("events[0].Target.duration_ms set — must only appear on the _ended row")
	}
}

func TestImpersonation_AuditWriteFailsBlocksRequest(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	tgtID := uuid.New()
	masterID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	target := &tenancy.Tenant{ID: tgtID, Name: "target", Host: "target.crm.local"}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{tgtID: target}}
	logger := &recordingLogger{
		failOn: func(event audit.SecurityEvent) error {
			if event == audit.SecurityEventImpersonationStart {
				return errors.New("audit DB down")
			}
			return nil
		},
	}

	downstreamRan := false
	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		downstreamRan = true
	}))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, masterID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	r.Header.Set(middleware.HeaderImpersonationReason, "incident-INC-9")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if downstreamRan {
		t.Fatal("downstream handler ran despite audit write failure — non-repudiation bypass!")
	}
	// Failed start MUST NOT enqueue an _ended row either: the deferred
	// log call only runs when start succeeded. The fake logger records
	// nothing because its failOn rejected the start before append.
	if got := logger.snapshot(); len(got) != 0 {
		t.Fatalf("audit rows recorded after start-fail=%d, want 0", len(got))
	}
}

func TestImpersonation_TenantNotFoundReturns404(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	masterID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{}}
	logger := &recordingLogger{}

	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream must not run for unknown tenant")
	}))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, masterID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, uuid.New().String())
	r.Header.Set(middleware.HeaderImpersonationReason, "audit")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
	if got := logger.snapshot(); len(got) != 0 {
		t.Fatalf("audit rows=%d, want 0 — failed resolver must not emit audit", len(got))
	}
}

func TestImpersonation_InvalidUUIDReturns400(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	masterID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	resolver := &fakeByIDResolver{}
	logger := &recordingLogger{}

	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream must not run for invalid uuid")
	}))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, masterID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, "not-a-uuid")
	r.Header.Set(middleware.HeaderImpersonationReason, "audit")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls=%d, want 0 (invalid uuid must short-circuit)", resolver.calls)
	}
}

func TestImpersonation_CheckerErrorReturns500(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	tgtID := uuid.New()
	userID := uuid.New()
	checker := &fakeMasterChecker{err: errors.New("users table down")}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{tgtID: {ID: tgtID}}}
	logger := &recordingLogger{}

	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream must not run when master check errors")
	}))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, userID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	r.Header.Set(middleware.HeaderImpersonationReason, "audit")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestImpersonation_ResolverInternalErrorReturns500(t *testing.T) {
	t.Parallel()
	srcID := uuid.New()
	tgtID := uuid.New()
	masterID := uuid.New()
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	resolver := &fakeByIDResolver{err: errors.New("db down")}
	logger := &recordingLogger{}

	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream must not run on resolver infra error")
	}))

	rec := httptest.NewRecorder()
	r := requestWithSession(t, srcID, masterID, "/data")
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	r.Header.Set(middleware.HeaderImpersonationReason, "audit")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestImpersonation_NoSessionReturns500(t *testing.T) {
	t.Parallel()
	tgtID := uuid.New()
	checker := &fakeMasterChecker{}
	resolver := &fakeByIDResolver{}
	logger := &recordingLogger{}

	h := middleware.Impersonation(checker, resolver, logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("downstream must not run when session is missing")
	}))

	rec := httptest.NewRecorder()
	// Build a request that has a tenant context but NO session — i.e.
	// the middleware was mounted ahead of Auth.
	r := httptest.NewRequest(http.MethodGet, "/data", nil)
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: uuid.New()})
	r = r.WithContext(ctx)
	r.Header.Set(middleware.HeaderImpersonateTenant, tgtID.String())
	r.Header.Set(middleware.HeaderImpersonationReason, "audit")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (wiring bug)", rec.Code)
	}
	if checker.calls != 0 {
		t.Fatalf("checker calls=%d, want 0 (must short-circuit before lookup)", checker.calls)
	}
}
