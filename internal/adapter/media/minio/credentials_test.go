package minio_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	miniominio "github.com/pericles-luz/crm/internal/adapter/media/minio"
)

func TestStaticProvider_ReturnsValueOrErrorOnMissing(t *testing.T) {
	t.Parallel()
	p, err := miniominio.StaticProvider(miniominio.Credentials{
		AccessKeyID:     "AKIA-1",
		SecretAccessKey: "S1",
		SessionToken:    "T1",
	})
	if err != nil {
		t.Fatalf("StaticProvider: %v", err)
	}
	c, err := p()
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if c.AccessKeyID != "AKIA-1" || c.SecretAccessKey != "S1" || c.SessionToken != "T1" {
		t.Errorf("got %+v", c)
	}

	if _, err := miniominio.StaticProvider(miniominio.Credentials{AccessKeyID: "x"}); err == nil {
		t.Error("expected error when SecretAccessKey is empty")
	}
	if _, err := miniominio.StaticProvider(miniominio.Credentials{SecretAccessKey: "x"}); err == nil {
		t.Error("expected error when AccessKeyID is empty")
	}
}

func TestCredentials_IsZero(t *testing.T) {
	t.Parallel()
	if !(miniominio.Credentials{}.IsZero()) {
		t.Error("zero value should report IsZero=true")
	}
	if (miniominio.Credentials{AccessKeyID: "x"}).IsZero() {
		t.Error("populated value should report IsZero=false")
	}
}

func TestNewRotatingProvider_Validation(t *testing.T) {
	t.Parallel()
	if _, err := miniominio.NewRotatingProvider(miniominio.RotatingProviderConfig{}); err == nil {
		t.Error("expected error when Refresh is nil")
	}
	if _, err := miniominio.NewRotatingProvider(miniominio.RotatingProviderConfig{
		Refresh: func() (miniominio.Credentials, error) { return miniominio.Credentials{}, nil },
	}); err == nil {
		t.Error("expected error when Interval <= 0")
	}
}

func TestNewRotatingProvider_RefreshesOnInterval(t *testing.T) {
	t.Parallel()
	// Drive the clock manually so the cache TTL is deterministic.
	var nowMu sync.Mutex
	clock := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	now := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		defer nowMu.Unlock()
		clock = clock.Add(d)
	}

	var calls int
	refresh := func() (miniominio.Credentials, error) {
		calls++
		return miniominio.Credentials{
			AccessKeyID:     "rot-" + strings.Repeat("a", calls),
			SecretAccessKey: "s",
			SessionToken:    "t",
		}, nil
	}
	p, err := miniominio.NewRotatingProvider(miniominio.RotatingProviderConfig{
		Refresh:  refresh,
		Interval: 50 * time.Minute,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewRotatingProvider: %v", err)
	}

	c1, err := p()
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if calls != 1 || c1.AccessKeyID != "rot-a" {
		t.Errorf("first call: got calls=%d ak=%q", calls, c1.AccessKeyID)
	}

	// Within TTL: cached.
	advance(10 * time.Minute)
	c2, err := p()
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if calls != 1 || c2.AccessKeyID != "rot-a" {
		t.Errorf("cached call: got calls=%d ak=%q", calls, c2.AccessKeyID)
	}

	// After TTL: refresh.
	advance(50 * time.Minute)
	c3, err := p()
	if err != nil {
		t.Fatalf("call 3: %v", err)
	}
	if calls != 2 || c3.AccessKeyID != "rot-aa" {
		t.Errorf("expired call: got calls=%d ak=%q", calls, c3.AccessKeyID)
	}
}

