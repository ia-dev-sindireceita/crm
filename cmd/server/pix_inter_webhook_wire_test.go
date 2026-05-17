package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	goredis "github.com/redis/go-redis/v9"
)

// fakeTxBeginnerPool is the minimal stub satisfying pixInterWebhookPool
// for buildPixInterWebhookWiringWithDeps: it must satisfy
// postgres.TxBeginner (BeginTx) AND Close().
type fakeTxBeginnerPool struct {
	closed bool
}

func (f *fakeTxBeginnerPool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("not used in wire-up tests")
}
func (f *fakeTxBeginnerPool) Close() { f.closed = true }

func fakeDial(*fakeTxBeginnerPool) pixInterWebhookDial {
	return func(_ context.Context, _ string) (pixInterWebhookPool, error) {
		return &fakeTxBeginnerPool{}, nil
	}
}

func fakeRedisDial() pixInterWebhookRedisDial {
	return func(_ string) (*goredis.Client, error) {
		// goredis.NewClient does not dial until first use; this gives
		// the wire a non-nil *goredis.Client without standing up a real
		// Redis. Tests below never trigger a Redis call so Ping is
		// never run.
		return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:0"}), nil
	}
}

// envMap returns a getenv function backed by a map literal.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestBuildPixInterWebhookWiring_DisabledByDefault(t *testing.T) {
	getenv := envMap(nil)
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, fakeDial(nil), fakeRedisDial()); w != nil {
		t.Errorf("expected nil wiring when PIX_INTER_WEBHOOK_ENABLED unset, got %+v", w)
	}
}

func TestBuildPixInterWebhookWiring_RequiresSecret(t *testing.T) {
	getenv := envMap(map[string]string{envPixInterWebhookEnabled: "1"})
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, fakeDial(nil), fakeRedisDial()); w != nil {
		t.Errorf("expected nil wiring when secret unset, got %+v", w)
	}
}

func TestBuildPixInterWebhookWiring_RequiresActor(t *testing.T) {
	getenv := envMap(map[string]string{
		envPixInterWebhookEnabled: "1",
		envPixInterWebhookSecret:  "s",
	})
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, fakeDial(nil), fakeRedisDial()); w != nil {
		t.Errorf("expected nil wiring when actor unset, got %+v", w)
	}
}

func TestBuildPixInterWebhookWiring_RequiresValidActor(t *testing.T) {
	getenv := envMap(map[string]string{
		envPixInterWebhookEnabled: "1",
		envPixInterWebhookSecret:  "s",
		envPixInterWebhookActor:   "not-a-uuid",
	})
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, fakeDial(nil), fakeRedisDial()); w != nil {
		t.Errorf("expected nil wiring when actor is not a uuid, got %+v", w)
	}
}

func TestBuildPixInterWebhookWiring_RequiresDSN(t *testing.T) {
	getenv := envMap(map[string]string{
		envPixInterWebhookEnabled: "1",
		envPixInterWebhookSecret:  "s",
		envPixInterWebhookActor:   uuid.NewString(),
	})
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, fakeDial(nil), fakeRedisDial()); w != nil {
		t.Errorf("expected nil wiring when DATABASE_URL unset, got %+v", w)
	}
}

func TestBuildPixInterWebhookWiring_RequiresRedis(t *testing.T) {
	getenv := envMap(map[string]string{
		envPixInterWebhookEnabled: "1",
		envPixInterWebhookSecret:  "s",
		envPixInterWebhookActor:   uuid.NewString(),
		"DATABASE_URL":            "postgres://stub",
	})
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, fakeDial(nil), fakeRedisDial()); w != nil {
		t.Errorf("expected nil wiring when REDIS_URL unset, got %+v", w)
	}
}

func TestBuildPixInterWebhookWiring_PoolDialFails(t *testing.T) {
	getenv := envMap(map[string]string{
		envPixInterWebhookEnabled: "1",
		envPixInterWebhookSecret:  "s",
		envPixInterWebhookActor:   uuid.NewString(),
		"DATABASE_URL":            "postgres://stub",
		envRedisURL:               "redis://127.0.0.1:6379/0",
	})
	failingDial := func(context.Context, string) (pixInterWebhookPool, error) {
		return nil, errors.New("pg unavailable")
	}
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, failingDial, fakeRedisDial()); w != nil {
		t.Errorf("expected nil wiring when pg dial fails, got %+v", w)
	}
}

