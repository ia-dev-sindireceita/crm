package main

// SIN-62959 wiring — public campaign redirect endpoint (Fase 4 child of
// SIN-62197).
//
// buildWebCampaignHandler assembles the GET /c/{slug} surface plus its
// per-IP rate limit. The handler is mounted by the chi router inside
// the tenanted group BUT outside the authed sub-group — the redirect
// is intentionally unauthenticated (AC #1) and protected by the
// compensating controls documented on internal/web/public/campaign.
//
// Returns (nil, no-op, nil) when the supplied pool / redis client is
// nil so cmd/server boots cleanly in health-only / partial-stack
// modes; the chi router skips the /c/{slug} route in that case.

import (
	"encoding/base64"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	pgcampaigns "github.com/pericles-luz/crm/internal/adapter/db/postgres/campaigns"
	httpratelimit "github.com/pericles-luz/crm/internal/adapter/httpapi/ratelimit"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	"github.com/pericles-luz/crm/internal/campaigns"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
	webcampaign "github.com/pericles-luz/crm/internal/web/public/campaign"
)

const (
	// envCampaignRatePerMin tunes the per-IP rate limit applied to
	// GET /c/{slug}. AC #4 default is 100/min/IP; operators dial it
	// higher / lower per environment without a redeploy. Negative or
	// non-numeric values fall back to defaultCampaignRatePerMin.
	envCampaignRatePerMin = "CAMPAIGNS_PUBLIC_CLICK_RATE_PER_MIN"

	// envCampaignAllowedHosts is the comma-separated open-redirect
	// allowlist (AC #7). Example: "wa.me,*.wa.me,t.me,*.example.com".
	// Empty disables the per-hostname allowlist; the request-Host
	// implicit allow still applies. Production wiring SHOULD set this
	// to the minimum set of trusted carriers.
	envCampaignAllowedHosts = "CAMPAIGNS_REDIRECT_ALLOWED_HOSTS"

	// envCampaignCookieInsecure flips the __Host-crm_click_id Secure
	// attribute off. Production leaves it unset (Secure=true); local
	// docker-compose without TLS sets it to "1" so the cookie still
	// rides over plain HTTP — note that browsers will reject a __Host-
	// cookie that is not Secure, so the click-idempotency path is a
	// no-op in that dev mode (SIN-62983 LOW-1).
	envCampaignCookieInsecure = "CAMPAIGNS_PUBLIC_COOKIE_INSECURE"

	// envCampaignMarkerSigningKey carries the HMAC secret used to sign
	// the `[crm:<id>.<hmac8>]` attribution marker the redirect handler
	// embeds and the inbox-side hook verifies (SIN-62982). Encoded as
	// base64-standard for transport tolerance; the decoded key must be
	// at least minCampaignMarkerKeyBytes long or the wire logs a
	// disabled-marker warning and emits the legacy unsigned form.
	envCampaignMarkerSigningKey = "CAMPAIGNS_MARKER_SIGNING_KEY"

	// defaultCampaignRatePerMin is the AC #4 floor.
	defaultCampaignRatePerMin = 100

	// minCampaignMarkerKeyBytes is the minimum decoded key length the
	// SIN-62982 signing key must carry. 32 bytes matches HMAC-SHA256's
	// recommended key size and prevents a wire that pasted a truncated
	// secret from silently signing with weak material.
	minCampaignMarkerKeyBytes = 32

	// campaignClickPolicyName is the iam/ratelimit.Policy name used
	// by the per-IP bucket. Kept distinct from the auth policy names
	// (login, m_login, …) so the Redis key prefix never collides.
	campaignClickPolicyName = "campaign_click"

	// campaignRateRedisPrefix is the Redis key namespace for the
	// per-IP rate limiter. Matches the "auth:rl:" naming convention
	// from internal/adapter/ratelimit/redis but keyed under its own
	// root so a flush of one domain does not nuke the other.
	campaignRateRedisPrefix = "campaign:rl:"
)

// buildWebCampaignHandler returns the GET /c/{slug} handler stitched
// with its per-IP rate limit. Returns (nil, nil) when pool or rdb is
// nil — the caller (IAM wire) treats that as "skip the campaigns
// route" without booting an empty handler.
//
// pool MUST be the runtime pool (campaigns adapter uses
// postgres.WithTenant under the hood). rdb is the shared goredis
// client the auth-side limiter also uses; campaign clicks live under
// the campaignRateRedisPrefix namespace so the two domains are
// independently observable.
func buildWebCampaignHandler(pool *pgxpool.Pool, rdb *goredis.Client, getenv func(string) string) (http.Handler, error) {
	if pool == nil || rdb == nil {
		return nil, nil
	}

	store, err := pgcampaigns.New(pool)
	if err != nil {
		return nil, fmt.Errorf("campaigns/public: build store: %w", err)
	}

	allowedHosts := parseAllowedHosts(getenv(envCampaignAllowedHosts))
	if len(allowedHosts) == 0 {
		log.Printf("crm: campaigns/public allowlist empty (CAMPAIGNS_REDIRECT_ALLOWED_HOSTS unset) — only same-host redirects will be allowed")
	}

	markerKey := readMarkerSigningKey(getenv)
	if !markerKey.HasValue() {
		log.Printf("crm: campaigns/public marker signing key disabled (CAMPAIGNS_MARKER_SIGNING_KEY unset or too short) — emitting legacy unsigned [crm:<id>] markers")
	}

	handler, err := assembleCampaignHandlerWithMarker(store, allowedHosts, !cookieInsecure(getenv), markerKey, slog.Default())
	if err != nil {
		return nil, err
	}

	rate := readCampaignRatePerMin(getenv)
	mw, err := buildCampaignRateLimitMiddleware(rdb, rate, slog.Default())
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	handler.Routes(mux)
	wrapped := mw(mux)

	log.Printf("crm: campaigns/public GET /c/{slug} mounted (rate=%d/min/IP, allowlist=%v)", rate, allowedHosts)
	return wrapped, nil
}

