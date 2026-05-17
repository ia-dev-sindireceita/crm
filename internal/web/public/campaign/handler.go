package campaign

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// CookieName is the browser-visible idempotency token for the click
// ledger. The value is a uuid v4 minted on the first click and reused
// on every subsequent click from the same browser within the cookie
// TTL — that is how AC #2 (no duplicate row per browser) is achieved
// without server-side session state.
const CookieName = "crm_click_id"

// CookieMaxAge is the 90-day TTL the spec requires (Persistent cookie,
// per-browser identity for attribution). Long enough that the contact
// who clicks a campaign on day 1 still attributes the message they send
// on day 30; short enough that a stale shared device eventually rolls.
const CookieMaxAge = 90 * 24 * time.Hour

// Now is the time source the handler uses for the expires_at check and
// for stamping CampaignClick.CreatedAt. Tests stub this for
// determinism; production passes time.Now.UTC.
type Now func() time.Time

// IDGen returns a fresh, opaque idempotency token. The handler calls
// this only when the browser presents no crm_click_id cookie; once a
// cookie exists, subsequent clicks reuse its value verbatim so the
// adapter's UNIQUE (tenant_id, click_id) collapses to the same row.
// Production passes uuid.NewString; tests pin it.
type IDGen func() string

// Deps bundles the handler collaborators. All fields are required.
type Deps struct {
	// Repo persists CampaignClick rows and resolves campaigns by slug.
	// Production wiring binds it to *postgres/campaigns.Store; tests
	// use *campaigns.InMemoryRepository.
	Repo campaigns.Repository

	// Now is the request-time clock. See type doc.
	Now Now

	// NewClickID generates a fresh click_id when the browser has no
	// crm_click_id cookie yet. See IDGen for the contract.
	NewClickID IDGen

	// AllowedHosts is the SSRF / open-redirect allowlist applied at
	// click time to the campaign's redirect_url Host. An empty slice
	// disables the check (NOT recommended for production; the wire
	// refuses to boot when the env var is unset under the production
	// build tag). When set, the request Host (after stripping port) is
	// also implicitly allowed so a tenant who pastes its own marketing
	// site under its primary domain need not re-list itself.
	AllowedHosts []string

	// CookieSecure controls the Secure attribute on the crm_click_id
	// cookie. Production sets it true (every public request lands on
	// TLS via Caddy); tests using httptest.NewRecorder pass false so
	// the cookie is observable in test recorders.
	CookieSecure bool

	// MarkerKey is the HMAC secret used to sign the attribution marker
	// substituted into the campaign's redirect_url (SIN-62982). When
	// the zero value, the handler emits the legacy unsigned form
	// `[crm:<click_id>]`; production wiring populates the key from
	// CAMPAIGNS_MARKER_SIGNING_KEY so the inbox-side verifier in
	// internal/inbox/usecase.linkContactToCampaign can refuse forged
	// markers that misattribute a campaign.
	MarkerKey campaigns.MarkerKey

	// Logger receives one structured line per non-success outcome
	// (slug miss, expired, allowlist reject, persistence error).
	Logger *slog.Logger
}

