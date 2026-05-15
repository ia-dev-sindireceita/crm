// credentials.go — STS rotation hook for the MinIO adapter.
//
// [SIN-62819] follow-up to [SIN-62805]: the Quarantiner and Reader sign
// every request with a (AccessKeyID, SecretAccessKey, SessionToken)
// triple. In production those creds come from MinIO STS assume-role and
// expire after 1h, so the worker rotates them ~50min in. To avoid a
// "recreate the adapter to rotate" coupling, we expose Credentials +
// CredentialsProvider here and have sign() call the provider on every
// request. The adapter never blocks on STS itself — the provider is
// expected to memoize a periodic refresh.
//
// Design notes:
//
//   - The package does not implement the STS HTTP/CLI call. Production
//     wires NewFileRefresher to read a JSON triple that a sidecar (or
//     the deployment harness) rewrites on the rotation cadence. Dev
//     wires StaticProvider with envs.
//   - RotatingProvider memoizes the refresh: refresh() runs on first
//     use and again after the cached value expires. Concurrent callers
//     share the cache via a mutex; the lock is held during refresh so a
//     bursty handler does not stampede MinIO STS.
//   - Errors from refresh() flow back to the caller (Move / Open) so
//     they surface as a NATS redelivery, not a silent retry of stale
//     creds. The caller's at-least-once policy already handles transient
//     STS hiccups.
package minio

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Credentials is the SigV4 triple consumed by Quarantiner and Reader.
// SessionToken is empty when long-lived credentials are in use (only
// acceptable in local-dev — production always passes STS triples).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// IsZero reports whether c has not been populated yet. RotatingProvider
// uses this to distinguish "never refreshed" from "expired".
func (c Credentials) IsZero() bool {
	return c.AccessKeyID == "" && c.SecretAccessKey == "" && c.SessionToken == ""
}

// CredentialsProvider returns the current credentials for signing.
// Implementations MUST be cheap to call (the adapter invokes it once per
// signed request); rotation is expected to be implemented as a cached
// refresh, not a per-call STS round trip.
type CredentialsProvider func() (Credentials, error)

// StaticProvider returns a provider that always returns c. Use for dev,
// or for tests that pin a deterministic triple. The returned provider
// validates c at construction time so misconfiguration fails fast.
func StaticProvider(c Credentials) (CredentialsProvider, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, errors.New("minio: StaticProvider requires AccessKeyID + SecretAccessKey")
	}
	return func() (Credentials, error) { return c, nil }, nil
}

// RotatingProviderConfig wires a refresh-on-interval provider. Use this
// in production: refresh() is invoked on first call and again after
// interval has elapsed. The cached value covers every signing operation
// in between, so a busy worker hits MinIO STS at most once per interval.
type RotatingProviderConfig struct {
	// Refresh returns a freshly-obtained credentials triple. Production
	// implementations call MinIO STS assume-role or read a sidecar-
	// rotated file (see NewFileRefresher).
	Refresh func() (Credentials, error)

	// Interval is the cache TTL — typically the STS lifetime minus a
	// safety margin (e.g. 50m for a 60m STS triple, matching the
	// SIN-62805 runbook). The first call refreshes regardless.
	Interval time.Duration

	// Now is optional; tests pin a deterministic clock to drive the
	// expiry. Production leaves it nil (time.Now is used).
	Now func() time.Time
}

// NewRotatingProvider returns a provider whose returned Credentials are
// refreshed every cfg.Interval. The provider is goroutine-safe.
func NewRotatingProvider(cfg RotatingProviderConfig) (CredentialsProvider, error) {
	if cfg.Refresh == nil {
		return nil, errors.New("minio: NewRotatingProvider requires Refresh")
	}
	if cfg.Interval <= 0 {
		return nil, errors.New("minio: NewRotatingProvider requires positive Interval")
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	r := &rotatingProvider{
		refresh:  cfg.Refresh,
		interval: cfg.Interval,
		now:      nowFn,
	}
	return r.Get, nil
}

type rotatingProvider struct {
	mu       sync.Mutex
	refresh  func() (Credentials, error)
	interval time.Duration
	now      func() time.Time
	cached   Credentials
	expiry   time.Time
}

// Get returns the cached credentials when still valid, otherwise calls
// refresh and replaces the cache. Refresh errors bypass the cache so a
// stale triple does not leak past its expected lifetime — the caller
// observes the error and retries via NATS redelivery.
func (r *rotatingProvider) Get() (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cached.IsZero() && r.now().Before(r.expiry) {
		return r.cached, nil
	}
	c, err := r.refresh()
	if err != nil {
		return Credentials{}, fmt.Errorf("minio: credentials refresh: %w", err)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return Credentials{}, errors.New("minio: credentials refresh returned empty AccessKeyID or SecretAccessKey")
	}
	r.cached = c
	r.expiry = r.now().Add(r.interval)
	return c, nil
}

// FileCredentials matches the on-disk JSON shape NewFileRefresher
// expects. Field names mirror MinIO's `mc admin sts assume-role` JSON
// output so an operator can pipe `mc ... --output json` straight to the
// file path the worker watches.
type FileCredentials struct {
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	SessionToken string `json:"sessionToken"`
}

// NewFileRefresher returns a Refresh function that re-reads path each
// time it is invoked. Production wires this into a RotatingProvider so a
// sidecar (e.g. systemd timer or k8s CronJob) calling
// `mc admin sts assume-role ... > /etc/mediascan/creds.json` rotates the
// worker's credentials without touching the worker process.
//
// The file MUST contain a JSON object with `accessKey` and `secretKey`
// (and optionally `sessionToken`). Missing keys, unreadable files, and
// invalid JSON all surface as errors so the caller's redelivery policy
// runs again — no stale fallbacks.
func NewFileRefresher(path string) (func() (Credentials, error), error) {
	if path == "" {
		return nil, errors.New("minio: NewFileRefresher requires non-empty path")
	}
	return func() (Credentials, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Credentials{}, fmt.Errorf("read credentials file %q: %w", path, err)
		}
		var fc FileCredentials
		if err := json.Unmarshal(raw, &fc); err != nil {
			return Credentials{}, fmt.Errorf("parse credentials file %q: %w", path, err)
		}
		ak := strings.TrimSpace(fc.AccessKey)
		sk := strings.TrimSpace(fc.SecretKey)
		st := strings.TrimSpace(fc.SessionToken)
		if ak == "" || sk == "" {
			return Credentials{}, fmt.Errorf("credentials file %q is missing accessKey or secretKey", path)
		}
		return Credentials{
			AccessKeyID:     ak,
			SecretAccessKey: sk,
			SessionToken:    st,
		}, nil
	}, nil
}
