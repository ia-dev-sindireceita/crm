package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pericles-luz/crm/internal/worker"
)

// fakeWebhookPool implements webhookPool for the wire-up tests. The
// stores opened against it are never exercised by these tests — the
// handler-mounted-on-mux tests stop at the registration boundary, and
// the reconciler runs against the noop UnpublishedSource so no DB
// chatter happens. The Close hook lets tests assert cleanup order.
type fakeWebhookPool struct {
	closed bool
}

func (f *fakeWebhookPool) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errPgxRow{}
}
func (f *fakeWebhookPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeWebhookPool) Close() { f.closed = true }

func webhookEnvAll(addr string) func(string) string {
	return func(k string) string {
		switch k {
		case envHTTPAddr:
			return addr
		case envWebhookEnabled:
			return "1"
		case "DATABASE_URL":
			return "postgres://x"
		case envWebhookMetaWhatsAppSecret:
			return "whatsapp-secret"
		case envWebhookMetaInstagramSecret:
			return "instagram-secret"
		case envWebhookMetaFacebookSecret:
			return "facebook-secret"
		}
		return ""
	}
}

func TestBuildWebhookWiring_DisabledByDefault(t *testing.T) {
	t.Parallel()
	getenv := func(string) string { return "" }
	wh := buildWebhookWiring(context.Background(), getenv)
	if wh != nil {
		t.Fatalf("expected nil wiring when WEBHOOK_ENABLED unset")
	}
}

func TestBuildWebhookWiring_RequiresDSN(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envWebhookEnabled {
			return "1"
		}
		return ""
	}
	wh := buildWebhookWiring(context.Background(), getenv)
	if wh != nil {
		t.Fatalf("expected nil wiring when DATABASE_URL unset")
	}
}

func TestBuildWebhookWiring_RequiresAtLeastOneChannelSecret(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envWebhookEnabled:
			return "1"
		case "DATABASE_URL":
			return "postgres://x"
		}
		return ""
	}
	dial := func(context.Context, string) (webhookPool, error) {
		t.Fatalf("dial must not be called when no channel secrets are configured")
		return nil, nil
	}
	wh := buildWebhookWiringWithDeps(context.Background(), getenv, dial)
	if wh != nil {
		t.Fatalf("expected nil wiring when no Meta channel secrets are set")
	}
}

func TestBuildWebhookWiring_DialErrorReturnsNil(t *testing.T) {
	t.Parallel()
	getenv := webhookEnvAll("")
	dial := func(context.Context, string) (webhookPool, error) {
		return nil, errBoom
	}
	wh := buildWebhookWiringWithDeps(context.Background(), getenv, dial)
	if wh != nil {
		t.Fatal("expected nil wiring when dial errors")
	}
}

func TestBuildWebhookWiring_RegistersWebhookRoute(t *testing.T) {
	t.Parallel()
	pool := &fakeWebhookPool{}
	dial := func(context.Context, string) (webhookPool, error) {
		return pool, nil
	}
	wh := buildWebhookWiringWithDeps(context.Background(), webhookEnvAll(""), dial)
	if wh == nil {
		t.Fatal("expected non-nil wiring with full env + stub dial")
	}
	t.Cleanup(wh.Cleanup)

	mux := http.NewServeMux()
	wh.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// POST against an unknown channel still answers 200 (anti-enumeration
	// — the webhook handler always 200s). We hit `telegram` because no
	// Meta secret is registered for it, so the service short-circuits at
	// HasAdapter and the handler still acks.
	res, err := http.Post(srv.URL+"/webhooks/telegram/some-token", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (anti-enumeration)", res.StatusCode)
	}

	// GET on the webhook path is not registered (the pattern is method-
	// scoped to POST), so a method-mismatch response is expected. Go's
	// stdlib mux returns 405 with an Allow header for known paths.
	res2, err := http.Get(srv.URL + "/webhooks/telegram/some-token")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405 (method scoped)", res2.StatusCode)
	}
}

func TestBuildWebhookWiring_RunWorkerExitsOnContextCancel(t *testing.T) {
	t.Parallel()
	pool := &fakeWebhookPool{}
	dial := func(context.Context, string) (webhookPool, error) {
		return pool, nil
	}
	wh := buildWebhookWiringWithDeps(context.Background(), webhookEnvAll(""), dial)
	if wh == nil {
		t.Fatal("expected non-nil wiring")
	}
	t.Cleanup(wh.Cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- wh.RunWorker(ctx) }()

	// Give the reconciler a moment to enter Run() and run its first
	// tick so the test exercises both startup and cancel paths.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunWorker returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWorker did not exit after context cancel")
	}
}

func TestBuildWebhookWiring_CleanupClosesPool(t *testing.T) {
	t.Parallel()
	pool := &fakeWebhookPool{}
	dial := func(context.Context, string) (webhookPool, error) {
		return pool, nil
	}
	wh := buildWebhookWiringWithDeps(context.Background(), webhookEnvAll(""), dial)
	if wh == nil {
		t.Fatal("expected non-nil wiring")
	}
	wh.Cleanup()
	if !pool.closed {
		t.Fatal("Cleanup did not close pool")
	}
}