// Handler serves the public campaign redirect endpoint. It is built
// once at composition root and is safe for concurrent use.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Nil required dependencies are rejected at
// boot time so a misconfigured wire fails fast.
func New(deps Deps) (*Handler, error) {
	if deps.Repo == nil {
		return nil, errors.New("web/public/campaign: Repo is required")
	}
	if deps.Now == nil {
		return nil, errors.New("web/public/campaign: Now is required")
	}
	if deps.NewClickID == nil {
		return nil, errors.New("web/public/campaign: NewClickID is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts the public GET /c/{slug} route on mux. Method+pattern
// uses the Go 1.22 syntax so r.PathValue("slug") resolves in the
// handler. Any other method on the path returns 405 from the stdlib
// mux automatically.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.Handle("GET /c/{slug}", h)
}

// ServeHTTP is the redirect handler. The flow follows the doc-comment
// on the package: tenant resolution → slug lookup → expiry gate →
// allowlist gate → click ledger insert → cookie set → 302.
//
// On any pre-redirect error the body is intentionally terse — the
// endpoint is unauthenticated and we do not leak internal state to a
// random caller. The structured log carries the detail for operators.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tenant, err := tenancy.FromContext(ctx)
	if err != nil {
		// TenantScope middleware MUST have run upstream. Missing
		// tenant context here is a wiring bug, not a runtime input,
		// so 500 with a logged stack reference is correct.
		h.deps.Logger.ErrorContext(ctx, "campaign/public: no tenant in context",
			slog.String("err", err.Error()),
		)
		writeStatus(w, http.StatusInternalServerError, "internal error\n")
		return
	}

	slug := r.PathValue("slug")
	normSlug, err := campaigns.NormalizeSlug(slug)
	if err != nil {
		// An invalid slug shape (e.g. /c/!!) cannot ever exist in
		// storage; collapse to 404 so we do not signal "we parse
		// this differently from a missing row".
		writeStatus(w, http.StatusNotFound, "not found\n")
		return
	}

	c, err := h.deps.Repo.GetBySlug(ctx, tenant.ID, normSlug)
	if err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			writeStatus(w, http.StatusNotFound, "not found\n")
			return
		}
		h.deps.Logger.ErrorContext(ctx, "campaign/public: GetBySlug failed",
			slog.String("tenant_id", tenant.ID.String()),
			slog.String("slug", normSlug),
			slog.String("err", err.Error()),
		)
		writeStatus(w, http.StatusInternalServerError, "internal error\n")
		return
	}

	now := h.deps.Now()
	if c.IsExpired(now) {
		h.deps.Logger.InfoContext(ctx, "campaign/public: expired slug",
			slog.String("tenant_id", tenant.ID.String()),
			slog.String("slug", normSlug),
		)
		writeStatus(w, http.StatusGone, "gone\n")
		return
	}

	if err := validateRedirectHost(c.RedirectURL, r.Host, h.deps.AllowedHosts); err != nil {
		// Reject loudly: an out-of-allowlist redirect_url is either a
		// marketer mistake (typo) or a hostile tenant trying to use
		// us as an open redirect / SSRF vector. Either way the
		// 502+log gives operators the signal they need to follow up.
		h.deps.Logger.WarnContext(ctx, "campaign/public: redirect host rejected",
			slog.String("tenant_id", tenant.ID.String()),
			slog.String("slug", normSlug),
			slog.String("redirect_url", c.RedirectURL),
			slog.String("err", err.Error()),
		)
		writeStatus(w, http.StatusBadGateway, "bad gateway\n")
		return
	}

	clickID, isNew := h.resolveClickID(r)

	click, err := campaigns.NewCampaignClick(uuid.New(), tenant.ID, c.ID, clickID, now)
	if err != nil {
		h.deps.Logger.ErrorContext(ctx, "campaign/public: NewCampaignClick failed",
			slog.String("err", err.Error()),
		)
		writeStatus(w, http.StatusInternalServerError, "internal error\n")
		return
	}
	click.IP = extractRemoteIP(r)
	click.UserAgent = r.UserAgent()
	click.Referrer = r.Referer()
	if isBotUA(click.UserAgent) {
		click.Meta["bot"] = true
	}

	if _, err := h.deps.Repo.RecordClick(ctx, click); err != nil {
		// A persistence failure does not block the redirect — the
		// marketer's link must work even when the ledger is having
		// a bad day. Logged so operators see the drift.
		h.deps.Logger.ErrorContext(ctx, "campaign/public: RecordClick failed (continuing with redirect)",
			slog.String("tenant_id", tenant.ID.String()),
			slog.String("slug", normSlug),
			slog.String("click_id", clickID),
			slog.String("err", err.Error()),
		)
	}

	if isNew {
		h.setClickCookie(w, r, clickID)
	}

	token := campaigns.BuildClickToken(h.deps.MarkerKey, tenant.ID, clickID)
	target := expandRedirect(c.RedirectURL, token)
	http.Redirect(w, r, target, http.StatusFound)
}

