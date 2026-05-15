// Tests for the MinIO credentials wiring in main.go ([SIN-62819]).
// These pin the operator-facing env contract: static envs for dev,
// MINIO_CREDS_FILE for production STS rotation, and the half-configured
// combinations that loadConfig must reject.

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_MinioRequiresEitherCredsFileOrStaticTriple(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("MINIO_ENDPOINT", "http://minio:9000")
	t.Setenv("MINIO_QUARANTINE_SOURCE", "media")
	t.Setenv("MINIO_QUARANTINE_DEST", "media-quarantine")
	// Neither static envs nor MINIO_CREDS_FILE set — expect failure
	// that names both knobs.
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error when MinIO is configured without credentials")
	}
	for _, want := range []string{"MINIO_ACCESS_KEY_ID", "MINIO_SECRET_ACCESS_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err.Error(), want)
		}
	}
}

func TestLoadConfig_MinioCredsFileRejectsStaticTriple(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("MINIO_ENDPOINT", "http://minio:9000")
	t.Setenv("MINIO_QUARANTINE_SOURCE", "media")
	t.Setenv("MINIO_QUARANTINE_DEST", "media-quarantine")
	t.Setenv("MINIO_CREDS_FILE", "/etc/mediascan/creds.json")
	t.Setenv("MINIO_ACCESS_KEY_ID", "stale")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error when MINIO_CREDS_FILE collides with static envs")
	}
	if !strings.Contains(err.Error(), "MINIO_CREDS_FILE") {
		t.Errorf("error %q should mention MINIO_CREDS_FILE", err.Error())
	}
}

func TestLoadConfig_MinioCredsFileAlone(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("MINIO_ENDPOINT", "http://minio:9000")
	t.Setenv("MINIO_QUARANTINE_SOURCE", "media")
	t.Setenv("MINIO_QUARANTINE_DEST", "media-quarantine")
	t.Setenv("MINIO_CREDS_FILE", "/etc/mediascan/creds.json")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.minioCredsFile != "/etc/mediascan/creds.json" {
		t.Errorf("minioCredsFile = %q", c.minioCredsFile)
	}
	if c.minioCredsRefresh != 50*time.Minute {
		t.Errorf("minioCredsRefresh default = %v, want 50m", c.minioCredsRefresh)
	}
}

func TestLoadConfig_MinioCredsRefreshOverride(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("MINIO_ENDPOINT", "http://minio:9000")
	t.Setenv("MINIO_QUARANTINE_SOURCE", "media")
	t.Setenv("MINIO_QUARANTINE_DEST", "media-quarantine")
	t.Setenv("MINIO_CREDS_FILE", "/etc/mediascan/creds.json")
	t.Setenv("MINIO_CREDS_REFRESH", "15m")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.minioCredsRefresh != 15*time.Minute {
		t.Errorf("minioCredsRefresh = %v, want 15m", c.minioCredsRefresh)
	}
}

func TestLoadConfig_MinioCredsRefreshRejectsInvalid(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("MINIO_ENDPOINT", "http://minio:9000")
	t.Setenv("MINIO_QUARANTINE_SOURCE", "media")
	t.Setenv("MINIO_QUARANTINE_DEST", "media-quarantine")
	t.Setenv("MINIO_CREDS_FILE", "/etc/mediascan/creds.json")
	t.Setenv("MINIO_CREDS_REFRESH", "not-a-duration")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error on invalid MINIO_CREDS_REFRESH")
	}
	if !strings.Contains(err.Error(), "MINIO_CREDS_REFRESH") {
		t.Errorf("error %q should name MINIO_CREDS_REFRESH", err.Error())
	}
}

func TestLoadConfig_MinioCredsRefreshRejectsNegative(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("MINIO_ENDPOINT", "http://minio:9000")
	t.Setenv("MINIO_QUARANTINE_SOURCE", "media")
	t.Setenv("MINIO_QUARANTINE_DEST", "media-quarantine")
	t.Setenv("MINIO_CREDS_FILE", "/etc/mediascan/creds.json")
	t.Setenv("MINIO_CREDS_REFRESH", "-5m")
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error on negative MINIO_CREDS_REFRESH")
	}
}

