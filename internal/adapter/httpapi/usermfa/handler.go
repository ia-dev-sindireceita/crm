package usermfa

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

//go:embed templates/setup.html templates/verify.html templates/regenerate_result.html templates/already_enrolled.html
var templatesFS embed.FS

// DefaultLockoutThreshold is the 5-strikes value mandated by AC #8.
const DefaultLockoutThreshold = 5

// DefaultLockoutWindow is the 15-minute brute-force window from AC #8.
const DefaultLockoutWindow = 15 * time.Minute

// DefaultSessionTTL is the post-verify tenant session lifetime applied
// when SessionMinter does not override.
const DefaultSessionTTL = 8 * time.Hour

// HandlerConfig wires the collaborators needed by the user-side 2FA
// endpoints. Every field listed in NewHandler's missing-collaborator
// check must be non-nil; NewHandler validates and returns an error on
// misconfiguration. TenantSession is the lone OPTIONAL field — leaving
// it nil keeps the pre-PR2 pending-cookie-only behaviour for Setup.
type HandlerConfig struct {
	Enroller    Enroller
	Verifier    Verifier
	Consumer    RecoveryConsumer
	Regenerator RecoveryRegenerator
	Pendings    PendingStore
	Enrollment  EnrollmentChecker
	Reenroller  Reenroller
	// TenantSession is the optional full-session entry predicate for
	// GET /admin/2fa/setup (SIN-65587 / PR2 of SIN-63361). When non-nil,
	// Setup first tries to resolve an already-authenticated tenant
	// session before falling back to the mid-login pending cookie, so a
	// logged-in user clicking "Configurar 2FA" reaches enrolment instead
	// of the bare 401. When nil, Setup is pending-only (legacy behaviour).
	TenantSession    TenantSessionResolver
	SessionMinter    SessionMinter
	Failures         FailureCounter
	Audit            AuditEmitter
	Labels           UserLabelReader
	LockoutThreshold int
	LockoutWindow    time.Duration
	FallbackOK       string
	LoginPath        string
	Logger           *slog.Logger
	Now              func() time.Time
}

// Handler bundles the GET/POST handlers for the four /admin/2fa/...
// routes. Mount returns a chi-friendly subrouter.
type Handler struct {
	cfg  HandlerConfig
	tmpl *template.Template
}

// NewHandler validates inputs, parses the embedded templates, and
// returns the handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	missing := []string{}
	if cfg.Enroller == nil {
		missing = append(missing, "Enroller")
	}
	if cfg.Verifier == nil {
		missing = append(missing, "Verifier")
	}
	if cfg.Consumer == nil {
		missing = append(missing, "Consumer")
	}
	if cfg.Regenerator == nil {
		missing = append(missing, "Regenerator")
	}
	if cfg.Pendings == nil {
		missing = append(missing, "Pendings")
	}
	if cfg.Enrollment == nil {
		missing = append(missing, "Enrollment")
	}
	if cfg.Reenroller == nil {
		missing = append(missing, "Reenroller")
	}
	if cfg.SessionMinter == nil {
		missing = append(missing, "SessionMinter")
	}
	if cfg.Failures == nil {
		missing = append(missing, "Failures")
	}
	if cfg.Audit == nil {
		missing = append(missing, "Audit")
	}
	if cfg.Labels == nil {
		missing = append(missing, "Labels")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("usermfa: NewHandler missing collaborators: %s", strings.Join(missing, ", "))
	}
	if cfg.LockoutThreshold <= 0 {
		cfg.LockoutThreshold = DefaultLockoutThreshold
	}
	if cfg.LockoutWindow <= 0 {
		cfg.LockoutWindow = DefaultLockoutWindow
	}
	if cfg.FallbackOK == "" {
		cfg.FallbackOK = "/hello-tenant"
	}
	if cfg.LoginPath == "" {
		cfg.LoginPath = "/login"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	tmpl, err := template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("usermfa: parse templates: %w", err)
	}
	return &Handler{cfg: cfg, tmpl: tmpl}, nil
}