func TestBuildPixInterWebhookWiring_RedisDialFails(t *testing.T) {
	getenv := envMap(map[string]string{
		envPixInterWebhookEnabled: "1",
		envPixInterWebhookSecret:  "s",
		envPixInterWebhookActor:   uuid.NewString(),
		"DATABASE_URL":            "postgres://stub",
		envRedisURL:               "redis://127.0.0.1:6379/0",
	})
	pool := &fakeTxBeginnerPool{}
	dial := func(context.Context, string) (pixInterWebhookPool, error) { return pool, nil }
	failingRedis := func(string) (*goredis.Client, error) { return nil, errors.New("redis down") }
	if w := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, dial, failingRedis); w != nil {
		t.Errorf("expected nil wiring when redis dial fails, got %+v", w)
	}
	if !pool.closed {
		t.Errorf("pool should have been closed after redis dial failure")
	}
}

func TestBuildPixInterWebhookWiring_HappyPath_MountsRoute(t *testing.T) {
	getenv := envMap(map[string]string{
		envPixInterWebhookEnabled: "1",
		envPixInterWebhookSecret:  "topsecret",
		envPixInterWebhookActor:   uuid.NewString(),
		"DATABASE_URL":            "postgres://stub",
		envRedisURL:               "redis://127.0.0.1:6379/0",
	})
	wiring := buildPixInterWebhookWiringWithDeps(context.Background(), getenv, fakeDial(nil), fakeRedisDial())
	if wiring == nil {
		t.Fatal("expected non-nil wiring on happy path")
	}
	defer wiring.Cleanup()

	mux := http.NewServeMux()
	wiring.Register(mux)

	// Sanity: the route is mounted (a POST hits the handler — we
	// expect 401 because no signature header is supplied; importantly,
	// it is NOT the 404 we would get if Register were a no-op).
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pix/inter", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusNotFound {
		t.Fatalf("status = 404, expected route to be mounted")
	}
}

func TestParsePixInterAllowedCIDRs_DefaultsWhenUnset(t *testing.T) {
	got := parsePixInterAllowedCIDRs(envMap(nil))
	if len(got) == 0 {
		t.Error("expected default allowlist to be non-empty")
	}
}

func TestParsePixInterAllowedCIDRs_OverrideRespected(t *testing.T) {
	got := parsePixInterAllowedCIDRs(envMap(map[string]string{
		envPixInterWebhookAllowlist: "10.0.0.0/8,192.168.1.0/24",
	}))
	if len(got) != 2 {
		t.Errorf("expected 2 cidrs, got %d", len(got))
	}
}

func TestParsePixInterAllowedCIDRs_AllInvalidFallsBackToDefault(t *testing.T) {
	got := parsePixInterAllowedCIDRs(envMap(map[string]string{
		envPixInterWebhookAllowlist: "bad,also-bad",
	}))
	if len(got) == 0 {
		t.Error("expected fallback to defaults when override drained to empty")
	}
}

func TestPixInterIPCheckDisabled(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"1", false},
		{"true", false},
		{"0", true},
		{"false", true},
		{"OFF", true},
		{"no", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := pixInterIPCheckDisabled(envMap(map[string]string{envPixInterWebhookIPCheck: tc.raw}))
			if got != tc.want {
				t.Errorf("pixInterIPCheckDisabled(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestReadPositiveInt(t *testing.T) {
	cases := []struct {
		raw      string
		fallback int
		want     int
	}{
		{"", 100, 100},
		{"50", 100, 50},
		{"-5", 100, 100},
		{"junk", 100, 100},
		{"0", 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := readPositiveInt(tc.raw, tc.fallback)
			if got != tc.want {
				t.Errorf("readPositiveInt(%q, %d) = %d, want %d", tc.raw, tc.fallback, got, tc.want)
			}
		})
	}
}
