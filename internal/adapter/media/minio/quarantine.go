// Package minio is the S3-compatible adapter that implements
// quarantine.Quarantiner against a MinIO endpoint. It does NOT depend
// on the official MinIO Go SDK; the wire surface is two requests
// (CopyObject + DeleteObject) and the auth surface is AWS SigV4. Both
// fit in this package using stdlib only — keeps the boring-tech budget
// without sacrificing correctness.
//
// SigV4 reference: AWS docs "Signature Version 4 signing process". We
// implement the streaming-less variant: every request body for the two
// supported operations is empty (CopyObject puts the source via a
// header, DeleteObject has no body) so the canonical payload hash is
// always the empty-string SHA-256.
//
// Production wiring (cmd/mediascan-worker) injects credentials sourced
// from MinIO's STS assume-role flow ([SIN-62805] AC) so the long-lived
// admin credentials never reach the worker container. This adapter does
// not implement STS itself — it only consumes (AccessKeyID, SecretKey,
// SessionToken) via the Config.CredentialsProvider hook ([SIN-62819]).
// Rotation is the caller's responsibility: wire a RotatingProvider
// backed by NewFileRefresher (or another refresh func) so the triple is
// re-read every ~50min without recreating the adapter.
package minio

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/media/quarantine"
)

// emptyPayloadSHA256 is hex(SHA256("")). Pre-computed so the hot path
// does not recompute it per request.
const emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Config configures a Quarantiner against an S3-compatible endpoint.
// All fields are required except SessionToken (omitted when running
// with permanent credentials, e.g. local-dev).
type Config struct {
	// Endpoint is the base URL including scheme, e.g.
	// "http://minio:9000" or "https://s3.example.com". Path is ignored;
	// bucket + key are appended.
	Endpoint string

	// Region is the AWS-style region string. MinIO accepts any
	// value but signing requires consistency between client + server.
	// Default "us-east-1" is fine.
	Region string

	// SourceBucket is the runtime media bucket the worker reads from
	// (e.g. "media").
	SourceBucket string

	// DestinationBucket is the quarantine bucket (e.g. "media-quarantine").
	DestinationBucket string

	// AccessKeyID / SecretAccessKey / SessionToken are the static
	// credentials used when CredentialsProvider is nil. They are
	// ignored when CredentialsProvider is set so a rotating triple does
	// not collide with stale envs.
	AccessKeyID     string
	SecretAccessKey string

	// SessionToken is set when AccessKeyID/SecretAccessKey originate
	// from an STS assume-role call. Empty when long-lived credentials
	// are used (only acceptable in local dev).
	SessionToken string

	// CredentialsProvider rotates the SigV4 triple on each sign. When
	// non-nil, the static AccessKeyID/SecretAccessKey/SessionToken
	// fields are ignored. Production wires a RotatingProvider here
	// ([SIN-62819]); dev / tests can pass StaticProvider or leave nil
	// and rely on the static triple.
	CredentialsProvider CredentialsProvider

	// HTTPClient is optional; tests inject an httptest server's Client.
	// Production callers should leave it nil to use http.DefaultClient.
	HTTPClient *http.Client

	// Now is optional; tests pin a deterministic clock for SigV4
	// signing. Production leaves it nil → time.Now.
	Now func() time.Time
}

// Quarantiner is the S3-compatible adapter. Construct via New.
type Quarantiner struct {
	cfg   Config
	hc    *http.Client
	now   func() time.Time
	creds CredentialsProvider
}

var _ quarantine.Quarantiner = (*Quarantiner)(nil)

// New validates cfg and returns a Quarantiner ready for use. Returns
// an error when any required field is empty. Exactly one of (a) the
// static AccessKeyID/SecretAccessKey pair or (b) CredentialsProvider
// MUST be supplied; supplying both is rejected so an operator does not
// silently fall back to stale envs after wiring a rotating provider.
func New(cfg Config) (*Quarantiner, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("minio: Config.Endpoint is required")
	}
	if cfg.SourceBucket == "" {
		return nil, errors.New("minio: Config.SourceBucket is required")
	}
	if cfg.DestinationBucket == "" {
		return nil, errors.New("minio: Config.DestinationBucket is required")
	}
	provider, err := resolveProvider(cfg.CredentialsProvider, cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Quarantiner{cfg: cfg, hc: hc, now: nowFn, creds: provider}, nil
}