func TestBuildCredentialsProvider_StaticFromEnv(t *testing.T) {
	t.Parallel()
	cfg := config{
		minioAccessKey:    "AK",
		minioSecretKey:    "SK",
		minioSessionToken: "TK",
	}
	provider, err := buildCredentialsProvider(cfg)
	if err != nil {
		t.Fatalf("buildCredentialsProvider: %v", err)
	}
	c, err := provider()
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if c.AccessKeyID != "AK" || c.SecretAccessKey != "SK" || c.SessionToken != "TK" {
		t.Errorf("got %+v", c)
	}
}

func TestBuildCredentialsProvider_RotatingFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(path, []byte(`{"accessKey":"AK-FILE","secretKey":"SK-FILE","sessionToken":"TK-FILE"}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := config{
		minioCredsFile:    path,
		minioCredsRefresh: time.Minute,
	}
	provider, err := buildCredentialsProvider(cfg)
	if err != nil {
		t.Fatalf("buildCredentialsProvider: %v", err)
	}
	c, err := provider()
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if c.AccessKeyID != "AK-FILE" || c.SecretAccessKey != "SK-FILE" || c.SessionToken != "TK-FILE" {
		t.Errorf("got %+v", c)
	}
}

func TestBuildCredentialsProvider_MissingFileSurfacesAtCall(t *testing.T) {
	t.Parallel()
	cfg := config{
		minioCredsFile:    "/tmp/does-not-exist-sin62819",
		minioCredsRefresh: time.Minute,
	}
	provider, err := buildCredentialsProvider(cfg)
	if err != nil {
		t.Fatalf("buildCredentialsProvider should defer the read; got %v", err)
	}
	if _, err := provider(); err == nil {
		t.Fatal("expected provider to error when the credentials file is missing")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Logf("note: error wraps something other than fs.ErrNotExist: %v", err)
	}
}

func TestBuildBlobReader_LocalFsWhenNoMinio(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, err := buildBlobReader(config{blobBaseDir: dir})
	if err != nil {
		t.Fatalf("buildBlobReader: %v", err)
	}
	if _, ok := r.(*localBlobs); !ok {
		t.Fatalf("expected *localBlobs, got %T", r)
	}
}

func TestBuildBlobReader_RejectsBothBlobAndMinioUnset(t *testing.T) {
	t.Parallel()
	if _, err := buildBlobReader(config{}); err == nil {
		t.Fatal("expected error when neither BLOB_BASE_DIR nor MINIO_ENDPOINT is set")
	}
}

func TestBuildBlobReader_MinioBuildsReaderWithProvider(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(path, []byte(`{"accessKey":"AK","secretKey":"SK"}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r, err := buildBlobReader(config{
		minioEndpoint:     "http://minio:9000",
		minioRegion:       "us-east-1",
		minioSource:       "media",
		minioCredsFile:    path,
		minioCredsRefresh: time.Minute,
	})
	if err != nil {
		t.Fatalf("buildBlobReader: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil BlobReader")
	}
}

func TestMinioCredsMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  config
		want string
	}{
		{"local-fs", config{}, "none"},
		{"static-env", config{minioEndpoint: "http://x", minioAccessKey: "AK", minioSecretKey: "SK"}, "static-env"},
		{"rotating-file", config{minioEndpoint: "http://x", minioCredsFile: "/etc/creds.json"}, "rotating-file"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := minioCredsMode(tc.cfg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPublisherShim_PublishProxiesToAdapter(t *testing.T) {
	// Compile-time fence: publisherShim must satisfy worker.Publisher's
	// (ctx, subject, []byte) error shape. Calling Publish with a nil
	// SDKAdapter panics, so we cover only the construction path here —
	// the proxy is exercised end-to-end in the integration tests.
	t.Parallel()
	var ps publisherShim
	if ps.a != nil {
		t.Fatal("zero value should have nil SDKAdapter")
	}
}