func TestBuildMetaAdapters_EmptyEnvReturnsEmpty(t *testing.T) {
	t.Parallel()
	out, err := buildMetaAdapters(func(string) string { return "" })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("len = %d, want 0", len(out))
	}
}

func TestBuildMetaAdapters_PerChannelSecrets(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		env  func(string) string
		want []string
	}{
		"only_whatsapp": {
			env: func(k string) string {
				if k == envWebhookMetaWhatsAppSecret {
					return "ws"
				}
				return ""
			},
			want: []string{"whatsapp"},
		},
		"all_three": {
			env: func(k string) string {
				switch k {
				case envWebhookMetaWhatsAppSecret:
					return "ws"
				case envWebhookMetaInstagramSecret:
					return "ig"
				case envWebhookMetaFacebookSecret:
					return "fb"
				}
				return ""
			},
			want: []string{"whatsapp", "instagram", "facebook"},
		},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := buildMetaAdapters(tc.env)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(out) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(out), len(tc.want))
			}
			for i, a := range out {
				if a.Name() != tc.want[i] {
					t.Fatalf("[%d] name = %q, want %q", i, a.Name(), tc.want[i])
				}
			}
		})
	}
}

func TestNoopPublisher_PublishReturnsNil(t *testing.T) {
	t.Parallel()
	p := newNoopPublisher()
	if err := p.Publish(context.Background(), [16]byte{}, [16]byte{}, "whatsapp", []byte(`{}`), nil); err != nil {
		t.Fatalf("Publish err = %v", err)
	}
}

func TestNoopUnpublishedSource_FetchReturnsEmpty(t *testing.T) {
	t.Parallel()
	src := newNoopUnpublishedSource()
	rows, err := src.FetchUnpublished(context.Background(), time.Now(), 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("len = %d, want 0", len(rows))
	}
}

// TestRunWith_MountsWebhookAndRunsWorker exercises the SIN-62300 wiring
// inside runWith: build a webhook + reconciler, start the public HTTP
// listener, hit the webhook route to confirm it is mounted, then cancel
// the context and verify both the HTTP server and the reconciler exit
// cleanly. This is the cmd/server-level coverage required by the
// SIN-62300 acceptance criteria.
func TestRunWith_MountsWebhookAndRunsWorker(t *testing.T) {
	t.Parallel()
	ln := freeListener(t)
	addr := ln.Addr().String()
	pool := &fakeWebhookPool{}
	dial := func(context.Context, string) (webhookPool, error) {
		return pool, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runWithListener(ctx, ln, webhookEnvAll(addr), dial) }()
	waitForListening(t, addr)

	res, err := http.Post("http://"+addr+"/webhooks/whatsapp/some-token", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (anti-enumeration)", res.StatusCode)
	}

	// /health stays available alongside the webhook route.
	res2, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", res2.StatusCode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runWith returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runWith did not return after context cancel")
	}

	if !pool.closed {
		t.Fatal("expected webhook pool Close() on shutdown")
	}
}

// TestRunWith_WrapsWorkerErrorOnDeadline forces the reconciler to exit
// with context.DeadlineExceeded (which RunWorker propagates as non-nil)
// and asserts runWith returns the wrapped worker error after the HTTP
// listener has drained. Covers the workerErr branch of runWith.
func TestRunWith_WrapsWorkerErrorOnDeadline(t *testing.T) {
	t.Parallel()
	ln := freeListener(t)
	addr := ln.Addr().String()
	pool := &fakeWebhookPool{}
	dial := func(context.Context, string) (webhookPool, error) {
		return pool, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := runWithListener(ctx, ln, webhookEnvAll(addr), dial)
	if err == nil {
		t.Fatal("expected non-nil error after deadline")
	}
	if !strings.Contains(err.Error(), "webhook reconciler") {
		t.Fatalf("err = %v; want it to mention 'webhook reconciler'", err)
	}
	if !pool.closed {
		t.Fatal("expected pool.Close on shutdown")
	}
}

// TestRunWith_WebhookDisabledStillRunsHealth confirms the existing
// /health-only baseline keeps working when WEBHOOK_ENABLED is not set —
// runWith must not require the webhook env to boot.
func TestRunWith_WebhookDisabledStillRunsHealth(t *testing.T) {
	t.Parallel()
	ln := freeListener(t)
	addr := ln.Addr().String()
	dial := func(context.Context, string) (webhookPool, error) {
		t.Fatal("dial must not be called when WEBHOOK_ENABLED is unset")
		return nil, nil
	}
	getenv := func(string) string { return "" }

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- runWithListener(ctx, ln, getenv, dial) }()
	waitForListening(t, addr)

	res, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runWith returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runWith did not return after context cancel")
	}
}

// Compile-time guard: the test fake satisfies the same surface as the
// production pool, kept here so refactors that change webhookPool
// catch up the tests at build time.
var _ webhookPool = (*fakeWebhookPool)(nil)

// _ keeps go vet happy if the worker import is otherwise unused.
var _ = worker.UnpublishedRow{}