func TestNewRotatingProvider_RefreshErrorBypassesCache(t *testing.T) {
	t.Parallel()
	refreshErr := errors.New("sts down")
	p, err := miniominio.NewRotatingProvider(miniominio.RotatingProviderConfig{
		Refresh: func() (miniominio.Credentials, error) {
			return miniominio.Credentials{}, refreshErr
		},
		Interval: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewRotatingProvider: %v", err)
	}
	_, err = p()
	if !errors.Is(err, refreshErr) {
		t.Fatalf("expected wrapped refresh error, got %v", err)
	}
}

func TestNewRotatingProvider_RefreshEmptyTripleIsError(t *testing.T) {
	t.Parallel()
	p, err := miniominio.NewRotatingProvider(miniominio.RotatingProviderConfig{
		Refresh: func() (miniominio.Credentials, error) {
			return miniominio.Credentials{SessionToken: "only-the-session"}, nil
		},
		Interval: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewRotatingProvider: %v", err)
	}
	if _, err := p(); err == nil {
		t.Fatal("expected error when refresh returned empty AccessKeyID/SecretAccessKey")
	}
}

func TestNewFileRefresher_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(path, []byte(`{
		"accessKey": "AKIA-FROM-FILE",
		"secretKey": "SECRET-FROM-FILE",
		"sessionToken": "TOKEN-FROM-FILE"
	}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	refresh, err := miniominio.NewFileRefresher(path)
	if err != nil {
		t.Fatalf("NewFileRefresher: %v", err)
	}
	c, err := refresh()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if c.AccessKeyID != "AKIA-FROM-FILE" || c.SecretAccessKey != "SECRET-FROM-FILE" || c.SessionToken != "TOKEN-FROM-FILE" {
		t.Fatalf("got %+v", c)
	}
}

func TestNewFileRefresher_ReReadsFileEachCall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	write := func(ak, sk, st string) {
		if err := os.WriteFile(path, []byte(`{"accessKey":"`+ak+`","secretKey":"`+sk+`","sessionToken":"`+st+`"}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write("AK1", "SK1", "ST1")
	refresh, err := miniominio.NewFileRefresher(path)
	if err != nil {
		t.Fatalf("NewFileRefresher: %v", err)
	}
	c, err := refresh()
	if err != nil {
		t.Fatalf("refresh 1: %v", err)
	}
	if c.AccessKeyID != "AK1" {
		t.Fatalf("first read: got %q", c.AccessKeyID)
	}
	write("AK2", "SK2", "ST2")
	c, err = refresh()
	if err != nil {
		t.Fatalf("refresh 2: %v", err)
	}
	if c.AccessKeyID != "AK2" {
		t.Fatalf("second read: got %q", c.AccessKeyID)
	}
}

func TestNewFileRefresher_Errors(t *testing.T) {
	t.Parallel()
	if _, err := miniominio.NewFileRefresher(""); err == nil {
		t.Error("empty path should be rejected")
	}

	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.json")
	refresh, err := miniominio.NewFileRefresher(missing)
	if err != nil {
		t.Fatalf("NewFileRefresher: %v", err)
	}
	if _, err := refresh(); err == nil {
		t.Error("expected error when file is missing")
	}

	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("not json"), 0o600); err != nil {
		t.Fatalf("seed bad: %v", err)
	}
	refresh, err = miniominio.NewFileRefresher(badJSON)
	if err != nil {
		t.Fatalf("NewFileRefresher: %v", err)
	}
	if _, err := refresh(); err == nil {
		t.Error("expected error on bad JSON")
	}

	missingFields := filepath.Join(dir, "missing-fields.json")
	if err := os.WriteFile(missingFields, []byte(`{"accessKey":"only-ak"}`), 0o600); err != nil {
		t.Fatalf("seed missing-fields: %v", err)
	}
	refresh, err = miniominio.NewFileRefresher(missingFields)
	if err != nil {
		t.Fatalf("NewFileRefresher: %v", err)
	}
	if _, err := refresh(); err == nil {
		t.Error("expected error when secretKey is missing")
	}
}

func TestQuarantinerNew_RejectsBothProviderAndStaticTriple(t *testing.T) {
	t.Parallel()
	provider, err := miniominio.StaticProvider(miniominio.Credentials{
		AccessKeyID:     "from-provider",
		SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("StaticProvider: %v", err)
	}
	_, err = miniominio.New(miniominio.Config{
		Endpoint:            "http://x",
		SourceBucket:        "media",
		DestinationBucket:   "media-quarantine",
		AccessKeyID:         "stale-static",
		SecretAccessKey:     "stale-secret",
		CredentialsProvider: provider,
	})
	if err == nil {
		t.Fatal("expected error when both provider and static triple are set")
	}
}

func TestQuarantinerNew_AcceptsCredentialsProviderAlone(t *testing.T) {
	t.Parallel()
	provider, err := miniominio.StaticProvider(miniominio.Credentials{
		AccessKeyID:     "from-provider",
		SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("StaticProvider: %v", err)
	}
	if _, err := miniominio.New(miniominio.Config{
		Endpoint:            "http://x",
		SourceBucket:        "media",
		DestinationBucket:   "media-quarantine",
		CredentialsProvider: provider,
	}); err != nil {
		t.Fatalf("New with provider only: %v", err)
	}
}

func TestQuarantiner_SignUsesProviderEveryCall(t *testing.T) {
	t.Parallel()
	// Two distinct triples; provider returns the next one on each call.
	var idx int
	var mu sync.Mutex
	triples := []miniominio.Credentials{
		{AccessKeyID: "AK-OLD", SecretAccessKey: "S-OLD", SessionToken: "T-OLD"},
		{AccessKeyID: "AK-NEW", SecretAccessKey: "S-NEW", SessionToken: "T-NEW"},
	}
	provider := func() (miniominio.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		c := triples[idx]
		idx++
		if idx >= len(triples) {
			idx = len(triples) - 1
		}
		return c, nil
	}

	var seen []string
	var seenMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seen = append(seen, r.Header.Get("x-amz-security-token"))
		seenMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	q, err := miniominio.New(miniominio.Config{
		Endpoint:            srv.URL,
		Region:              "us-east-1",
		SourceBucket:        "media",
		DestinationBucket:   "media-quarantine",
		CredentialsProvider: provider,
		HTTPClient:          srv.Client(),
		Now:                 func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := q.Move(context.Background(), "tenant/k.png"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 signed requests, got %d", len(seen))
	}
	if seen[0] != "T-OLD" {
		t.Errorf("first request used token %q, want T-OLD", seen[0])
	}
	if seen[1] != "T-NEW" {
		t.Errorf("second request used token %q, want T-NEW (provider should rotate per sign)", seen[1])
	}
}

func TestQuarantiner_ProviderErrorSurfaces(t *testing.T) {
	t.Parallel()
	want := errors.New("sts down")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be reached when credentials fail")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	q, err := miniominio.New(miniominio.Config{
		Endpoint:            srv.URL,
		SourceBucket:        "media",
		DestinationBucket:   "media-quarantine",
		CredentialsProvider: func() (miniominio.Credentials, error) { return miniominio.Credentials{}, want },
		HTTPClient:          srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = q.Move(context.Background(), "tenant/k.png")
	if err == nil {
		t.Fatal("expected provider error to surface from Move")
	}
	if !errors.Is(err, want) {
		t.Fatalf("error %v should wrap %v", err, want)
	}
}

func TestReaderNew_RejectsBothProviderAndStaticTriple(t *testing.T) {
	t.Parallel()
	provider, err := miniominio.StaticProvider(miniominio.Credentials{
		AccessKeyID:     "p",
		SecretAccessKey: "p",
	})
	if err != nil {
		t.Fatalf("StaticProvider: %v", err)
	}
	if _, err := miniominio.NewReader(miniominio.ReaderConfig{
		Endpoint:            "http://x",
		Bucket:              "media",
		AccessKeyID:         "stale",
		SecretAccessKey:     "stale",
		CredentialsProvider: provider,
	}); err == nil {
		t.Fatal("expected error when both provider and static triple set on ReaderConfig")
	}
}

func TestReader_SignUsesProviderEveryCall(t *testing.T) {
	t.Parallel()
	var idx int
	var mu sync.Mutex
	triples := []miniominio.Credentials{
		{AccessKeyID: "RAK-OLD", SecretAccessKey: "RSK-OLD", SessionToken: "RTK-OLD"},
		{AccessKeyID: "RAK-NEW", SecretAccessKey: "RSK-NEW", SessionToken: "RTK-NEW"},
	}
	provider := func() (miniominio.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		c := triples[idx]
		if idx < len(triples)-1 {
			idx++
		}
		return c, nil
	}

	var seenTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTokens = append(seenTokens, r.Header.Get("x-amz-security-token"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	r, err := miniominio.NewReader(miniominio.ReaderConfig{
		Endpoint:            srv.URL,
		Bucket:              "media",
		CredentialsProvider: provider,
		HTTPClient:          srv.Client(),
		Now:                 func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	for i := 0; i < 2; i++ {
		rc, err := r.Open(context.Background(), "tenant/k.bin")
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		rc.Close()
	}
	if len(seenTokens) != 2 || seenTokens[0] == seenTokens[1] {
		t.Fatalf("expected two distinct session tokens, got %v", seenTokens)
	}
}

func TestReader_ProviderErrorSurfaces(t *testing.T) {
	t.Parallel()
	want := errors.New("file gone")
	r, err := miniominio.NewReader(miniominio.ReaderConfig{
		Endpoint: "http://127.0.0.1:1",
		Bucket:   "media",
		CredentialsProvider: func() (miniominio.Credentials, error) {
			return miniominio.Credentials{}, want
		},
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	_, err = r.Open(context.Background(), "k")
	if err == nil {
		t.Fatal("expected provider error to surface from Open")
	}
	if !errors.Is(err, want) {
		t.Fatalf("error %v should wrap %v", err, want)
	}
}
