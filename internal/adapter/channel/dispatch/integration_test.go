package dispatch_test

// Integration test for the outbound WhatsApp dispatcher (SIN-68306). It
// wires the REAL Meta Cloud sender (internal/adapter/channel/whatsapp)
// behind the production decorator stack —
// Router{"whatsapp": Idempotent(RateLimited(sender))} — and drives it
// against an httptest Graph stand-in. Each SendMessage models one
// operator reply, so the HTTP request counter proves the acceptance
// criteria at the integrated carrier boundary:
//
//   - happy path        → exactly one Graph request;
//   - operator double-submit (same idempotency key) → one Graph request;
//   - transient 5xx      → sender retries, one logical send still records;
//   - flag-off / unrouted → zero Graph requests (deny-by-default no-op);
//   - rate-limit exhausted → zero Graph requests.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/adapter/channel/dispatch"
	"github.com/pericles-luz/crm/internal/adapter/channel/whatsapp"
	"github.com/pericles-luz/crm/internal/inbox"
)

const channelWhatsApp = "whatsapp"

// graphStub is an httptest handler standing in for the Meta Graph
// /{phone_number_id}/messages endpoint. It counts requests and can be
// scripted to fail the first failFirst calls with 500 before succeeding.
type graphStub struct {
	requests  int64
	failFirst int64
	wamid     string
}

func (g *graphStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&g.requests, 1)
		if n <= atomic.LoadInt64(&g.failFirst) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messages":[{"id":"` + g.wamid + `"}]}`))
	}
}

func (g *graphStub) count() int64 { return atomic.LoadInt64(&g.requests) }

// allowNLimiter allows the first N calls per key then denies — a
// deterministic stand-in for the redis sliding window.
type allowNLimiter struct{ max int }

func (l *allowNLimiter) Allow(_ context.Context, _ string, _ time.Duration, _ int) (bool, time.Duration, error) {
	if l.max <= 0 {
		return false, time.Second, nil
	}
	l.max--
	return true, 0, nil
}

// buildDispatcher wires the real sender behind the decorator stack. enabled
// toggles the tenant feature flag; limiter (may be nil) is the outbound
// rate limiter.
func buildDispatcher(t *testing.T, baseURL string, enabled bool, limiter dispatch.RateLimiter) inbox.OutboundChannel {
	t.Helper()
	lookup := func(_ context.Context, _ uuid.UUID) (whatsapp.TenantConfig, error) {
		return whatsapp.TenantConfig{PhoneNumberID: "PN123", Enabled: enabled}, nil
	}
	sender, err := whatsapp.New("META-TOKEN", lookup, prometheus.NewRegistry(),
		whatsapp.WithBaseURL(baseURL),
		whatsapp.WithBackoffBase(0), // no real sleeps between retries
	)
	if err != nil {
		t.Fatalf("whatsapp.New: %v", err)
	}
	var oc inbox.OutboundChannel = sender
	oc = dispatch.NewRateLimited(oc, limiter, time.Minute, 600, nil)
	oc = dispatch.NewIdempotent(oc, dispatch.NewMemoryLedger())
	return dispatch.NewRouter(map[string]inbox.OutboundChannel{channelWhatsApp: oc})
}

func TestIntegration_OperatorReply_OneGraphCall(t *testing.T) {
	t.Parallel()
	stub := &graphStub{wamid: "wamid.HAPPY"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	d := buildDispatcher(t, srv.URL, true, &allowNLimiter{max: 10})
	got, err := d.SendMessage(context.Background(), newMsg(uuid.New(), ""))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != "wamid.HAPPY" {
		t.Errorf("wamid = %q, want wamid.HAPPY", got)
	}
	if stub.count() != 1 {
		t.Errorf("graph requests = %d, want 1", stub.count())
	}
}

func TestIntegration_DoubleSubmit_OneGraphCall(t *testing.T) {
	t.Parallel()
	stub := &graphStub{wamid: "wamid.DEDUP"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	d := buildDispatcher(t, srv.URL, true, &allowNLimiter{max: 10})
	tenant := uuid.New()
	m1 := newMsg(tenant, "compose-token-1")
	m2 := newMsg(tenant, "compose-token-1") // operator double-clicked send

	if _, err := d.SendMessage(context.Background(), m1); err != nil {
		t.Fatalf("first: %v", err)
	}
	w2, err := d.SendMessage(context.Background(), m2)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if w2 != "wamid.DEDUP" {
		t.Errorf("second wamid = %q, want cached wamid.DEDUP", w2)
	}
	if stub.count() != 1 {
		t.Errorf("graph requests = %d, want 1 (double-submit must not double-send)", stub.count())
	}
}

func TestIntegration_TransientRetry_ThenSucceeds(t *testing.T) {
	t.Parallel()
	// Fail the first two attempts (500), succeed on the third. The sender's
	// bounded retry (3 attempts) absorbs it; the dispatcher records one send.
	stub := &graphStub{wamid: "wamid.RETRY", failFirst: 2}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	d := buildDispatcher(t, srv.URL, true, &allowNLimiter{max: 10})
	got, err := d.SendMessage(context.Background(), newMsg(uuid.New(), "k-retry"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != "wamid.RETRY" {
		t.Errorf("wamid = %q, want wamid.RETRY", got)
	}
	if stub.count() != 3 {
		t.Errorf("graph attempts = %d, want 3 (2 transient + 1 success)", stub.count())
	}
}

func TestIntegration_FlagOff_NoGraphCall(t *testing.T) {
	t.Parallel()
	stub := &graphStub{wamid: "wamid.NEVER"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	d := buildDispatcher(t, srv.URL, false /* disabled */, &allowNLimiter{max: 10})
	_, err := d.SendMessage(context.Background(), newMsg(uuid.New(), ""))
	if err == nil {
		t.Fatal("want error when flag off, got nil")
	}
	if stub.count() != 0 {
		t.Errorf("graph requests = %d, want 0 (flag-off issues no HTTP)", stub.count())
	}
}

func TestIntegration_UnroutedChannel_NoGraphCall(t *testing.T) {
	t.Parallel()
	stub := &graphStub{wamid: "wamid.NEVER"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	d := buildDispatcher(t, srv.URL, true, &allowNLimiter{max: 10})
	m := newMsg(uuid.New(), "")
	m.Channel = "sms" // no route registered for sms
	_, err := d.SendMessage(context.Background(), m)
	if err == nil {
		t.Fatal("want error for unrouted channel, got nil")
	}
	if stub.count() != 0 {
		t.Errorf("graph requests = %d, want 0 (unrouted issues no HTTP)", stub.count())
	}
}

func TestIntegration_RateLimited_NoGraphCall(t *testing.T) {
	t.Parallel()
	stub := &graphStub{wamid: "wamid.NEVER"}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	// Budget of zero → the first send is already over the limit.
	d := buildDispatcher(t, srv.URL, true, &allowNLimiter{max: 0})
	_, err := d.SendMessage(context.Background(), newMsg(uuid.New(), ""))
	if err == nil {
		t.Fatal("want transient error when rate-limited, got nil")
	}
	if stub.count() != 0 {
		t.Errorf("graph requests = %d, want 0 (rate-limited issues no HTTP)", stub.count())
	}
}

func newMsg(tenant uuid.UUID, key string) inbox.OutboundMessage {
	return inbox.OutboundMessage{
		TenantID:       tenant,
		ConversationID: uuid.New(),
		Channel:        channelWhatsApp,
		ToExternalID:   "+5511988887777",
		Body:           "resposta do operador",
		IdempotencyKey: key,
	}
}
