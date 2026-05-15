package authz_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
)

// recordCall captures Record arguments so tests can assert the
// per-decision payload the recorder saw. The fake is intentionally
// minimal — exercising the audit row shape is covered by the postgres
// adapter regression test.
type recordCall struct {
	principal iam.Principal
	action    iam.Action
	resource  iam.Resource
	decision  iam.Decision
	at        time.Time
}

// recordingRecorder is a fake Recorder. It is concurrency-safe via
// atomic.Pointer + slice copy on Record so table-driven tests that
// call Can in parallel stay race-free.
type recordingRecorder struct {
	mu    chan struct{}
	calls []recordCall
}

func newRecordingRecorder() *recordingRecorder {
	return &recordingRecorder{mu: make(chan struct{}, 1)}
}

func (r *recordingRecorder) Record(_ context.Context, p iam.Principal, a iam.Action, res iam.Resource, d iam.Decision, now time.Time) {
	r.mu <- struct{}{}
	defer func() { <-r.mu }()
	r.calls = append(r.calls, recordCall{principal: p, action: a, resource: res, decision: d, at: now})
}

func (r *recordingRecorder) snapshot() []recordCall {
	r.mu <- struct{}{}
	defer func() { <-r.mu }()
	out := make([]recordCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// stubAuthorizer returns a fixed Decision regardless of input. Tests
// pre-seed the Decision so the wrapper can be exercised without
// dragging in RBACAuthorizer's matrix.
type stubAuthorizer struct {
	decision iam.Decision
	called   atomic.Int32
}

func (s *stubAuthorizer) Can(context.Context, iam.Principal, iam.Action, iam.Resource) iam.Decision {
	s.called.Add(1)
	return s.decision
}

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func defaultPrincipal() iam.Principal {
	return iam.Principal{UserID: uuid.New(), TenantID: uuid.New(), Roles: []iam.Role{iam.RoleTenantAtendente}}
}

func TestAuditingAuthorizer_Can_PassesDecisionThrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dec  iam.Decision
	}{
		{"deny propagates verbatim", iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC, TargetKind: "contact", TargetID: "abc"}},
		{"allow propagates verbatim", iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC, TargetKind: "contact", TargetID: "abc"}},
		{"tenant-mismatch passes through", iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedTenantMismatch}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := &stubAuthorizer{decision: tc.dec}
			rec := newRecordingRecorder()
			a := authz.New(authz.Config{
				Inner:    inner,
				Recorder: rec,
				Sampler:  authz.AlwaysSample{},
				Now:      fixedNow(time.Unix(1700000000, 0).UTC()),
			})
			got := a.Can(context.Background(), defaultPrincipal(), iam.ActionTenantContactRead, iam.Resource{})
			if got != tc.dec {
				t.Fatalf("Decision was mutated: got %+v want %+v", got, tc.dec)
			}
			if inner.called.Load() != 1 {
				t.Fatalf("inner Authorizer called %d times, want 1", inner.called.Load())
			}
		})
	}
}

func TestAuditingAuthorizer_Can_RecordsDeniesAt100Percent(t *testing.T) {
	t.Parallel()
	inner := &stubAuthorizer{decision: iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}}
	rec := newRecordingRecorder()
	a := authz.New(authz.Config{
		Inner:    inner,
		Recorder: rec,
		Sampler:  authz.NeverSample{},
		Now:      fixedNow(time.Unix(1700000000, 0).UTC()),
	})
	ctx := context.Background()
	p := defaultPrincipal()
	for i := 0; i < 11; i++ {
		_ = a.Can(ctx, p, iam.ActionTenantConversationRead, iam.Resource{Kind: "conversation", ID: "z"})
	}
	calls := rec.snapshot()
	if len(calls) != 11 {
		t.Fatalf("expected 11 recorded denies, got %d", len(calls))
	}
	for i, c := range calls {
		if c.decision.Allow {
			t.Fatalf("call %d: recorded an allow decision, want deny", i)
		}
		if c.decision.ReasonCode != iam.ReasonDeniedRBAC {
			t.Fatalf("call %d: reason_code=%s want %s", i, c.decision.ReasonCode, iam.ReasonDeniedRBAC)
		}
	}
}

