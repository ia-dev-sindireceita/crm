// reader.go — S3-compatible BlobReader for the clamav adapter.
//
// The mediascan worker reads the runtime media object via this adapter
// so the scanner pipeline never touches the local filesystem in
// production. The Reader signs a GET with the same SigV4 routine as the
// Quarantiner above (the helpers live in quarantine.go and are reused
// here), so the wire surface stays stdlib-only and the worker keeps its
// hexagonal boundary clean — no MinIO SDK in cmd/mediascan-worker.
package minio

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ReaderConfig configures a BlobReader against an S3-compatible
// endpoint. Either the static AccessKeyID/SecretAccessKey pair OR the
// CredentialsProvider hook MUST be provided; supplying both is rejected
// at NewReader.
type ReaderConfig struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// CredentialsProvider rotates the SigV4 triple on each request.
	// Production wires a RotatingProvider here so STS-issued creds
	// expire and rotate cleanly without recreating the Reader. When
	// nil, the static AccessKeyID/SecretAccessKey/SessionToken fields
	// are required (dev / tests).
	CredentialsProvider CredentialsProvider

	HTTPClient *http.Client
	Now        func() time.Time
}

// Reader implements clamav.BlobReader against an S3-compatible endpoint
// (MinIO in production). Concrete construction lives in NewReader.
type Reader struct {
	cfg   ReaderConfig
	hc    *http.Client
	now   func() time.Time
	creds CredentialsProvider
}

// NewReader validates cfg and returns a Reader ready for use.
func NewReader(cfg ReaderConfig) (*Reader, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("minio: ReaderConfig.Endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("minio: ReaderConfig.Bucket is required")
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
	return &Reader{cfg: cfg, hc: hc, now: nowFn, creds: provider}, nil
}

// Open issues a signed GET against Endpoint/Bucket/key and returns an
// io.ReadCloser over the response body. Non-2xx responses surface as an
// error after the body is drained so the worker can retry per its own
// policy. The caller MUST close the returned ReadCloser.
func (r *Reader) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("minio: empty key")
	}
	target, err := r.bucketURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, err
	}
	if err := r.sign(req); err != nil {
		return nil, err
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("minio: get %q status %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

func (r *Reader) bucketURL(key string) (string, error) {
	base, err := url.Parse(r.cfg.Endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", r.cfg.Endpoint, err)
	}
	base.Path = "/" + r.cfg.Bucket + "/" + urlEscapeKey(key)
	base.RawPath = base.Path
	return base.String(), nil
}

// sign mirrors Quarantiner.sign — both use SigV4 over an empty body. We
// duplicate the small wrapper rather than coupling the two structs so a
// future change to one signing path (e.g. presigned URLs for Reader)
// does not entangle the Quarantiner's CopyObject/DeleteObject signatures.
// Like Quarantiner.sign, the credential triple comes from
// CredentialsProvider on every call so STS rotation propagates without
// recreating the Reader ([SIN-62819]).
func (r *Reader) sign(req *http.Request) error {
	c, err := r.creds()
	if err != nil {
		return fmt.Errorf("minio: credentials: %w", err)
	}
	now := r.now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyPayloadSHA256)
	if c.SessionToken != "" {
		req.Header.Set("x-amz-security-token", c.SessionToken)
	}

	signedHeaders, canonicalHeaderBlock := canonicalHeaders(req.Header)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaderBlock,
		signedHeaders,
		emptyPayloadSHA256,
	}, "\n")

	credentialScope := dateStamp + "/" + r.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hexHash([]byte(canonicalRequest)),
	}, "\n")

	signingKey := deriveSigningKey(c.SecretAccessKey, dateStamp, r.cfg.Region, "s3")
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	auth := "AWS4-HMAC-SHA256 " +
		"Credential=" + c.AccessKeyID + "/" + credentialScope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
	req.Header.Set("Authorization", auth)
	return nil
}