// Setup serves GET+POST /admin/2fa/setup.
//
// Access is granted by either of two authenticated predicates (SIN-65587,
// PR2 of SIN-63361):
//
//  1. A full post-login tenant session (__Host-sess-tenant), resolved by
//     the optional TenantSession port. This is the voluntary path: a
//     logged-in user who clicked "Configurar 2FA" on /hello-tenant
//     (the SIN-65579 case).
//  2. A mid-login pending cookie (__Host-mfa-pending), the forced path
//     for users whose tenant policy requires TOTP before the session is
//     minted. Unchanged from the original flow.
//
// When neither predicate holds the handler issues a styled 303 to the
// login page (never the bare "<p>2FA required</p>" 401) and does NOT emit
// a 2fa_required audit row — an unauthenticated visit to an enrolment URL
// is not a bypass attempt and would otherwise pollute the security ledger
// with false positives.
func (h *Handler) Setup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Predicate 1: voluntary full-session entry.
	if h.cfg.TenantSession != nil {
		if actor, ok := h.cfg.TenantSession.ResolveTenantSession(r); ok {
			h.setupFullSession(w, r, actor)
			return
		}
	}

	// Predicate 2: forced mid-login pending cookie. Read-only resolve —
	// no deny/audit/clear side effects, so a missing or expired cookie
	// falls through to the styled login redirect instead of the bare 401.
	if pending, ok := h.pendingActor(r); ok {
		// Mid-login users are by definition mid-enrolment; render the
		// enrolment UI. (A pending user who is already enrolled is routed
		// to /verify by the login wrapper, never here.)
		h.renderEnrollment(w, r, pending.UserID, pending.TenantID, pending.NextPath)
		return
	}

	// Neither predicate: not logged in and not mid-login. Bounce to the
	// styled login page with next= back to setup. No false audit.
	h.redirectToLoginForSetup(w, r)
}

