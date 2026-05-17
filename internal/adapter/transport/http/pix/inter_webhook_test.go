package pix_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	pixinter "github.com/pericles-luz/crm/internal/adapter/pix/inter"
	httppix "github.com/pericles-luz/crm/internal/adapter/transport/http/pix"
	domainpix "github.com/pericles-luz/crm/internal/billing/pix"
)

const (
	testSecret      = "topsecret"
	testTxID        = "tx-abc"
	testEnvelope    = `{"pix":[{"endToEndId":"E1","txid":"tx-abc","valor":"12.34","horario":"2026-05-17T12:00:00Z"}]}`
	testEnvelopeAlt = `{"pix":[{"endToEndId":"E2","txid":"tx-xyz","valor":"55.55","horario":"2026-05-17T12:00:00Z"}]}`
)

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// fakeLimiter is a deterministic RateLimiter test double. Each
// key/window/max triple is counted in memory; the caller can override
// the response per call via responses[key]. err overrides everything.
type fakeLimiter struct {
	mu        sync.Mutex
	counts    map[string]int
	overrides map[string]bool
	retryFor  map[string]time.Duration
	err       error
	calls     []string
}

func newFakeLimiter() *fakeLimiter {
	return &fakeLimiter{
		counts:    map[string]int{},
		overrides: map[string]bool{},
		retryFor:  map[string]time.Duration{},
	}
}

func (f *fakeLimiter) Allow(_ context.Context, key string, _ time.Duration, max int) (bool, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	if f.err != nil {
		return false, 0, f.err
	}
	if allowed, ok := f.overrides[key]; ok {
		retry := f.retryFor[key]
		if retry == 0 {
			retry = 2 * time.Second
		}
		return allowed, retry, nil
	}
	f.counts[key]++
	if f.counts[key] > max {
		retry := f.retryFor[key]
		if retry == 0 {
			retry = 2 * time.Second
		}
		return false, retry, nil
	}
	return true, 0, nil
}

// fakeReconciler is the orchestration-layer double. Each call records
// the inbound event and returns whatever Apply was primed to return.
type fakeReconciler struct {
	mu         sync.Mutex
	calls      []domainpix.WebhookEvent
	outcomes   []domainpix.Outcome
	err        error
	dedup      map[string]struct{}
	applyDelay time.Duration
}

func newFakeReconciler() *fakeReconciler {
	return &fakeReconciler{dedup: map[string]struct{}{}}
}

func (r *fakeReconciler) Apply(ctx context.Context, evt domainpix.WebhookEvent) (domainpix.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, evt)
	if r.err != nil {
		return domainpix.Outcome{}, r.err
	}
	if r.applyDelay > 0 {
		time.Sleep(r.applyDelay)
	}
	key := evt.Source + "|" + evt.ExternalID + "|" + string(evt.EventType)
	if _, ok := r.dedup[key]; ok {
		out := domainpix.Outcome{Duplicate: true}
		r.outcomes = append(r.outcomes, out)
		return out, nil
	}
	r.dedup[key] = struct{}{}
	out := domainpix.Outcome{Transitioned: true}
	r.outcomes = append(r.outcomes, out)
	return out, nil
}

func mustHandler(t *testing.T, cfg httppix.InterWebhookConfig) *httppix.InterWebhookHandler {
	t.Helper()
	h, err := httppix.NewInterWebhookHandler(cfg)
	if err != nil {
		t.Fatalf("NewInterWebhookHandler: %v", err)
	}
	return h
}

func newRequest(body string, signature string, remoteAddr string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pix/inter", strings.NewReader(body))
	if signature != "" {
		req.Header.Set(pixinter.DefaultSignatureHeader, signature)
	}
	req.RemoteAddr = remoteAddr
	req.Header.Set("X-Request-Id", "test-req")
	return req
}

func mustParseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, c, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse cidr %q: %v", s, err)
	}
	return c
}