// expandRedirect substitutes the {click_id} placeholder (URL-encoded)
// into the campaign's redirect_url. Marketers configure URLs like
// https://wa.me/55119xxxxxxxx?text=Ol%C3%A1%20%5Bcrm%3A{click_id}%5D
// so the click_id rides along inside the pre-filled WhatsApp / Telegram
// message and the inbox-side hook can correlate the inbound message
// back to the click row.
//
// The substituted value is whatever the caller minted via
// campaigns.BuildClickToken — either the bare click_id (no signing
// key) or `<click_id>.<hmac8>` (SIN-62982 signed marker). The
// placeholder name stays `{click_id}` so existing marketer templates
// pick up the signed form for free; the dot separator survives
// url.QueryEscape unchanged.
//
// We only substitute the exact token "{click_id}" (case-sensitive) so
// existing redirect_urls without the placeholder keep working
// unchanged. The substitution uses url.QueryEscape so the value is safe
// inside a query-string value; marketers who embed the placeholder in
// the path are responsible for choosing a click_id alphabet that does
// not require escaping (today, uuid v4 hex-with-hyphens does not, and
// the optional `.<hmac8>` suffix is RFC 3986 unreserved as well).
func expandRedirect(redirectURL, clickToken string) string {
	const placeholder = "{click_id}"
	if !strings.Contains(redirectURL, placeholder) {
		return redirectURL
	}
	return strings.ReplaceAll(redirectURL, placeholder, url.QueryEscape(clickToken))
}

// resolveClickID returns the click_id to use for this request and
// whether it was freshly minted. A cookie present and non-empty wins;
// otherwise we mint via NewClickID.
func (h *Handler) resolveClickID(r *http.Request) (string, bool) {
	if cookie, err := r.Cookie(CookieName); err == nil {
		v := strings.TrimSpace(cookie.Value)
		if v != "" {
			return v, false
		}
	}
	return h.deps.NewClickID(), true
}

