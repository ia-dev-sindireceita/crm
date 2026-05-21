package consent

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/consent"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// PolicyVersion is the cookies-consent version label stamped onto the
// browser cookie and onto the ConsentRegistry row. Bump when the
// banner copy changes materially so old acceptances roll forward.
const PolicyVersion = "v1"

// CookieName is the consent decision cookie. The "__Host-" prefix is
// the same RFC 6265bis §4.1.3.2 guard that internal/web/public/campaign
// uses on its click_id cookie: the browser drops the cookie if it is
// not Secure / Path=/ / Domain-less. Local HTTP dev with
// CONSENT_COOKIE_INSECURE=1 sees the cookie rejected by the browser —
// that is a deliberate downgrade signal.
const CookieName = "__Host-crm_consent_v1"

// CookieMaxAge is the 1-year TTL for the decision. The LGPD
// orientation memo (Senacon 2023) considers any consent older than 12
// months as stale; 1y is the upper bound.
const CookieMaxAge = 365 * 24 * time.Hour

// Decision is one of the two terminal values the visitor can pick.
type Decision string

const (
	DecisionAccept  Decision = "accept"
	DecisionDecline Decision = "decline"
)

// Valid reports whether d is one of the terminal values.
func (d Decision) Valid() bool { return d == DecisionAccept || d == DecisionDecline }

// Recorder writes the visitor's decision into the consent registry
// when the principal can be resolved. The narrow interface keeps the
// handler test-friendly; production binds it to
// internal/iam/consent.RecordingRegistry via the wire layer.
type Recorder interface {
	Record(ctx context.Context, rec consent.ConsentRecord) (consent.ConsentRecord, bool, error)
}

// Deps bundles the handler collaborators.
type Deps struct {
	// Registry persists the decision when the principal can be
	// resolved from context. Nil is permitted in tests that only
	// exercise the cookie path; production wires the
	// SIN-63185 RecordingRegistry.
	Registry Recorder

	// Now is the wall-clock source for the cookie expiry stamp and
	// the ConsentRecord GrantedAt. Defaults to time.Now.UTC.
	Now func() time.Time

	// CookieSecure controls the Secure attribute. Production sets
	// true (every public request lands on TLS via Caddy); tests with
	// httptest.NewRecorder pass false so the Set-Cookie header is
	// observable in the recorder.
	CookieSecure bool

	// Logger receives one structured line per non-success outcome
	// (registry error, malformed body). Defaults to slog.Default.
	Logger *slog.Logger
}

// Handler serves the banner partial and the decision POST.
type Handler struct {
	deps Deps
}

// New validates deps and returns a ready Handler. Registry is
// optional; everything else falls back to a sensible default so tests
// can leave them zero.
func New(deps Deps) (*Handler, error) {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts the two endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /consent/cookies-banner", h.banner)
	mux.HandleFunc("POST /consent/cookies", h.submit)
}

// Banner and Submit are exported so router tests can drive the
// handlers without going through the inner mux.
func (h *Handler) Banner(w http.ResponseWriter, r *http.Request) { h.banner(w, r) }
func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) { h.submit(w, r) }

func (h *Handler) banner(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(CookieName); err == nil && ck.Value != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := bannerTmpl.Execute(w, bannerData{Version: PolicyVersion}); err != nil {
		h.deps.Logger.Error("web/consent: render banner", "err", err)
	}
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	decision := Decision(strings.TrimSpace(r.PostFormValue("decision")))
	if !decision.Valid() {
		http.Error(w, "invalid decision", http.StatusBadRequest)
		return
	}
	now := h.deps.Now().UTC()
	value := fmt.Sprintf("%s.%s.%d", PolicyVersion, decision, now.Unix())
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(CookieMaxAge / time.Second),
		Secure:   h.deps.CookieSecure,
		HttpOnly: false, // banner is JS-readable so a future progressive enhancement can re-render
		SameSite: http.SameSiteLaxMode,
	})

	// Try to record into the registry when we have an authenticated
	// principal. Anonymous visitors get cookie-only; the registry
	// needs a non-empty Subject.ID and tenant context.
	if err := h.recordDecision(r, decision, now); err != nil {
		// Non-fatal — the cookie already carries the visitor's
		// decision so the banner does not re-appear. Log so the
		// audit team can reconcile if needed.
		h.deps.Logger.Warn("web/consent: registry record failed",
			"decision", string(decision), "err", err)
	}

	// Empty 200 response: HTMX swap consumes it (clears the banner),
	// non-JS clients see a blank page — both are valid liveness paths.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := ackTmpl.Execute(w, ackData{Decision: string(decision)}); err != nil {
		h.deps.Logger.Error("web/consent: render ack", "err", err)
	}
}

