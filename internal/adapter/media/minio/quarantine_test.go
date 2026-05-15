package minio_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	miniominio "github.com/pericles-luz/crm/internal/adapter/media/minio"
)

// testCfg builds a Config wired to srv (httptest server) with pinned
// time, deterministic credentials, and a session token so the sign +
// header surface is exercised.
func testCfg(srv *httptest.Server) miniominio.Config {
	return miniominio.Config{
		Endpoint:          srv.URL,
		Region:            "us-east-1",
		SourceBucket:      "media",
		DestinationBucket: "media-quarantine",
		AccessKeyID:       "AKIA-test",
		SecretAccessKey:   "secret-test",
		SessionToken:      "session-test",
		HTTPClient:        srv.Client(),
		Now:               func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) },
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  miniominio.Config
	}{
		{"missing endpoint", miniominio.Config{SourceBucket: "a", DestinationBucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}},
		{"missing source", miniominio.Config{Endpoint: "http://x", DestinationBucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}},
		{"missing destination", miniominio.Config{Endpoint: "http://x", SourceBucket: "a", AccessKeyID: "k", SecretAccessKey: "s"}},
		{"missing creds", miniominio.Config{Endpoint: "http://x", SourceBucket: "a", DestinationBucket: "b"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := miniominio.New(tc.cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNew_DefaultsRegionAndClient(t *testing.T) {
	t.Parallel()
	q, err := miniominio.New(miniominio.Config{
		Endpoint:          "http://x",
		SourceBucket:      "media",
		DestinationBucket: "media-quarantine",
		AccessKeyID:       "k",
		SecretAccessKey:   "s",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if q == nil {
		t.Fatal("expected non-nil Quarantiner")
	}
}

// TestMove_CallsCopyThenDelete is the primary happy-path test: it
// verifies the adapter issues exactly two requests, in order, with
// well-formed SigV4 headers and the expected URLs/method/body shape.
func TestMove_CallsCopyThenDelete(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var requests []*http.Request

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		clone := r.Clone(context.Background())
		clone.Body = io.NopCloser(strings.NewReader(string(body)))
		mu.Lock()
		requests = append(requests, clone)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	q, err := miniominio.New(testCfg(srv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := q.Move(context.Background(), "tenant/2026-05/abc.png"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests (copy + delete), got %d", len(requests))
	}

	cp := requests[0]
	if cp.Method != http.MethodPut {
		t.Errorf("copy method = %s, want PUT", cp.Method)
	}
	if cp.URL.Path != "/media-quarantine/tenant/2026-05/abc.png" {
		t.Errorf("copy path = %s", cp.URL.Path)
	}
	if cp.Header.Get("x-amz-copy-source") != "/media/tenant/2026-05/abc.png" {
		t.Errorf("copy source header = %q", cp.Header.Get("x-amz-copy-source"))
	}
	if cp.Header.Get("x-amz-content-sha256") == "" {
		t.Error("missing x-amz-content-sha256")
	}
	if !strings.HasPrefix(cp.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIA-test/") {
		t.Errorf("unexpected Authorization: %s", cp.Header.Get("Authorization"))
	}
	if cp.Header.Get("x-amz-security-token") != "session-test" {
		t.Errorf("missing x-amz-security-token")
	}

	del := requests[1]
	if del.Method != http.MethodDelete {
		t.Errorf("delete method = %s, want DELETE", del.Method)
	}
	if del.URL.Path != "/media/tenant/2026-05/abc.png" {
		t.Errorf("delete path = %s", del.URL.Path)
	}
}

func TestMove_EscapesSpecialCharsInKey(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	q, err := miniominio.New(testCfg(srv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := q.Move(context.Background(), "tenant/some path/file name.png"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	for _, p := range paths {
		if !strings.Contains(p, "some%20path/file%20name.png") {
			t.Errorf("expected escaped path, got %q", p)
		}
	}
}

func TestMove_EmptyKeyIsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be hit")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	q, err := miniominio.New(testCfg(srv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := q.Move(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestMove_CopyErrorSurfaces(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
	}))
	defer srv.Close()
	q, err := miniominio.New(testCfg(srv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = q.Move(context.Background(), "tenant/k.png")
	if err == nil {
		t.Fatal("expected error from copy")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("error should include MinIO body, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected delete to be skipped, got %d total calls", calls)
	}
}

func TestMove_DeleteErrorSurfaces(t *testing.T) {
	t.Parallel()
	state := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state++
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	q, err := miniominio.New(testCfg(srv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := q.Move(context.Background(), "tenant/k.png"); err == nil {
		t.Fatal("expected error from delete")
	}
	if state != 2 {
		t.Errorf("expected copy then delete, total calls = %d", state)
	}
}

func TestMove_HonoursContextCancel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// never reached after ctx is cancelled, but keep it well-behaved
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	q, err := miniominio.New(testCfg(srv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Move(ctx, "tenant/k.png"); err == nil {
		t.Fatal("expected error when ctx cancelled before dispatch")
	}
}

func TestMove_SignatureIsDeterministic(t *testing.T) {
	t.Parallel()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			got = r.Header.Get("Authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	q, err := miniominio.New(testCfg(srv))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := q.Move(context.Background(), "tenant/key.png"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=AKIA-test/20260515/us-east-1/s3/aws4_request,") {
		t.Errorf("unexpected credential scope: %q", got)
	}
	if !strings.Contains(got, "SignedHeaders=") || !strings.Contains(got, "Signature=") {
		t.Errorf("missing SigV4 fields: %q", got)
	}
}
