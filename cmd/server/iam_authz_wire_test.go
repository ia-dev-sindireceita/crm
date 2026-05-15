package main

// SIN-62765 — wireup tests for the production AuditingAuthorizer
// assembled in iam_authz_wire.go. Coverage focuses on the seams the
// boot path actually exercises: env parsing, error propagation when
// the pool surface is missing, and the end-to-end behaviour that a
// Decision flows through the recorder and into Prometheus.
//
// The recorder is exercised against a fake AuditExecutor that captures
// the SQL the postgres SplitAuditLogger emits — running these against
// a real Postgres is covered by the SIN-62254 integration test in
// internal/adapter/db/postgres/authz_audit_test.go. Here we only need
// to prove the wireup is correctly stitched together.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	prommodel "github.com/prometheus/client_model/go"

	"github.com/pericles-luz/crm/internal/iam"
)

func TestParseAuthzAllowSampleRate_DefaultsWhenUnset(t *testing.T) {
	t.Parallel()
	got := parseAuthzAllowSampleRate(func(string) string { return "" })
	if got != defaultAuthzAllowSampleRate {
		t.Fatalf("rate = %v, want %v", got, defaultAuthzAllowSampleRate)
	}
}

func TestParseAuthzAllowSampleRate_HonoursEnvOverride(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want float64
	}{
		{"1.0", 1.0},
		{"0.5", 0.5},
		{"0", 0.0},
		{"0.001", 0.001},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			got := parseAuthzAllowSampleRate(func(k string) string {
				if k == envAuthzAllowSampleRate {
					return tc.raw
				}
				return ""
			})
			if got != tc.want {
				t.Fatalf("rate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseAuthzAllowSampleRate_FallsBackOnGarbage(t *testing.T) {
	t.Parallel()
	got := parseAuthzAllowSampleRate(func(k string) string {
		if k == envAuthzAllowSampleRate {
			return "not-a-float"
		}
		return ""
	})
	if got != defaultAuthzAllowSampleRate {
		t.Fatalf("garbage rate = %v, want default %v", got, defaultAuthzAllowSampleRate)
	}
}

func TestNewAuditedAuthorizer_NilPoolErrors(t *testing.T) {
	t.Parallel()
	_, err := newAuditedAuthorizer(nil, prometheus.NewRegistry(), func(string) string { return "" }, nil)
	if err == nil {
		t.Fatal("want error for nil pool, got nil")
	}
}

// fakeAuditExecutor implements postgresadapter.AuditExecutor and
// captures the SQL the SplitAuditLogger executes. Used to verify that
// the recorder is wired all the way through to the writer at boot.
type fakeAuditExecutor struct {
	mu    sync.Mutex
	calls []fakeExec
}

type fakeExec struct {
	sql  string
	args []any
}

func (f *fakeAuditExecutor) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeExec{sql: sql, args: append([]any(nil), args...)})
	return pgconn.CommandTag{}, nil
}

func (f *fakeAuditExecutor) snapshot() []fakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeExec, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestNewAuditedAuthorizer_DenyFlowsThroughToWriterAndMetrics is the
// wire-level integration test: build the authorizer the way cmd/server
// does at boot, drive a deny through it, and assert the SQL hit
// audit_log_security AND the Prometheus counters incremented. This
// proves the decorator is composed end-to-end and that F10's two
// visibility channels (DB row + counter) both light up on a real deny.
func TestNewAuditedAuthorizer_DenyFlowsThroughToWriterAndMetrics(t *testing.T) {
	t.Parallel()
	pool := &fakeAuditExecutor{}
	reg := prometheus.NewRegistry()
	authzr, err := newAuditedAuthorizer(pool, reg, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("newAuditedAuthorizer: %v", err)
	}

	// Common tenant role cannot read PII → deny via the ADR 0090
	// matrix. The decorator MUST record this without changing the
	// verdict.
	p := iam.Principal{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []iam.Role{iam.RoleTenantCommon},
	}
	d := authzr.Can(context.Background(), p, iam.ActionTenantContactReadPII, iam.Resource{Kind: "contact", ID: "abc"})
	if d.Allow {
		t.Fatalf("want deny, got allow: %+v", d)
	}
	if d.ReasonCode != iam.ReasonDeniedRBAC {
		t.Fatalf("reason_code = %s, want %s", d.ReasonCode, iam.ReasonDeniedRBAC)
	}

	// SplitAuditLogger.WriteSecurity emits an INSERT against
	// audit_log_security; the deny path runs it 1×.
	calls := pool.snapshot()
	if len(calls) != 1 {
		t.Fatalf("audit_log_security INSERTs = %d, want 1", len(calls))
	}
	if !strings.Contains(calls[0].sql, "audit_log_security") {
		t.Fatalf("unexpected SQL on deny: %q", calls[0].sql)
	}
	// args order matches splitSecurityInsertSQL:
	// (tenant, actor, event, target, occurred_at)
	if got := calls[0].args[2]; got != "authz_deny" {
		t.Fatalf("event_type = %v, want authz_deny", got)
	}

	// Both counters MUST increment: the dashboard counter
	// (authz_decisions_total) and the deny-only per-actor counter
	// (authz_user_deny_total) that backs the probing alert.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := metricsByName(mfs)
	if v := counterValue(t, got, "authz_decisions_total", map[string]string{
		"action":      string(iam.ActionTenantContactReadPII),
		"reason_code": string(iam.ReasonDeniedRBAC),
		"outcome":     "deny",
	}); v != 1 {
		t.Fatalf("authz_decisions_total = %v, want 1", v)
	}
	if v := counterValue(t, got, "authz_user_deny_total", map[string]string{
		"actor_user_id": p.UserID.String(),
		"tenant_id":     p.TenantID.String(),
	}); v != 1 {
		t.Fatalf("authz_user_deny_total = %v, want 1", v)
	}
}