// assembleCampaignHandler is the pure-assembly seam used by tests so
// the handler construction can be exercised without a real pgxpool.
// Delegates to assembleCampaignHandlerWithMarker with a zero MarkerKey
// (legacy unsigned marker output) so pre-SIN-62982 callers stay
// source-compatible. New wire-level call sites SHOULD use the
// WithMarker variant so the SIN-62982 signed marker is emitted.
func assembleCampaignHandler(
	repo campaigns.Repository,
	allowedHosts []string,
	cookieSecure bool,
	logger *slog.Logger,
) (*webcampaign.Handler, error) {
	return assembleCampaignHandlerWithMarker(repo, allowedHosts, cookieSecure, nil, logger)
}

// assembleCampaignHandlerWithMarker is the SIN-62982 seam: same as
// assembleCampaignHandler but accepts the MarkerKey the redirect
// handler uses to sign attribution markers.
func assembleCampaignHandlerWithMarker(
	repo campaigns.Repository,
	allowedHosts []string,
	cookieSecure bool,
	markerKey campaigns.MarkerKey,
	logger *slog.Logger,
) (*webcampaign.Handler, error) {
	return webcampaign.New(webcampaign.Deps{
		Repo:         repo,
		Now:          func() time.Time { return time.Now().UTC() },
		NewClickID:   func() string { return uuid.NewString() },
		AllowedHosts: allowedHosts,
		CookieSecure: cookieSecure,
		MarkerKey:    markerKey,
		Logger:       logger,
	})
}

// buildCampaignRateLimitMiddleware assembles the single-bucket per-IP
// throttle in front of the click handler. Lives as a separate function
// so cmd/server tests can substitute the policy/limiter without dragging
// in goredis.
func buildCampaignRateLimitMiddleware(rdb *goredis.Client, ratePerMin int, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	policy, err := domainratelimit.NewPolicy(
		campaignClickPolicyName,
		[]domainratelimit.Bucket{
			{Name: "ip", Window: time.Minute, Max: ratePerMin},
		},
		domainratelimit.Lockout{},
	)
	if err != nil {
		return nil, fmt.Errorf("campaigns/public: build rate-limit policy: %w", err)
	}
	limiter := rlredis.New(rdb, campaignRateRedisPrefix)
	// SIN-62978: IPKeyExtractor reads r.RemoteAddr. The trusted-proxy
	// wrapper in internal/adapter/httpapi/trusted_realip.go gates the
	// chimw.RealIP rewrite to peers inside TRUSTED_PROXY_CIDRS, and
	// deploy/caddy/Caddyfile strips True-Client-IP / X-Real-IP /
	// X-Forwarded-For at the edge — together those two controls mean
	// the key here is the real client IP for the AC #4 100/min/IP bucket.
	mw, err := httpratelimit.New(httpratelimit.Config{
		Policy:  policy,
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
		},
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("campaigns/public: build rate-limit middleware: %w", err)
	}
	return mw, nil
}

// readCampaignRatePerMin parses CAMPAIGNS_PUBLIC_CLICK_RATE_PER_MIN;
// unset / non-positive falls back to the AC #4 default (100). Capped
// at 1_000_000 so a typo does not overflow downstream arithmetic.
func readCampaignRatePerMin(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(envCampaignRatePerMin))
	if raw == "" {
		return defaultCampaignRatePerMin
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultCampaignRatePerMin
	}
	if v > 1_000_000 {
		v = 1_000_000
	}
	return v
}

// parseAllowedHosts splits the comma-separated env value, trims each
// entry, and drops empty fragments. A trailing comma or extra space
// collapses to a clean slice rather than implicitly trusting the
// empty host.
func parseAllowedHosts(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// cookieInsecure reports whether CAMPAIGNS_PUBLIC_COOKIE_INSECURE is
// set to a truthy value. Recognised truthy values are "1", "true",
// "yes" (case-insensitive). Anything else (including the empty
// string) keeps the secure default.
func cookieInsecure(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(envCampaignCookieInsecure))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// buildCampaignLinker returns the campaign-link port for inbound
// message attribution (SIN-62959 AC #3). Wired into ReceiveInbound by
// the WhatsApp / Messenger wires; returns (nil, error) on adapter
// failure so the caller can fall back to leaving the hook unwired.
func buildCampaignLinker(pool *pgxpool.Pool) (campaigns.Repository, error) {
	if pool == nil {
		return nil, nil
	}
	return pgcampaigns.New(pool)
}

// readMarkerSigningKey decodes CAMPAIGNS_MARKER_SIGNING_KEY. The env
// value is base64 (standard or url-safe; padding optional) so deploy
// tooling can ship binary key material without quoting drama. Returns
// the zero MarkerKey when the env is unset, undecodable, or shorter
// than minCampaignMarkerKeyBytes after decode — the wire then logs a
// disabled-marker warning and the handler emits the legacy unsigned
// marker form.
func readMarkerSigningKey(getenv func(string) string) campaigns.MarkerKey {
	raw := strings.TrimSpace(getenv(envCampaignMarkerSigningKey))
	if raw == "" {
		return nil
	}
	for _, dec := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if k, err := dec.DecodeString(raw); err == nil && len(k) >= minCampaignMarkerKeyBytes {
			return campaigns.MarkerKey(k)
		}
	}
	return nil
}