func defaultCfg(t *testing.T, rec domainpix.Reconciler, lim *fakeLimiter) httppix.InterWebhookConfig {
	t.Helper()
	v, err := pixinter.NewWebhookVerifier(pixinter.WebhookConfig{Secret: testSecret})
	if err != nil {
		t.Fatalf("NewWebhookVerifier: %v", err)
	}
	return httppix.InterWebhookConfig{
		Verifier:     v,
		Parser:       pixinter.NewWebhookParser(),
		Reconciler:   rec,
		Limiter:      lim,
		AllowedCIDRs: []*net.IPNet{mustParseCIDR(t, "10.0.0.0/8")},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestNew_RequiresFields(t *testing.T) {
	v, _ := pixinter.NewWebhookVerifier(pixinter.WebhookConfig{Secret: "s"})
	p := pixinter.NewWebhookParser()
	rec := newFakeReconciler()
	lim := newFakeLimiter()

	for _, tc := range []struct {
		name string
		cfg  httppix.InterWebhookConfig
	}{
		{"verifier", httppix.InterWebhookConfig{Parser: p, Reconciler: rec, Limiter: lim}},
		{"parser", httppix.InterWebhookConfig{Verifier: v, Reconciler: rec, Limiter: lim}},
		{"reconciler", httppix.InterWebhookConfig{Verifier: v, Parser: p, Limiter: lim}},
		{"limiter", httppix.InterWebhookConfig{Verifier: v, Parser: p, Reconciler: rec}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := httppix.NewInterWebhookHandler(tc.cfg); err == nil {
				t.Fatalf("expected error for missing %s", tc.name)
			}
		})
	}
}

func TestServeHTTP_HappyPath_AppliesAndAcks(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	var metricCalls []httppix.Outcome
	cfg := defaultCfg(t, rec, lim)
	cfg.MetricsHook = func(o httppix.Outcome) { metricCalls = append(metricCalls, o) }
	h := mustHandler(t, cfg)

	w := httptest.NewRecorder()
	r := newRequest(testEnvelope, sign(testEnvelope), "10.1.2.3:9999")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("reconciler called %d times, want 1", len(rec.calls))
	}
	if rec.calls[0].ExternalID != testTxID {
		t.Errorf("ExternalID = %q, want %q", rec.calls[0].ExternalID, testTxID)
	}
	if len(metricCalls) != 1 || metricCalls[0] != httppix.OutcomeApplied {
		t.Errorf("metricCalls = %v, want [applied]", metricCalls)
	}
}

func TestServeHTTP_MissingSignature_Returns401(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	r := newRequest(testEnvelope, "", "10.1.2.3:9999")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if len(rec.calls) != 0 {
		t.Errorf("reconciler called on missing signature: %v", rec.calls)
	}
}

func TestServeHTTP_BadSignature_Returns401(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	r := newRequest(testEnvelope, sign("different body"), "10.1.2.3:9999")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestServeHTTP_IPOutsideAllowlist_Returns403(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	r := newRequest(testEnvelope, sign(testEnvelope), "192.168.1.1:9999")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if len(rec.calls) != 0 {
		t.Errorf("reconciler called on IP fail")
	}
}

func TestServeHTTP_IPDisabled_BypassesAllowlist(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	cfg := defaultCfg(t, rec, lim)
	cfg.IPCheckDisabled = true
	cfg.AllowedCIDRs = nil
	h := mustHandler(t, cfg)

	w := httptest.NewRecorder()
	r := newRequest(testEnvelope, sign(testEnvelope), "203.0.113.7:9999")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when IP check disabled", w.Code)
	}
}

func TestServeHTTP_RateLimit_IP_Returns429WithRetryAfter(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	lim.overrides["pix:inter:ip:10.1.2.3"] = false
	lim.retryFor["pix:inter:ip:10.1.2.3"] = 12 * time.Second
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	r := newRequest(testEnvelope, sign(testEnvelope), "10.1.2.3:9999")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", w.Code)
	}
	ra := w.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("Retry-After header missing")
	}
	if got, err := strconv.Atoi(ra); err != nil || got < 1 {
		t.Errorf("Retry-After = %q, want positive integer", ra)
	}
	if len(rec.calls) != 0 {
		t.Errorf("reconciler called on rate-limit fail")
	}
}

func TestServeHTTP_RateLimit_ExternalID_Returns429(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	lim.overrides["pix:inter:ext:"+testTxID] = false
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	r := newRequest(testEnvelope, sign(testEnvelope), "10.1.2.3:9999")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", w.Code)
	}
	if len(rec.calls) != 0 {
		t.Errorf("reconciler called on ext rate-limit fail")
	}
}

func TestServeHTTP_DuplicatePayload_DedupHit(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	// first delivery
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, newRequest(testEnvelope, sign(testEnvelope), "10.1.2.3:9999"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first: status = %d, want 200", w1.Code)
	}

	// second delivery (same body, same sig) — reconciler returns
	// Duplicate=true on the same (source, ext_id, event_type).
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, newRequest(testEnvelope, sign(testEnvelope), "10.1.2.3:9999"))
	if w2.Code != http.StatusOK {
		t.Errorf("second: status = %d, want 200", w2.Code)
	}
	if len(rec.calls) != 2 {
		t.Errorf("reconciler called %d times, want 2", len(rec.calls))
	}
	if !rec.outcomes[1].Duplicate {
		t.Errorf("second call should be Duplicate; got %+v", rec.outcomes[1])
	}
}