// setupFullSession handles /admin/2fa/setup for an authenticated tenant
// session. CRITICAL (SIN-65587 security AC): when the user is already
// enrolled we MUST NOT call Enroll — mfa.Service.Enroll rotates the TOTP
// seed and invalidates the recovery codes unconditionally, so a stray GET
// would silently lock the user out of their authenticator (self-DoS).
// Already-enrolled users get a styled "2FA já está ativo" page instead;
// only never-enrolled users proceed to first-time enrolment.
func (h *Handler) setupFullSession(w http.ResponseWriter, r *http.Request, actor TenantActor) {
	enrolled, err := h.cfg.Enrollment.IsEnrolled(r.Context(), actor.UserID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: setup enrollment check failed",
			slog.String("user_id", actor.UserID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if enrolled {
		h.renderAlreadyEnrolled(w, r)
		return
	}
	// First-time enrolment via the full session: there is no pending
	// cookie carrying a NextPath, so land the user back on the post-login
	// surface after they confirm.
	h.renderEnrollment(w, r, actor.UserID, actor.TenantID, h.cfg.FallbackOK)
}

// renderEnrollment runs the label lookup + Enroll + setup.html render
// shared by the pending (mid-login) and full-session (never-enrolled)
// paths. Callers MUST have already established that the user is NOT yet
// enrolled before invoking this — it calls Enroll, which rotates seeds.
func (h *Handler) renderEnrollment(w http.ResponseWriter, r *http.Request, userID, tenantID uuid.UUID, nextPath string) {
	label, err := h.cfg.Labels.LookupLabel(r.Context(), tenantID, userID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: lookup label failed",
			slog.String("user_id", userID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	res, err := h.cfg.Enroller.Enroll(r.Context(), userID, label)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: enroll failed",
			slog.String("user_id", userID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	formatted := make([]string, len(res.RecoveryCodes))
	for i, c := range res.RecoveryCodes {
		formatted[i] = mfa.FormatRecoveryCode(c)
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := setupViewData{
		OTPAuthURI:    res.OTPAuthURI,
		SecretEncoded: res.SecretEncoded,
		RecoveryCodes: formatted,
		NextPath:      nextPath,
		CSRFToken:     "", // populated by upstream CSRF middleware on render
	}
	if err := h.tmpl.ExecuteTemplate(w, "setup.html", data); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: setup template render failed",
			slog.String("err", err.Error()),
		)
	}
}

// renderAlreadyEnrolled serves the styled "2FA já está ativo" page for a
// full-session user who is already enrolled. It deliberately does NOT
// touch Enroll or Regenerate so the existing TOTP seed and recovery codes
// stay intact (SIN-65587 anti-rotation guard). Re-enrolment of an
// existing secret is a step-up flow tracked as follow-up.
func (h *Handler) renderAlreadyEnrolled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := h.tmpl.ExecuteTemplate(w, "already_enrolled.html", nil); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: already-enrolled template render failed",
			slog.String("err", err.Error()),
		)
	}
}

// pendingActor resolves the __Host-mfa-pending cookie to a Pending row
// WITHOUT any deny/audit/clear side effects. It is the read-only sibling
// of requirePending, used by Setup so a missing or expired pending cookie
// falls through to the styled login redirect instead of the bare 401 +
// false 2fa_required audit that requirePending emits for the mid-login
// Verify/Regenerate routes.
func (h *Handler) pendingActor(r *http.Request) (Pending, bool) {
	raw, err := sessioncookie.Read(r, sessioncookie.NameTenantPending)
	if err != nil {
		return Pending{}, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return Pending{}, false
	}
	row, err := h.cfg.Pendings.Get(r.Context(), id)
	if err != nil {
		return Pending{}, false
	}
	if row.IsExpired(h.cfg.Now()) {
		return Pending{}, false
	}
	return row, true
}

// redirectToLoginForSetup issues the styled 303 that replaces the bare
// 401 the no-credential path used to short-circuit. next= points back at
// the setup surface so the user lands on enrolment after authenticating.
func (h *Handler) redirectToLoginForSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	target := h.cfg.LoginPath + "?next=" + url.QueryEscape("/admin/2fa/setup")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// Verify serves GET+POST /admin/2fa/verify.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	pending, ok := h.requirePending(w, r, "/admin/2fa/verify")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.renderVerifyForm(w, r, pending, "", http.StatusOK)
		return
	case http.MethodPost:
		h.handleVerifyPost(w, r, pending)
		return
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Regenerate serves POST /admin/2fa/regenerate. AC #5: regen requires
// the caller to have already passed a recent TOTP verify on an active
// pending session — i.e. the pending cookie is the proof of ownership
// for this PR (the post-MFA tenant session route is added in PR2 of
// the milestone; for now, holding a verified pending cookie is the
// access predicate). The handler revalidates the cookie, calls
// RegenerateRecovery, and renders the fresh codes once.
func (h *Handler) Regenerate(w http.ResponseWriter, r *http.Request) {
	pending, ok := h.requirePending(w, r, "/admin/2fa/regenerate")
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqCtx := mfa.RequestContext{
		IP:        remoteIP(r),
		UserAgent: r.Header.Get("User-Agent"),
		Route:     r.URL.Path,
	}
	codes, err := h.cfg.Regenerator.RegenerateRecovery(r.Context(), pending.UserID, reqCtx)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: regenerate failed",
			slog.String("user_id", pending.UserID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	formatted := make([]string, len(codes))
	for i, c := range codes {
		formatted[i] = mfa.FormatRecoveryCode(c)
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "regenerate_result.html", regenerateViewData{RecoveryCodes: formatted}); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: regenerate template render failed",
			slog.String("err", err.Error()),
		)
	}
}