// setClickCookie writes the crm_click_id cookie with the security
// attributes spec'd in AC #2 (httpOnly, SameSite=Lax, Secure when wired,
// Path=/, 90d). Domain is intentionally left empty so the cookie binds
// to the exact tenant host the browser saw — a contact who clicks on
// acme.crm.example does NOT receive a cookie that travels to
// other-tenant.crm.example, even if the operator misconfigures DNS.
func (h *Handler) setClickCookie(w http.ResponseWriter, _ *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(CookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   h.deps.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// extractRemoteIP parses the request RemoteAddr into a netip.Addr.
// Returns the zero Addr (!IsValid) when the source is unparseable —
// the storage adapter writes SQL NULL for that case, which is what the
// schema expects for "unknown". Matches httpapi/ratelimit's stance on
// X-Forwarded-For: the trust boundary lives in Caddy / the edge, which
// rewrites r.RemoteAddr; this handler stays naive on purpose.
func extractRemoteIP(r *http.Request) netip.Addr {
	if r == nil || r.RemoteAddr == "" {
		return netip.Addr{}
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

// validateRedirectHost enforces the SSRF / open-redirect allowlist on
// the campaign's redirect_url. The validation is intentionally a
// belt-and-braces re-check at click-time: NewCampaign already filtered
// the URL on write, but allowlist drift after a tenant is configured
// (operator narrows the list, marketer pre-loaded a hostile URL) MUST
// not silently keep working.
//
// Allowed paths:
//
//   - The redirect host (case-folded, port-stripped) matches the
//     request Host exactly. Tenants whose marketing site is on the
//     same primary domain as the CRM never need to list themselves.
//   - The redirect host matches any entry in allowed (also case-folded,
//     port-stripped). A leading "*." in an allowed entry matches any
//     direct subdomain (wa.me, *.wa.me, etc.). Empty entries are
//     ignored so a misconfigured "wa.me,," does not implicitly trust
//     the empty host.
//
// Returns errAllowlistMiss when no rule matches. errInvalidRedirect
// when the URL itself cannot be re-parsed (this should be impossible
// for stored rows but we guard anyway).
func validateRedirectHost(redirectURL, requestHost string, allowed []string) error {
	u, err := url.Parse(redirectURL)
	if err != nil {
		return errInvalidRedirect
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return errInvalidRedirect
	}
	target := strings.ToLower(u.Hostname())
	if target == "" {
		return errInvalidRedirect
	}
	if reqHost := normaliseHost(requestHost); reqHost != "" && reqHost == target {
		return nil
	}
	for _, entry := range allowed {
		e := normaliseHost(entry)
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, "*.") {
			suffix := e[1:] // ".wa.me"
			if strings.HasSuffix(target, suffix) && len(target) > len(suffix) {
				return nil
			}
			continue
		}
		if e == target {
			return nil
		}
	}
	return errAllowlistMiss
}

// errAllowlistMiss is the typed sentinel for an out-of-allowlist
// redirect host. Exposed so the wire's tests can errors.Is against
// it; the package itself does not export it (use the public type
// IsAllowlistMissError instead from a future package surface).
var errAllowlistMiss = errors.New("campaign/public: redirect host not in allowlist")

// errInvalidRedirect signals that the stored redirect_url is no longer
// a parseable http/https URL. This is a defence-in-depth check — the
// write-time NewCampaign validator already rejects these — but a row
// that drifted (legacy migration, manual SQL) must NOT silently 302
// to a control character or javascript: URL.
var errInvalidRedirect = errors.New("campaign/public: redirect url invalid at click time")

// normaliseHost lowercases the host and drops a trailing :port. Used
// by validateRedirectHost on both the request Host and each allowlist
// entry.
func normaliseHost(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// isBotUA is a best-effort matcher: a small set of substrings common
// to crawler / scanner User-Agents. It deliberately leans permissive
// (false negatives are fine — the click still persists; the dashboard
// just won't tag it bot=true). False positives would be worse because
// real human clicks would be discounted from attribution. Maintain
// from the bottom up: only add substrings observed in real traffic
// logs.
func isBotUA(ua string) bool {
	if ua == "" {
		// Empty UA is suspicious enough on a marketer link click that
		// we tag it — almost no real browser elides UA today.
		return true
	}
	low := strings.ToLower(ua)
	for _, marker := range botMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

var botMarkers = []string{
	"bot", "spider", "crawler", "preview", "scrape", "fetch",
	"curl/", "wget/", "python-requests", "go-http-client",
	"headlesschrome", "phantomjs", "puppeteer",
}

// writeStatus writes a terse text/plain body with the given code. The
// content-type and nosniff header are set explicitly so an
// unauthenticated 4xx is never sniffed into something more dangerous.
func writeStatus(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// Compile-time guard: Handler is a usable http.Handler. Caught here so
// a future signature change on ServeHTTP fails this file's build
// before it fails the router test suite.
var _ http.Handler = (*Handler)(nil)

// Sentinel re-exports for the wire and its tests. Kept at the bottom
// of the file so a reader scanning the handler logic does not trip on
// them.
var (
	// ErrAllowlistMiss reflects the package-internal allowlist-miss
	// sentinel. Tests assert against it via errors.Is.
	ErrAllowlistMiss = errAllowlistMiss
	// ErrInvalidRedirect reflects the click-time URL re-parse failure.
	ErrInvalidRedirect = errInvalidRedirect
)