// recordDecision writes one row through Registry when a Principal +
// Tenant are on the context. Returns an error only when the registry
// itself returned one (programmer-visible failure); missing
// principal/tenant returns nil so anonymous /privacy visitors do not
// log warnings.
func (h *Handler) recordDecision(r *http.Request, d Decision, now time.Time) error {
	if h.deps.Registry == nil {
		return nil
	}
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil || tenant == nil {
		return nil
	}
	principal, ok := iam.PrincipalFromContext(r.Context())
	if !ok || principal.UserID.String() == "" {
		return nil
	}
	addr, _ := netip.ParseAddr(clientIP(r))
	rec := consent.ConsentRecord{
		TenantID: tenant.ID,
		Subject: consent.Subject{
			Type: consent.SubjectUser,
			ID:   principal.UserID.String(),
		},
		Purpose:   consent.PurposeCookiesAnalytics,
		Version:   PolicyVersion,
		Granted:   d == DecisionAccept,
		GrantedAt: now,
		IP:        addr,
		UserAgent: r.UserAgent(),
	}
	_, _, err = h.deps.Registry.Record(r.Context(), rec)
	if err != nil {
		return fmt.Errorf("consent: registry record: %w", err)
	}
	return nil
}

// clientIP strips the port from r.RemoteAddr. The trusted_realip
// middleware already rewrote r.RemoteAddr upstream when the immediate
// peer is a trusted proxy; we only render the final value here.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// bannerData drives the banner partial.
type bannerData struct {
	Version string
}

// ackData drives the post-submit empty ack fragment.
type ackData struct {
	Decision string
}

// bannerTmpl renders the cookie consent banner partial. Both buttons
// are inside the same form and submit to the same endpoint with a
// different `decision` value, so the form degrades cleanly to a
// non-JS POST. Aria roles + visible labels satisfy AC #5
// (accessibility / WCAG AA).
var bannerTmpl = template.Must(template.New("cookie_banner").Parse(`<aside id="cookie-banner"
       class="cookie-banner"
       role="dialog"
       aria-labelledby="cookie-banner-title"
       aria-describedby="cookie-banner-body"
       data-version="{{.Version}}">
  <h2 id="cookie-banner-title" class="cookie-banner__title">Cookies e privacidade</h2>
  <p id="cookie-banner-body" class="cookie-banner__body">
    Usamos cookies essenciais para o funcionamento do atendimento (registrados
    automaticamente, conforme a LGPD). Para uso de cookies de
    <strong>analytics</strong>, precisamos do seu aceite explícito.
    Consulte nossa <a href="/privacy">Política de Privacidade</a>.
  </p>
  <form class="cookie-banner__form"
        method="post"
        action="/consent/cookies"
        hx-post="/consent/cookies"
        hx-target="#cookie-banner"
        hx-swap="outerHTML"
        aria-label="Decisão de cookies de analytics">
    <button type="submit"
            name="decision"
            value="accept"
            class="cookie-banner__btn cookie-banner__btn--primary"
            aria-label="Aceitar cookies de analytics">
      Aceitar analytics
    </button>
    <button type="submit"
            name="decision"
            value="decline"
            class="cookie-banner__btn cookie-banner__btn--secondary"
            aria-label="Recusar cookies de analytics">
      Recusar analytics
    </button>
  </form>
</aside>
`))

// ackTmpl is the empty-but-not-empty fragment returned after a
// successful POST. HTMX swaps it into #cookie-banner replacing the
// banner element with a thin status line that screen readers
// announce. data-decision is exposed so a future enhancement can
// surface a "change your mind" link without re-rendering the page.
var ackTmpl = template.Must(template.New("cookie_ack").Parse(`<div id="cookie-banner"
     class="cookie-banner cookie-banner--ack"
     role="status"
     aria-live="polite"
     data-decision="{{.Decision}}">
  Preferência de cookies registrada.
</div>
`))

func init() {
	// Prime the lazy escaper (SIN-62774 race fix pattern).
	_ = bannerTmpl.Execute(io.Discard, bannerData{})
	_ = ackTmpl.Execute(io.Discard, ackData{})
}