// TestNewAuditedAuthorizer_AllowSampled100PctRecordsRow verifies the
// env-flipped 100% allow path the issue calls out: when oncall sets
// AUTHZ_ALLOW_SAMPLE_RATE=1.0, a real allow request emits an
// audit_log_security row with event_type='authz_allow'. Default 1%
// is exercised by the deterministic sampler unit tests.
func TestNewAuditedAuthorizer_AllowSampled100PctRecordsRow(t *testing.T) {
	t.Parallel()
	pool := &fakeAuditExecutor{}
	reg := prometheus.NewRegistry()
	authzr, err := newAuditedAuthorizer(pool, reg, func(k string) string {
		if k == envAuthzAllowSampleRate {
			return "1.0"
		}
		return ""
	}, nil)
	if err != nil {
		t.Fatalf("newAuditedAuthorizer: %v", err)
	}

	// Atendente CAN send messages → allow via the ADR 0090 matrix.
	p := iam.Principal{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []iam.Role{iam.RoleTenantAtendente},
	}
	d := authzr.Can(context.Background(), p, iam.ActionTenantMessageSend, iam.Resource{Kind: "conversation", ID: "abc"})
	if !d.Allow {
		t.Fatalf("want allow, got deny: %+v", d)
	}

	calls := pool.snapshot()
	if len(calls) != 1 {
		t.Fatalf("audit_log_security INSERTs = %d, want 1 (sampler=1.0)", len(calls))
	}
	if got := calls[0].args[2]; got != "authz_allow" {
		t.Fatalf("event_type = %v, want authz_allow", got)
	}
}

// TestNewAuditedAuthorizer_DefaultSampleRateDropsAllowsAlmostAlways
// is the boundary test for the default 1% sampling: across many allow
// decisions the recorder is invoked far less often than 100% of the
// time, which is the property that keeps audit_log_security growth
// bounded in production.
//
// The deterministic sampler hashes request_id; without a request id in
// context it falls back to crypto/rand, so the exact hit count is not
// deterministic — we only assert it is well below the request count.
func TestNewAuditedAuthorizer_DefaultSampleRateDropsAllowsAlmostAlways(t *testing.T) {
	t.Parallel()
	pool := &fakeAuditExecutor{}
	reg := prometheus.NewRegistry()
	authzr, err := newAuditedAuthorizer(pool, reg, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("newAuditedAuthorizer: %v", err)
	}

	p := iam.Principal{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Roles:    []iam.Role{iam.RoleTenantAtendente},
	}
	const n = 200
	for i := 0; i < n; i++ {
		_ = authzr.Can(context.Background(), p, iam.ActionTenantMessageSend, iam.Resource{Kind: "conversation", ID: "abc"})
	}
	got := len(pool.snapshot())
	// At 1% expected, n=200 → mean 2; 50 is a generous upper bound
	// that catches the wireup mistake "default rate flipped to 1.0"
	// without flaking on legitimate sampling variance.
	if got >= 50 {
		t.Fatalf("recorded allows = %d/%d — default rate appears to be ~100%%", got, n)
	}
}

func metricsByName(mfs []*prommodel.MetricFamily) map[string]*prommodel.MetricFamily {
	out := make(map[string]*prommodel.MetricFamily, len(mfs))
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

func counterValue(t *testing.T, mfs map[string]*prommodel.MetricFamily, name string, want map[string]string) float64 {
	t.Helper()
	mf, ok := mfs[name]
	if !ok {
		t.Fatalf("metric family %q not registered", name)
	}
	for _, m := range mf.Metric {
		if labelsMatch(m, want) {
			return m.GetCounter().GetValue()
		}
	}
	t.Fatalf("no %s child matched labels %v", name, want)
	return 0
}

func labelsMatch(m *prommodel.Metric, want map[string]string) bool {
	have := make(map[string]string, len(m.Label))
	for _, lp := range m.Label {
		have[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