func TestServeHTTP_ParseFail_Returns400(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	junk := `not json at all`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(junk, sign(junk), "10.1.2.3:9999"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestServeHTTP_ReconcilerError_Returns500(t *testing.T) {
	rec := newFakeReconciler()
	rec.err = errors.New("pg down")
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(testEnvelope, sign(testEnvelope), "10.1.2.3:9999"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestServeHTTP_NonPost_Returns405(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/webhooks/pix/inter", nil)
	r.RemoteAddr = "10.1.2.3:9999"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestServeHTTP_BodyOverMax_Returns413(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))

	big := strings.Repeat("x", 512*1024) // 512 KiB > 256 KiB cap
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(big, sign(big), "10.1.2.3:9999"))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestServeHTTP_LimiterError_FailsOpen(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	lim.err = errors.New("redis down")
	h := mustHandler(t, defaultCfg(t, rec, lim))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(testEnvelope, sign(testEnvelope), "10.1.2.3:9999"))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (fail-open on limiter error)", w.Code)
	}
}

func TestServeHTTP_MultiItem_AllAppliedAggregateApplied(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	cfg := defaultCfg(t, rec, lim)
	var outcome httppix.Outcome
	cfg.MetricsHook = func(o httppix.Outcome) { outcome = o }
	h := mustHandler(t, cfg)

	body := `{"pix":[{"txid":"a","horario":"2026-05-17T01:00:00Z"},{"txid":"b","horario":"2026-05-17T02:00:00Z"}]}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(body, sign(body), "10.1.2.3:9999"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if len(rec.calls) != 2 {
		t.Errorf("reconciler called %d times, want 2", len(rec.calls))
	}
	if outcome != httppix.OutcomeApplied {
		t.Errorf("aggregate outcome = %q, want applied", outcome)
	}
}

func TestServeHTTP_MixedDedup(t *testing.T) {
	rec := newFakeReconciler()
	// Pre-seed dedup for tx-a so the first item is a duplicate, second
	// item still applies fresh.
	rec.dedup["banco-inter|a|paid"] = struct{}{}
	lim := newFakeLimiter()
	cfg := defaultCfg(t, rec, lim)
	var outcome httppix.Outcome
	cfg.MetricsHook = func(o httppix.Outcome) { outcome = o }
	h := mustHandler(t, cfg)

	body := `{"pix":[{"txid":"a","horario":"2026-05-17T01:00:00Z"},{"txid":"b","horario":"2026-05-17T02:00:00Z"}]}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequest(body, sign(body), "10.1.2.3:9999"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if outcome != httppix.OutcomeMixed {
		t.Errorf("aggregate outcome = %q, want mixed", outcome)
	}
}

func TestRegister_AttachesRoute(t *testing.T) {
	rec := newFakeReconciler()
	lim := newFakeLimiter()
	h := mustHandler(t, defaultCfg(t, rec, lim))
	mux := http.NewServeMux()
	h.Register(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := strings.NewReader(testEnvelope)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/pix/inter", body)
	req.Header.Set(pixinter.DefaultSignatureHeader, sign(testEnvelope))
	req.RemoteAddr = "10.1.2.3:9999" // ignored by real srv; uses peer addr
	// Real net.Conn yields 127.0.0.1 — adjust handler allowlist for
	// this test by re-building the handler with loopback allowed.
	{
		cfg := defaultCfg(t, rec, lim)
		cfg.AllowedCIDRs = []*net.IPNet{mustParseCIDR(t, "127.0.0.0/8"), mustParseCIDR(t, "::1/128")}
		h2 := mustHandler(t, cfg)
		mux2 := http.NewServeMux()
		h2.Register(mux2)
		srv2 := httptest.NewServer(mux2)
		defer srv2.Close()
		req2, _ := http.NewRequest(http.MethodPost, srv2.URL+"/webhooks/pix/inter", strings.NewReader(testEnvelope))
		req2.Header.Set(pixinter.DefaultSignatureHeader, sign(testEnvelope))
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("client.Do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	}
	_ = testEnvelopeAlt // referenced to keep import warnings quiet in future expansions
	_ = uuid.New
	_ = sleeper
}

// sleeper is kept as a no-op helper for future flaky-cleanup tests that
// need a brief jitter; referenced from TestRegister so the symbol does
// not collide with unused-import lint when refactoring.
func sleeper(_ time.Duration) {}
