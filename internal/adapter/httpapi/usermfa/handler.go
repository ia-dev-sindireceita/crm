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
	"github.com/pericles-luz/crm/internal/tenancy"
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
// endpoints. Every field must be non-nil; NewHandler validates and
// panics on misconfiguration.
type HandlerConfig struct {
	Enroller      Enroller
	Verifier      Verifier
	Consumer      RecoveryConsumer
	Regenerator   RecoveryRegenerator
	Pendings      PendingStore
	Enrollment    EnrollmentChecker
	Reenroller    Reenroller
	SessionMinter SessionMinter
	// TenantSession is the OPTIONAL second access predicate for
	// /admin/2fa/setup: it resolves the post-login __Host-sess-tenant
	// cookie to a server-derived actor so an already-authenticated user
	// (no pending cookie) can reach the enrolment surface voluntarily
	// (SIN-65579 / SIN-65587). When nil the setup handler keeps its
	// pending-cookie-only behaviour, so router/handler tests that do not
	// wire it are unaffected. NewHandler does NOT require it.
	TenantSession    TenantSessionResolver
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

// setupActor is the resolved access decision for a /admin/2fa/setup
// request. Identity is always derived server-side — from the validated
// tenant session row (fromSession) or the pending-MFA row — never from
// request input.
type setupActor struct {
	userID      uuid.UUID
	tenantID    uuid.UUID
	nextPath    string
	fromSession bool // true = full post-login session; false = mid-login pending cookie
}

// Setup serves GET+POST /admin/2fa/setup.
//
// Access is granted by EITHER of two authenticated predicates
// (SIN-65579 / SIN-65587):
//
//  1. A full post-login tenant session (__Host-sess-tenant), resolved
//     server-side via TenantSession. This is the voluntary "Configurar
//     2FA" entry point from /hello-tenant and the user menu.
//  2. The mid-login __Host-mfa-pending cookie — the original, unchanged
//     forced-enrolment path.
//
// Guard against silent secret rotation: a full-session user who is ALREADY
// enrolled never reaches Enroll on a bare GET (that would upsert a fresh
// seed + invalidate recovery codes = self-lockout / DoS). They get the
// styled "2FA já ativo" page; rotating an existing secret requires a
// step-up (a current valid TOTP) on POST. First-time enrolment needs no
// step-up. When neither predicate matches, the handler redirects to the
// styled login page — it does NOT emit a raw 401 or a false 2fa_required
// audit row.
func (h *Handler) Setup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	act, ok := h.resolveSetupActor(w, r)
	if !ok {
		return
	}
	if !act.fromSession {
		// Mid-login pending path — behaviour unchanged: enrol directly.
		h.renderEnrollment(w, r, act)
		return
	}
	// Full-session path: guard against silent rotation of an existing
	// secret before ever calling Enroll.
	enrolled, err := h.cfg.Enrollment.IsEnrolled(r.Context(), act.userID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: enrollment check failed",
			slog.String("user_id", act.userID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !enrolled {
		// First-time enrolment for a logged-in user — no step-up.
		h.renderEnrollment(w, r, act)
		return
	}
	// Already enrolled. A bare GET must NOT rotate; POST rotates only
	// behind a valid current TOTP (step-up).
	if r.Method == http.MethodGet {
		h.renderAlreadyEnrolled(w, r, "", http.StatusOK)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderAlreadyEnrolled(w, r, "Requisição inválida.", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	if !isSixDigit(code) {
		h.renderAlreadyEnrolled(w, r, "Digite o código de 6 dígitos do seu autenticador atual.", http.StatusBadRequest)
		return
	}
	switch err := h.cfg.Verifier.Verify(r.Context(), act.userID, code); {
	case errors.Is(err, mfa.ErrInvalidCode):
		h.renderAlreadyEnrolled(w, r, "Código inválido. Tente novamente com um código atual.", http.StatusUnauthorized)
		return
	case err != nil:
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: step-up verify failed",
			slog.String("user_id", act.userID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Step-up passed — rotate the secret and render the fresh QR + codes.
	h.renderEnrollment(w, r, act)
}

// resolveSetupActor applies the dual access predicate for /admin/2fa/setup.
// It tries the full tenant session first (the voluntary post-login path),
// then the mid-login pending cookie. When neither resolves it redirects to
// the styled login page and returns ok=false — no raw 401, no 2fa_required
// audit row (those false signals are reserved for the verify/regenerate
// surfaces where the pending cookie is the ONLY predicate).
func (h *Handler) resolveSetupActor(w http.ResponseWriter, r *http.Request) (setupActor, bool) {
	if act, ok := h.resolveFullSession(r); ok {
		return act, true
	}
	if act, ok := h.resolvePendingActor(w, r); ok {
		return act, true
	}
	h.redirectToLogin(w, r)
	return setupActor{}, false
}

// resolveFullSession reads __Host-sess-tenant, parses it, and validates it
// against the host-resolved tenant via TenantSession. Any failure (no
// resolver wired, missing/malformed cookie, no tenant scope, or
// ErrNoTenantSession) returns ok=false so the caller falls through to the
// pending predicate. The actor's NextPath is the post-enrolment fallback
// (FallbackOK) because a full-session user has no pending ?next= row.
func (h *Handler) resolveFullSession(r *http.Request) (setupActor, bool) {
	if h.cfg.TenantSession == nil {
		return setupActor{}, false
	}
	raw, err := sessioncookie.Read(r, sessioncookie.NameTenant)
	if err != nil {
		return setupActor{}, false
	}
	sessionID, err := uuid.Parse(raw)
	if err != nil {
		return setupActor{}, false
	}
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		return setupActor{}, false
	}
	actor, err := h.cfg.TenantSession.ResolveTenantSession(r.Context(), tenant.ID, sessionID)
	if err != nil {
		return setupActor{}, false
	}
	return setupActor{
		userID:      actor.UserID,
		tenantID:    actor.TenantID,
		nextPath:    h.cfg.FallbackOK,
		fromSession: true,
	}, true
}

// resolvePendingActor resolves the __Host-mfa-pending cookie WITHOUT the
// audit + 401 side effects of requirePending — for the setup surface a
// missing/expired pending cookie on a non-session visit is a styled
// login redirect, not a bypass attempt. Expired rows are still purged and
// the stale cookie cleared.
func (h *Handler) resolvePendingActor(w http.ResponseWriter, r *http.Request) (setupActor, bool) {
	raw, err := sessioncookie.Read(r, sessioncookie.NameTenantPending)
	if err != nil {
		return setupActor{}, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return setupActor{}, false
	}
	row, err := h.cfg.Pendings.Get(r.Context(), id)
	if err != nil {
		return setupActor{}, false
	}
	if row.IsExpired(h.cfg.Now()) {
		_ = h.cfg.Pendings.Delete(r.Context(), id)
		sessioncookie.ClearTenantPending(w)
		return setupActor{}, false
	}
	return setupActor{
		userID:   row.UserID,
		tenantID: row.TenantID,
		nextPath: row.NextPath,
	}, true
}

// renderEnrollment enrols (or rotates) the actor's TOTP secret and renders
// the QR + recovery codes. This is the single Enroll call site for both
// the pending path and the full-session first-enrolment / post-step-up
// paths, so the rotation rule lives in exactly one place.
func (h *Handler) renderEnrollment(w http.ResponseWriter, r *http.Request, act setupActor) {
	label, err := h.cfg.Labels.LookupLabel(r.Context(), act.tenantID, act.userID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: lookup label failed",
			slog.String("user_id", act.userID.String()),
			slog.String("err", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	res, err := h.cfg.Enroller.Enroll(r.Context(), act.userID, label)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: enroll failed",
			slog.String("user_id", act.userID.String()),
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
		NextPath:      act.nextPath,
		CSRFToken:     "", // populated by upstream CSRF middleware on render
	}
	if err := h.tmpl.ExecuteTemplate(w, "setup.html", data); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: setup template render failed",
			slog.String("err", err.Error()),
		)
	}
}

// renderAlreadyEnrolled renders the styled "2FA já está ativo" page for a
// full-session user who already has a secret. It NEVER calls Enroll, so a
// GET cannot rotate the secret. The page offers a step-up form (enter a
// current TOTP) that POSTs back to /admin/2fa/setup to deliberately
// re-enrol.
func (h *Handler) renderAlreadyEnrolled(w http.ResponseWriter, r *http.Request, errMsg string, status int) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	data := alreadyEnrolledViewData{
		ErrorMessage:     errMsg,
		CSRFToken:        "", // populated by upstream CSRF middleware on render
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
	}
	if err := h.tmpl.ExecuteTemplate(w, "already_enrolled.html", data); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "usermfa: already-enrolled template render failed",
			slog.String("err", err.Error()),
		)
	}
}

// redirectToLogin 303s to the styled login page with the original path as
// ?next= so the user lands back on /admin/2fa/setup after signing in. Used
// when neither setup access predicate resolves.
func (h *Handler) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	target := h.cfg.LoginPath + "?next=" + url.QueryEscape(r.URL.RequestURI())
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

type alreadyEnrolledViewData struct {
	ErrorMessage     string
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
}
