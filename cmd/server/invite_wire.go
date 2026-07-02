package main

// SIN-66510 wiring — public invite / set-password endpoint (child of
// SIN-66493; consumes the credential token minted by SIN-66496).
//
// buildWebInviteHandler assembles the GET|POST /invite/{token} surface plus
// its per-IP + per-token-prefix rate limit (G6). The handler is mounted by
// the chi router inside the tenanted group BUT outside the authed sub-group —
// the token in the URL IS the credential, so the page is unauthenticated by
// design and protected by the compensating controls documented on
// internal/web/invite.
//
// Returns (nil, nil) when the supplied pool / redis client is nil so
// cmd/server boots cleanly in health-only / partial-stack modes; the chi
// router skips /invite/{token} in that case. A nil rdb specifically means "no
// rate limiter" — the endpoint MUST NOT ship without one (SIN-66510 "never
// ships: endpoint público sem rate-limit"), so the route stays unmounted.

import (
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgtenantusers "github.com/pericles-luz/crm/internal/adapter/db/postgres/tenantusers"
	"github.com/pericles-luz/crm/internal/adapter/hibp"
	httpratelimit "github.com/pericles-luz/crm/internal/adapter/httpapi/ratelimit"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	"github.com/pericles-luz/crm/internal/iam/password"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
	"github.com/pericles-luz/crm/internal/tenantusers"
	webinvite "github.com/pericles-luz/crm/internal/web/invite"
)

const (
	// envInviteRatePerMin tunes the per-IP rate limit on /invite/{token}.
	// Default is defaultInviteRatePerMin; operators dial it per environment
	// without a redeploy. Non-positive / non-numeric falls back to the default.
	envInviteRatePerMin = "INVITE_PUBLIC_RATE_PER_MIN"

	// defaultInviteRatePerMin is the per-IP floor: a legitimate invitee loads
	// the page and submits once or twice, so 20/min/IP is generous headroom
	// while still blunting scripted abuse.
	defaultInviteRatePerMin = 20

	// inviteTokenRatePerMin is the per-token-prefix cap: repeated attempts
	// against the SAME invite link (e.g. brute-forcing the tail of a leaked
	// prefix, or a stuck retry loop) are throttled independently of the IP.
	inviteTokenRatePerMin = 10

	// inviteTokenPrefixLen is how many leading characters of the URL token
	// key the per-token bucket. A prefix (not the whole token) keeps the
	// Redis key from being a full secret while still isolating one invite
	// link's traffic; 12 base64url chars ≈ 72 bits so distinct tokens do not
	// collide in practice.
	inviteTokenPrefixLen = 12

	// invitePolicyName is the iam/ratelimit.Policy name for the invite
	// buckets. Distinct from the auth policy names so the Redis key prefix
	// never collides.
	invitePolicyName = "invite_consume"

	// inviteRateRedisPrefix is the Redis key namespace for the invite rate
	// limiter, kept under its own root so a flush of one domain does not nuke
	// another.
	inviteRateRedisPrefix = "invite:rl:"
)

// buildWebInviteHandler returns the /invite/{token} handler stitched with its
// per-IP + per-token-prefix rate limit. Returns (nil, nil) when pool or rdb
// is nil — the caller (IAM wire) treats that as "skip the invite route".
//
// pool MUST be the runtime pool (the tenantusers adapter uses
// postgres.WithTenant under the hood). rdb is the shared goredis client the
// auth-side limiter also uses; invite traffic lives under its own namespace.
func buildWebInviteHandler(pool *pgxpool.Pool, rdb *goredis.Client, getenv func(string) string) (http.Handler, error) {
	if pool == nil || rdb == nil {
		return nil, nil
	}

	store, err := pgtenantusers.New(pool)
	if err != nil {
		return nil, fmt.Errorf("invite/public: build store: %w", err)
	}
	auditor, err := pgpool.NewSplitAuditLogger(pool)
	if err != nil {
		return nil, fmt.Errorf("invite/public: build audit logger: %w", err)
	}

	handler, err := assembleWebInviteHandler(store, auditor, slog.Default())
	if err != nil {
		return nil, err
	}

	rate := readInviteRatePerMin(getenv)
	mw, err := buildInviteRateLimitMiddleware(rdb, rate, slog.Default())
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	handler.Routes(mux)
	wrapped := mw(mux)

	log.Printf("crm: invite/public /invite/{token} mounted (rate=%d/min/IP, %d/min/token-prefix)", rate, inviteTokenRatePerMin)
	return wrapped, nil
}