// resolveProvider centralises the "static triple or rotating provider"
// resolution used by both Quarantiner and Reader. Either source MUST be
// supplied; supplying both is rejected so a misconfigured deploy fails
// fast at startup instead of silently using stale envs after the
// rotating provider was meant to take over.
func resolveProvider(provider CredentialsProvider, ak, sk, st string) (CredentialsProvider, error) {
	if provider != nil {
		if ak != "" || sk != "" || st != "" {
			return nil, errors.New("minio: set either CredentialsProvider or static AccessKeyID/SecretAccessKey/SessionToken, not both")
		}
		return provider, nil
	}
	if ak == "" || sk == "" {
		return nil, errors.New("minio: AccessKeyID and SecretAccessKey are required when CredentialsProvider is nil")
	}
	return StaticProvider(Credentials{
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SessionToken:    st,
	})
}

// Move performs CopyObject (Source→Destination) then DeleteObject on
// the Source. Returns nil on success. Implementations of Quarantiner
// must be idempotent: a CopyObject that targets a key already present
// in the destination is still a 200 OK from S3 (overwrite), so a
// retried Move is safe. The trailing DeleteObject is also idempotent —
// S3 returns 204 on missing keys.
func (q *Quarantiner) Move(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("minio: empty key")
	}
	if err := q.copy(ctx, key); err != nil {
		return fmt.Errorf("minio: copy %q: %w", key, err)
	}
	if err := q.delete(ctx, key); err != nil {
		return fmt.Errorf("minio: delete %q: %w", key, err)
	}
	return nil
}

func (q *Quarantiner) copy(ctx context.Context, key string) error {
	target, err := q.bucketURL(q.cfg.DestinationBucket, key)
	if err != nil {
		return err
	}
	source := "/" + q.cfg.SourceBucket + "/" + urlEscapeKey(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("x-amz-copy-source", source)
	return q.do(req)
}

func (q *Quarantiner) delete(ctx context.Context, key string) error {
	target, err := q.bucketURL(q.cfg.SourceBucket, key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, http.NoBody)
	if err != nil {
		return err
	}
	return q.do(req)
}

// do signs req with SigV4 and reads the response. Non-2xx responses
// are mapped to a wrapped error including the body so log triage can
// see MinIO's <Error><Code>… payload.
func (q *Quarantiner) do(req *http.Request) error {
	if err := q.sign(req); err != nil {
		return err
	}
	resp, err := q.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// bucketURL composes Endpoint + bucket + key into a URL. urlEscapeKey
// handles slashes inside the key (which S3 treats as object-name path
// separators) without rewriting them.
func (q *Quarantiner) bucketURL(bucket, key string) (string, error) {
	base, err := url.Parse(q.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", q.cfg.Endpoint, err)
	}
	base.Path = "/" + bucket + "/" + urlEscapeKey(key)
	base.RawPath = base.Path
	return base.String(), nil
}

// urlEscapeKey escapes the key for use in a URL path. S3 keys allow
// `/` so we preserve those; everything else goes through url.PathEscape.
func urlEscapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// sign attaches the AWS SigV4 Authorization header (and the
// x-amz-date / x-amz-content-sha256 / x-amz-security-token headers it
// references) to req. The body is empty for both operations we issue.
// The credential triple is fetched from CredentialsProvider on every
// sign so STS rotation takes effect on the next request without
// recreating the adapter ([SIN-62819]).
func (q *Quarantiner) sign(req *http.Request) error {
	c, err := q.creds()
	if err != nil {
		return fmt.Errorf("minio: credentials: %w", err)
	}
	now := q.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyPayloadSHA256)
	if c.SessionToken != "" {
		req.Header.Set("x-amz-security-token", c.SessionToken)
	}

	signedHeaders, canonicalHeaders := canonicalHeaders(req.Header)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		emptyPayloadSHA256,
	}, "\n")

	credentialScope := dateStamp + "/" + q.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hexHash([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(c.SecretAccessKey, dateStamp, q.cfg.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	auth := "AWS4-HMAC-SHA256 " +
		"Credential=" + c.AccessKeyID + "/" + credentialScope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
	req.Header.Set("Authorization", auth)
	return nil
}

// canonicalHeaders produces the lowercased+sorted SignedHeaders list
// and the canonical header lines required by SigV4. Header names are
// lower-cased; multi-value headers are joined by `,` (none of the
// headers we set are multi-value, so this is conservative).
func canonicalHeaders(h http.Header) (string, string) {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, strings.ToLower(k))
	}
	// insertion sort — names is small (≤8 entries in our paths)
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	var canon bytes.Buffer
	for _, n := range names {
		vals := h.Values(http.CanonicalHeaderKey(n))
		if len(vals) == 0 {
			vals = h.Values(n)
		}
		canon.WriteString(n)
		canon.WriteByte(':')
		canon.WriteString(strings.TrimSpace(strings.Join(vals, ",")))
		canon.WriteByte('\n')
	}
	return strings.Join(names, ";"), canon.String()
}

func hexHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}
