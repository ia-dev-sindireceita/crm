package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthHandler_ReturnsOKJSON(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	healthHandler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body[status] = %q, want %q", body["status"], "ok")
	}
}

func TestNewMux_RoutesHealth(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
}

// TestNewMux_HealthEmitsCommitSHA pins the SIN-63165 wireup fix: /health
// served through newMux MUST emit commit_sha so cd-stg.yml's version gate
// (SIN-63146) can compare it against github.sha. Pre-fix this assertion
// failed because cmd/server/main.go shadowed handler.Health with an
// inline {"status":"ok"} handler. Without a ldflag the value falls back
// to "unknown"; never empty, never absent.
func TestNewMux_HealthEmitsCommitSHA(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("body[status] = %q, want %q", body["status"], "ok")
	}
	sha, ok := body["commit_sha"]
	if !ok {
		t.Fatalf("body missing commit_sha field; got %v", body)
	}
	if sha == "" {
		t.Fatalf("body[commit_sha] is empty; want non-empty (default %q)", "unknown")
	}
	// With no -ldflags injection (vanilla `go test`), version.CommitSHA
	// returns the literal "unknown" sentinel — keep callers honest.
	if sha != "unknown" {
		t.Fatalf("body[commit_sha] = %q, want %q under vanilla `go test`", sha, "unknown")
	}
}

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, addr) }()

	waitForListening(t, addr)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func TestRun_ReturnsErrorOnInvalidAddr(t *testing.T) {
	t.Parallel()
	if err := run(context.Background(), "not-a-valid-addr"); err == nil {
		t.Fatal("run with invalid addr returned nil, want error")
	}
}

func TestExecute_ReturnsZeroOnGracefulShutdown(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	getenv := func(k string) string {
		if k == "HTTP_ADDR" {
			return addr
		}
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	go func() { codeCh <- execute(ctx, getenv) }()

	waitForListening(t, addr)
	cancel()

	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("execute returned %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execute did not return after cancel")
	}
}

func TestExecute_ReturnsOneOnRunError(t *testing.T) {
	t.Parallel()
	getenv := func(string) string { return "definitely-not-an-addr" }
	if code := execute(context.Background(), getenv); code != 1 {
		t.Fatalf("execute returned %d, want 1", code)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// freeListener binds a live :0 listener on loopback and hands it back
// still open. The server-under-test (runWithListener) takes ownership and
// closes it on shutdown. Because the port is never released between bind
// and serve, there is no window for another parallel test to grab it —
// this is the TOCTOU-free replacement for freePort+runWith (SIN-65045).
func freeListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func waitForListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server did not listen on %s", addr)
}