// assembleWebInviteHandler is the pure-assembly seam tests call with a fake
// repository + auditor so the handler construction is exercised without a real
// pgxpool. The password Policy reuses the bundled HIBP local list (ADR 0070
// §5 breach screening) with the argon2id Default hasher — crypto is reused,
// not re-rolled.
func assembleWebInviteHandler(repo tenantusers.CredentialTokenRepository, auditor tenantusers.Auditor, logger *slog.Logger) (*webinvite.Handler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	policy, err := buildInvitePasswordPolicy(logger)
	if err != nil {
		return nil, fmt.Errorf("invite/public: build password policy: %w", err)
	}
	svc, err := tenantusers.NewCredentialService(tenantusers.CredentialConfig{
		Repo:    repo,
		Hasher:  password.Default(),
		Policy:  policy,
		Auditor: auditor,
		Logger:  logger,
	})
	if err != nil {
		return nil, fmt.Errorf("invite/public: build credential service: %w", err)
	}
	h, err := webinvite.New(webinvite.Deps{Service: svc, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("invite/public: build handler: %w", err)
	}
	return h, nil
}

// buildInvitePasswordPolicy returns the ADR 0070 §5 policy wired to the
// bundled top-N breach list (no remote HIBP call on the public set-password
// path — the local list gives offline breach screening; Pwned=nil means the
// policy consults LocalList directly).
func buildInvitePasswordPolicy(logger *slog.Logger) (*password.Policy, error) {
	local, err := hibp.NewLocalList()
	if err != nil {
		return nil, fmt.Errorf("invite/public: load local breach list: %w", err)
	}
	return &password.Policy{LocalList: local, Logger: logger}, nil
}

// buildInviteRateLimitMiddleware assembles the two-bucket throttle (per-IP +
// per-token-prefix) in front of the invite handler.
func buildInviteRateLimitMiddleware(rdb *goredis.Client, ratePerMin int, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	policy, err := domainratelimit.NewPolicy(
		invitePolicyName,
		[]domainratelimit.Bucket{
			{Name: "ip", Window: time.Minute, Max: ratePerMin},
			{Name: "token", Window: time.Minute, Max: inviteTokenRatePerMin},
		},
		domainratelimit.Lockout{},
	)
	if err != nil {
		return nil, fmt.Errorf("invite/public: build rate-limit policy: %w", err)
	}
	limiter := rlredis.New(rdb, inviteRateRedisPrefix)
	mw, err := httpratelimit.New(httpratelimit.Config{
		Policy:  policy,
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			// SIN-62978: IPKeyExtractor reads r.RemoteAddr; the trusted-proxy
			// wrapper + edge strip make that the real client IP.
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
			{Name: "token", Extractor: inviteTokenPrefixExtractor},
		},
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("invite/public: build rate-limit middleware: %w", err)
	}
	return mw, nil
}

// inviteTokenPrefixExtractor keys the per-token bucket on a short prefix of
// the {token} path segment. It parses the path directly (router-agnostic:
// works whether or not chi has assigned the URLParam yet). An empty / missing
// token yields "" so the middleware skips the bucket rather than 429-ing.
func inviteTokenPrefixExtractor(r *http.Request) string {
	if r == nil {
		return ""
	}
	const prefix = "/invite/"
	path := r.URL.Path
	i := strings.Index(path, prefix)
	if i < 0 {
		return ""
	}
	tok := path[i+len(prefix):]
	if j := strings.IndexByte(tok, '/'); j >= 0 {
		tok = tok[:j]
	}
	if tok == "" {
		return ""
	}
	if len(tok) > inviteTokenPrefixLen {
		tok = tok[:inviteTokenPrefixLen]
	}
	return tok
}

// readInviteRatePerMin parses INVITE_PUBLIC_RATE_PER_MIN; unset / non-positive
// falls back to defaultInviteRatePerMin. Capped at 1_000_000 so a typo cannot
// overflow downstream arithmetic.
func readInviteRatePerMin(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(envInviteRatePerMin))
	if raw == "" {
		return defaultInviteRatePerMin
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultInviteRatePerMin
	}
	if v > 1_000_000 {
		v = 1_000_000
	}
	return v
}
