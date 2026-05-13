// Package hibp is the production adapter for the
// password.PwnedPasswordChecker port.
//
// It performs the HaveIBeenPwned k-anonymity lookup
// (https://api.pwnedpasswords.com/range/<sha1-prefix-5>), parses the
// response, and matches the remaining 35-character SHA-1 suffix locally —
// the plaintext password is never sent to the upstream service.
//
// The adapter wraps a 3-state circuit breaker (see breaker.go) so a
// degraded upstream short-circuits with ErrPwnedCheckUnavailable rather
// than blocking every login. The password.Policy treats that sentinel as
// "consult the bundled local top-N list" per ADR 0070 §5.
package hibp

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/iam/password"
)

// DefaultBaseURL is the production HIBP range endpoint. Tests inject
// an httptest.Server URL via Client.BaseURL.
const DefaultBaseURL = "https://api.pwnedpasswords.com"

// DefaultUserAgent is the User-Agent header HIBP requires (their TOS asks
// for an identifying UA — opaque "Go-http-client" is rejected at the
// edge).
const DefaultUserAgent = "sindireceita-crm-hibp/1.0"

// DefaultTimeout caps each upstream call. ADR 0070 §5 — the breach check
// runs on the password-set/login critical path; a degraded upstream MUST
// fall through to the local list within seconds, not wait on a stalled
// connection.
const DefaultTimeout = 3 * time.Second

const (
	defaultBreakerThreshold = 5
	defaultBreakerCooldown  = 30 * time.Second
)

// Client implements password.PwnedPasswordChecker against the HIBP
// k-anonymity API.
//
// The zero-value Client is NOT useful — use New() so the breaker, HTTP
// timeout, and User-Agent are correctly initialised.
type Client struct {
	HTTPClient *http.Client
	BaseURL    string
	UserAgent  string

	breaker *breaker
}

// New returns a Client with sensible defaults: 3 s timeout, 5-failure
// breaker, 30 s cool-down.
func New() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: DefaultTimeout},
		BaseURL:    DefaultBaseURL,
		UserAgent:  DefaultUserAgent,
		breaker:    newBreaker(defaultBreakerThreshold, defaultBreakerCooldown),
	}
}

// IsPwned implements password.PwnedPasswordChecker. It returns
// (true, nil) if the password's SHA-1 is in the HIBP corpus, (false, nil)
// if not, and (false, password.ErrPwnedCheckUnavailable) when the
// upstream is degraded (network error, non-2xx status, or breaker open).
//
// The plaintext is hashed with SHA-1 and ONLY the first five hex
// characters are sent over the wire. The server returns ~500–1000 35-char
// suffix lines for that prefix; we match locally in constant time per
// line — the plaintext never leaves this process.
func (c *Client) IsPwned(ctx context.Context, plain string) (bool, error) {
	if c == nil || c.HTTPClient == nil {
		return false, password.ErrPwnedCheckUnavailable
	}
	if c.breaker == nil {
		c.breaker = newBreaker(defaultBreakerThreshold, defaultBreakerCooldown)
	}
	if !c.breaker.allow() {
		return false, password.ErrPwnedCheckUnavailable
	}
	prefix, suffix := sha1HexSplit(plain)
	url := strings.TrimRight(c.BaseURL, "/") + "/range/" + prefix
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.breaker.recordFailure()
		return false, password.ErrPwnedCheckUnavailable
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	req.Header.Set("Add-Padding", "true")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		c.breaker.recordFailure()
		return false, fmt.Errorf("%w: %v", password.ErrPwnedCheckUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		c.breaker.recordFailure()
		return false, fmt.Errorf("%w: status %d", password.ErrPwnedCheckUnavailable, resp.StatusCode)
	}
	hit, err := scanSuffix(resp.Body, suffix)
	if err != nil {
		c.breaker.recordFailure()
		return false, fmt.Errorf("%w: %v", password.ErrPwnedCheckUnavailable, err)
	}
	c.breaker.recordSuccess()
	return hit, nil
}

// sha1HexSplit returns the (5-char prefix, 35-char suffix) of the
// uppercased SHA-1 hex of plain. The split point is HIBP's k-anonymity
// boundary.
func sha1HexSplit(plain string) (string, string) {
	sum := sha1.Sum([]byte(plain)) //nolint:gosec // HIBP API uses SHA-1 by spec; not a security primitive here.
	full := strings.ToUpper(hex.EncodeToString(sum[:]))
	return full[:5], full[5:]
}

// scanSuffix reads the HIBP range response line by line. Each line is
// "<35-hex-suffix>:<count>\r\n"; "Add-Padding: true" makes the response
// length attacker-opaque. We compare the suffix in constant time per
// line and return on the first hit.
func scanSuffix(r io.Reader, want string) (bool, error) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			s := strings.TrimSpace(line)
			if s != "" {
				colon := strings.IndexByte(s, ':')
				if colon < 0 {
					continue
				}
				suffix := s[:colon]
				countStr := s[colon+1:]
				if !strings.EqualFold(suffix, want) {
					continue
				}
				count, perr := strconv.Atoi(countStr)
				if perr != nil {
					continue
				}
				if count > 0 {
					return true, nil
				}
				// "<suffix>:0" lines come from Add-Padding; skip.
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}