// requirePending resolves the __Host-mfa-pending cookie to a Pending
// row. When the cookie is missing, malformed, expired, or the row is
// gone, writes a 401, emits a 2fa_required audit row, and returns
// ok=false. Callers MUST stop on ok=false.
func (h *Handler) requirePending(w http.ResponseWriter, r *http.Request, route string) (Pending, bool) {
	raw, err := sessioncookie.Read(r, sessioncookie.NameTenantPending)
	if err != nil {
		h.denyMFA(r.Context(), w, uuid.Nil, route, "missing_pending_cookie")
		return Pending{}, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		h.denyMFA(r.Context(), w, uuid.Nil, route, "malformed_pending_cookie")
		return Pending{}, false
	}
	row, err := h.cfg.Pendings.Get(r.Context(), id)
	if err != nil {
		h.denyMFA(r.Context(), w, uuid.Nil, route, "pending_lookup_failed")
		return Pending{}, false
	}
	if row.IsExpired(h.cfg.Now()) {
		_ = h.cfg.Pendings.Delete(r.Context(), id)
		sessioncookie.ClearTenantPending(w)
		h.denyMFA(r.Context(), w, row.UserID, route, "pending_expired")
		return Pending{}, false
	}
	return row, true
}

// denyMFA writes the 401, clears any leftover pending cookie, and
// emits a 2fa_required audit row.
func (h *Handler) denyMFA(ctx context.Context, w http.ResponseWriter, userID uuid.UUID, route, reason string) {
	sessioncookie.ClearTenantPending(w)
	if h.cfg.Audit != nil {
		if err := h.cfg.Audit.LogMFARequired(ctx, userID, route, reason); err != nil {
			h.cfg.Logger.WarnContext(ctx, "usermfa: audit emit failed",
				slog.String("err", err.Error()),
				slog.String("route", route),
				slog.String("reason", reason),
			)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("<p>2FA required</p>\n"))
}

func (h *Handler) handleVerifyPost(w http.ResponseWriter, r *http.Request, pending Pending) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	if code == "" {
		h.handleWrongCode(w, r, pending, "empty_code")
		return
	}
	enrolled, err := h.cfg.Enrollment.IsEnrolled(r.Context(), pending.UserID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: enrollment check failed",
			slog.String("user_id", pending.UserID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !enrolled {
		// AC #3 — user must enroll first; the setup page calls Enroll
		// and renders the QR + codes inline. Redirect with the pending
		// cookie still attached.
		http.Redirect(w, r, "/admin/2fa/setup", http.StatusSeeOther)
		return
	}
	var verifyErr error
	if isSixDigit(code) {
		verifyErr = h.cfg.Verifier.Verify(r.Context(), pending.UserID, code)
	} else {
		reqCtx := mfa.RequestContext{
			IP:        remoteIP(r),
			UserAgent: r.Header.Get("User-Agent"),
			Route:     r.URL.Path,
		}
		verifyErr = h.cfg.Consumer.ConsumeRecovery(r.Context(), pending.UserID, code, reqCtx)
	}
	if errors.Is(verifyErr, mfa.ErrInvalidCode) {
		h.handleWrongCode(w, r, pending, "invalid_code")
		return
	}
	if errors.Is(verifyErr, mfa.ErrSeedCipherDecode) {
		// The stored seed ciphertext is unreadable under the current
		// SeedCipher key — almost always the aftermath of an operator
		// rotating USERMFA_SEED_KEY. Flip the row into reenroll_required
		// so IsEnrolled returns false on the next request, then redirect
		// the user to the setup surface with the pending cookie intact.
		// Logged at WARN (not ERROR) because this is operator-driven and
		// self-heals on re-enrol — it should not page on-call, but it
		// is SIEM-visible as a signal that a recent key rotation is
		// stranding users.
		if reErr := h.cfg.Reenroller.MarkReenrollRequired(r.Context(), pending.UserID); reErr != nil {
			h.cfg.Logger.ErrorContext(r.Context(), "usermfa: mark reenroll-required failed after stale-ciphertext detection",
				slog.String("user_id", pending.UserID.String()),
				slog.String("err", reErr.Error()),
			)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		h.cfg.Logger.WarnContext(r.Context(), "usermfa: stored seed unreadable under current USERMFA_SEED_KEY, forcing re-enrollment",
			slog.String("user_id", pending.UserID.String()),
			slog.String("tenant_id", pending.TenantID.String()),
		)
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
		http.Redirect(w, r, "/admin/2fa/setup", http.StatusSeeOther)
		return
	}
	if verifyErr != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: verify failed",
			slog.String("user_id", pending.UserID.String()),
			slog.String("err", verifyErr.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Success: reset the failure counter, mint the real session,
	// clear the pending cookie + row.
	if err := h.cfg.Failures.Reset(r.Context(), pending.ID); err != nil {
		h.cfg.Logger.WarnContext(r.Context(), "usermfa: failure reset failed",
			slog.String("err", err.Error()),
		)
	}
	sess, err := h.cfg.SessionMinter.MintTenantSession(r.Context(), pending.TenantID, pending.UserID, remoteIP(r), r.UserAgent())
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: mint session failed",
			slog.String("user_id", pending.UserID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := h.cfg.Pendings.Delete(r.Context(), pending.ID); err != nil {
		h.cfg.Logger.WarnContext(r.Context(), "usermfa: pending delete failed",
			slog.String("err", err.Error()),
		)
	}
	sessioncookie.ClearTenantPending(w)
	sessioncookie.SetTenant(w, sess.ID.String(), 0)
	if sess.CSRFToken != "" {
		sessioncookie.SetCSRF(w, sess.CSRFToken, 0)
	}
	target := safeNext(pending.NextPath, h.cfg.FallbackOK)
	if n := strings.TrimSpace(r.PostForm.Get("next")); n != "" {
		target = safeNext(n, target)
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) handleWrongCode(w http.ResponseWriter, r *http.Request, pending Pending, reason string) {
	count, err := h.cfg.Failures.Increment(r.Context(), pending.ID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: failure increment failed",
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if count >= h.cfg.LockoutThreshold {
		// AC #8 brute-force lockout: 5 wrong codes within the window →
		// delete the pending row, fire the audit, return 429 with
		// Retry-After. The user must re-login.
		if err := h.cfg.Pendings.Delete(r.Context(), pending.ID); err != nil {
			h.cfg.Logger.WarnContext(r.Context(), "usermfa: pending delete on lockout failed",
				slog.String("err", err.Error()),
			)
		}
		_ = h.cfg.Failures.Reset(r.Context(), pending.ID)
		sessioncookie.ClearTenantPending(w)
		if h.cfg.Audit != nil {
			_ = h.cfg.Audit.LogMFARequired(r.Context(), pending.UserID, r.URL.Path, fmt.Sprintf("lockout_%s", reason))
		}
		secs := int64(h.cfg.LockoutWindow.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("<p>muitas tentativas; tente novamente em 15 minutos</p>\n"))
		return
	}
	h.renderVerifyForm(w, r, pending, "código inválido", http.StatusUnauthorized)
}

func (h *Handler) renderVerifyForm(w http.ResponseWriter, r *http.Request, pending Pending, errMsg string, status int) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := verifyViewData{
		ErrorMessage:     errMsg,
		NextPath:         pending.NextPath,
		CSRFToken:        "",
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
	}
	if err := h.tmpl.ExecuteTemplate(w, "verify.html", data); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: verify template render failed",
			slog.String("err", err.Error()),
		)
	}
}

// safeNext clamps the post-verify redirect to a same-origin path.
// Mirrors handler.SanitizeNext from the password-only login surface.
func safeNext(raw, fallback string) string {
	if raw == "" {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fallback
	}
	if u.IsAbs() || u.Host != "" {
		return fallback
	}
	if !strings.HasPrefix(u.Path, "/") {
		return fallback
	}
	return u.RequestURI()
}

func isSixDigit(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type setupViewData struct {
	OTPAuthURI    string
	SecretEncoded string
	RecoveryCodes []string
	NextPath      string
	CSRFToken     string
}

type verifyViewData struct {
	ErrorMessage     string
	NextPath         string
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
}

type regenerateViewData struct {
	RecoveryCodes []string
}
