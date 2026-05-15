package minio_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	miniominio "github.com/pericles-luz/crm/internal/adapter/media/minio"
)

func readerCfg(srv *httptest.Server) miniominio.ReaderConfig {
	return miniominio.ReaderConfig{
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		Bucket:          "media",
		AccessKeyID:     "AKIA-test",
		SecretAccessKey: "secret-test",
		SessionToken:    "session-test",
		HTTPClient:      srv.Client(),
		Now:             func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) },
	}
}

func TestNewReader_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  miniominio.ReaderConfig
	}{
		{"missing endpoint", miniominio.ReaderConfig{Bucket: "media", AccessKeyID: "k", SecretAccessKey: "s"}},
		{"missing bucket", miniominio.ReaderConfig{Endpoint: "http://x", AccessKeyID: "k", SecretAccessKey: "s"}},
		{"missing creds", miniominio.ReaderConfig{Endpoint: "http://x", Bucket: "media"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := miniominio.NewReader(tc.cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestReader_Open_ReadsBlobWithSignedGET(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth, gotAmzDate, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAmzDate = r.Header.Get("x-amz-date")
		gotToken = r.Header.Get("x-amz-security-token")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello eicar"))
	}))
	defer srv.Close()

	r, err := miniominio.NewReader(readerCfg(srv))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	rc, err := r.Open(context.Background(), "t1/2026-05/abc.bin")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "hello eicar" {
		t.Fatalf("body: got %q", string(body))
	}
	if gotMethod != "GET" {
		t.Fatalf("method: got %q want GET", gotMethod)
	}
	if gotPath != "/media/t1/2026-05/abc.bin" {
		t.Fatalf("path: got %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization: got %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "Credential=AKIA-test/") {
		t.Fatalf("Credential missing: %q", gotAuth)
	}
	if gotAmzDate != "20260515T120000Z" {
		t.Fatalf("x-amz-date: got %q", gotAmzDate)
	}
	if gotToken != "session-test" {
		t.Fatalf("x-amz-security-token: got %q", gotToken)
	}
}

func TestReader_Open_PropagatesNon2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
	}))
	defer srv.Close()
	r, err := miniominio.NewReader(readerCfg(srv))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	_, err = r.Open(context.Background(), "t1/x.bin")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("err: got %v", err)
	}
}

func TestReader_Open_EmptyKey(t *testing.T) {
	t.Parallel()
	r, err := miniominio.NewReader(miniominio.ReaderConfig{
		Endpoint:        "http://x",
		Bucket:          "media",
		AccessKeyID:     "k",
		SecretAccessKey: "s",
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Open(context.Background(), ""); err == nil {
		t.Fatalf("expected error for empty key")
	}
}
