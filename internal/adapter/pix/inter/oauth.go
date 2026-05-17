package inter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenResponse is the JSON shape Inter returns from /oauth/v2/token.
// We ignore "scope" — the boot wiring picks the scope at request time,
// so echoing it back has no observability value.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// refreshSkew is how early we proactively refresh the cached token
// relative to its declared expiry. AC#4 fixes this at 60s so the next
// request never races a token that is about to expire on the wire.
const refreshSkew = 60 * time.Second

// minTokenTTL guards against pathological /oauth responses that claim
// a near-zero expires_in. If Inter returns anything below this we
// still cache the token but treat it as expired immediately so the
// next Create/Status call re-mints.
const minTokenTTL = 2 * refreshSkew

// tokenCache is the in-memory bearer-token cache shared across all
// concurrent Create/Status callers. The mutex serialises refresh; the
// cached token itself is read on every call, so the steady-state hot
// path is one mutex acquisition + a time comparison.
type tokenCache struct {
	mu sync.Mutex

	// token is the current bearer string, empty before the first
	// successful mint.
	token string

	// expiresAt is the absolute time at which Inter declared the
	// token would expire. Compare against time.Now()+refreshSkew to
	// decide whether to refresh.
	expiresAt time.Time
}

// fetch returns a valid bearer token, minting a new one via Inter's
// /oauth/v2/token endpoint when the cached value is empty or within
// refreshSkew of expiry.
//
// Concurrent callers serialise on the cache mutex — under contention
// the loser of the lock race sees the just-refreshed token and skips
// the network round-trip. This is intentional: Inter rate-limits
// /oauth/v2/token, and a stampede during a deploy would otherwise
// burn the limit.
func (c *Charger) fetchToken(ctx context.Context) (string, error) {
	c.tokens.mu.Lock()
	defer c.tokens.mu.Unlock()

	if c.tokens.token != "" && c.now().Add(refreshSkew).Before(c.tokens.expiresAt) {
		return c.tokens.token, nil
	}

	tok, ttl, err := c.mintToken(ctx)
	if err != nil {
		return "", err
	}

	c.tokens.token = tok
	c.tokens.expiresAt = c.now().Add(ttl)
	c.logger.Info("pix.inter: token refreshed",
		slog.String("psp", "inter"),
		slog.Duration("ttl", ttl),
		slog.Time("expires_at", c.tokens.expiresAt),
	)
	return tok, nil
}

// mintToken POSTs the client_credentials form to /oauth/v2/token and
// parses the JSON response. The bearer is never logged, even at
// debug level — only the resulting TTL.
//
// Returns the access token plus the effective TTL (>= minTokenTTL so
// the caller can never store a "born expired" token). Errors are
// wrapped with ErrTokenRefresh so callers can branch on auth
// failures separately from charge-creation failures.
func (c *Charger) mintToken(ctx context.Context) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("grant_type", "client_credentials")
	form.Set("scope", c.scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth/v2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("%w: build request: %v", ErrTokenRefresh, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrTokenRefresh, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		// The /oauth body can echo the client_id back on a 400 —
		// we DO NOT log it. Only the status code goes to the log.
		c.logger.Error("pix.inter: token refresh failed",
			slog.String("psp", "inter"),
			slog.Int("status", resp.StatusCode),
		)
		return "", 0, fmt.Errorf("%w: inter status %d", ErrTokenRefresh, resp.StatusCode)
	}

	var decoded tokenResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", 0, fmt.Errorf("%w: decode: %v", ErrTokenRefresh, err)
	}
	if decoded.AccessToken == "" {
		return "", 0, fmt.Errorf("%w: empty access_token", ErrTokenRefresh)
	}

	ttl := time.Duration(decoded.ExpiresIn) * time.Second
	if ttl < minTokenTTL {
		// Inter declared a TTL too short to be useful — treat as a
		// one-shot and force a refresh on the next call.
		ttl = 0
	}
	return decoded.AccessToken, ttl, nil
}

// ErrTokenRefresh marks any failure to obtain a bearer token from
// Inter's /oauth/v2/token. Callers may use errors.Is to distinguish
// auth-layer failures from charge-creation failures (`ErrUpstream`).
var ErrTokenRefresh = errors.New("pix.inter: token refresh failed")