func TestAuditingAuthorizer_Can_RecordsAllowsWhenSamplerSaysYes(t *testing.T) {
	t.Parallel()
	inner := &stubAuthorizer{decision: iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC}}
	rec := newRecordingRecorder()
	a := authz.New(authz.Config{
		Inner:    inner,
		Recorder: rec,
		Sampler:  authz.AlwaysSample{},
		Now:      fixedNow(time.Unix(1700000000, 0).UTC()),
	})
	_ = a.Can(context.Background(), defaultPrincipal(), iam.ActionTenantContactRead, iam.Resource{})
	if len(rec.snapshot()) != 1 {
		t.Fatalf("AlwaysSample: expected 1 recorded allow, got %d", len(rec.snapshot()))
	}
}

func TestAuditingAuthorizer_Can_SkipsAllowsWhenSamplerSaysNo(t *testing.T) {
	t.Parallel()
	inner := &stubAuthorizer{decision: iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC}}
	rec := newRecordingRecorder()
	a := authz.New(authz.Config{
		Inner:    inner,
		Recorder: rec,
		Sampler:  authz.NeverSample{},
		Now:      fixedNow(time.Unix(1700000000, 0).UTC()),
	})
	for i := 0; i < 50; i++ {
		_ = a.Can(context.Background(), defaultPrincipal(), iam.ActionTenantContactRead, iam.Resource{})
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("NeverSample: expected 0 recorded allows, got %d", got)
	}
}

func TestAuditingAuthorizer_Can_NilSamplerNeverRecordsAllow(t *testing.T) {
	t.Parallel()
	inner := &stubAuthorizer{decision: iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC}}
	rec := newRecordingRecorder()
	a := authz.New(authz.Config{
		Inner:    inner,
		Recorder: rec,
		Sampler:  nil,
		Now:      fixedNow(time.Unix(1700000000, 0).UTC()),
	})
	_ = a.Can(context.Background(), defaultPrincipal(), iam.ActionTenantContactRead, iam.Resource{})
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("nil sampler: expected 0 recorded allows, got %d", got)
	}
}

func TestAuditingAuthorizer_Can_NilSamplerStillRecordsDeny(t *testing.T) {
	t.Parallel()
	inner := &stubAuthorizer{decision: iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}}
	rec := newRecordingRecorder()
	a := authz.New(authz.Config{
		Inner:    inner,
		Recorder: rec,
		Sampler:  nil,
		Now:      fixedNow(time.Unix(1700000000, 0).UTC()),
	})
	_ = a.Can(context.Background(), defaultPrincipal(), iam.ActionTenantContactRead, iam.Resource{})
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("nil sampler must still record deny: got %d records", got)
	}
}

func TestAuditingAuthorizer_Can_PinnedNowFlowsIntoRecord(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	inner := &stubAuthorizer{decision: iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}}
	rec := newRecordingRecorder()
	a := authz.New(authz.Config{
		Inner:    inner,
		Recorder: rec,
		Sampler:  authz.NeverSample{},
		Now:      fixedNow(want),
	})
	_ = a.Can(context.Background(), defaultPrincipal(), iam.ActionTenantContactRead, iam.Resource{})
	got := rec.snapshot()
	if len(got) != 1 || !got[0].at.Equal(want) {
		t.Fatalf("Now() not threaded into Record: got %+v", got)
	}
}

func TestAuditingAuthorizer_New_NilNowDefaultsToTimeNow(t *testing.T) {
	t.Parallel()
	inner := &stubAuthorizer{decision: iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}}
	rec := newRecordingRecorder()
	before := time.Now().UTC()
	a := authz.New(authz.Config{Inner: inner, Recorder: rec})
	_ = a.Can(context.Background(), defaultPrincipal(), iam.ActionTenantContactRead, iam.Resource{})
	after := time.Now().UTC()
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 record, got %d", len(calls))
	}
	if calls[0].at.Before(before) || calls[0].at.After(after) {
		t.Fatalf("default Now() outside [before, after]: at=%v before=%v after=%v", calls[0].at, before, after)
	}
}

func TestAuditingAuthorizer_New_PanicsOnNilInner(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Inner is nil")
		}
	}()
	_ = authz.New(authz.Config{Recorder: newRecordingRecorder()})
}

func TestAuditingAuthorizer_New_PanicsOnNilRecorder(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Recorder is nil")
		}
	}()
	_ = authz.New(authz.Config{Inner: &stubAuthorizer{}})
}
