package mastermfa

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// Verifier is the slice of mfa.Service.Verify the verify handler
// needs.
type Verifier interface {
	Verify(ctx context.Context, userID uuid.UUID, code string) error
}

// RecoveryConsumer is the slice of mfa.Service.ConsumeRecovery the
// verify handler needs.
type RecoveryConsumer interface {
	ConsumeRecovery(ctx context.Context, userID uuid.UUID, submitted string, reqCtx mfa.RequestContext) error
}

// VerifyHandlerConfig is the constructor input.
type VerifyHandlerConfig struct {
	Verifier Verifier
	Consumer RecoveryConsumer
	Sessions MasterSessionMFA

	// Rotator, when non-nil, replaces the post-success MarkVerified
	// call with a session-id rotation: the pre-MFA cookie is swapped
	// for a fresh CSPRNG id and mfa_verified_at is stamped on the new
	// row in a single transaction (ADR 0073 §D3, SIN-62377 / FAIL-4).
	// cmd/server wires the production HTTPSession adapter, which
	// implements both MasterSessionMFA and MasterSessionRotator. Tests
	// that don't exercise rotation behaviour leave this nil and fall
	// through to the legacy MarkVerified path.
	Rotator MasterSessionRotator

	Logger     *slog.Logger
	FallbackOK string // destination after a successful verify when ?return= is absent or unsafe
}

// VerifyHandler renders POST /m/2fa/verify. The form carries a single
// `code` field that may be either a six-digit TOTP code OR a
// 10-character (optionally dashed) recovery code. The handler
// dispatches by shape: six-digit numeric goes to Verifier.Verify;
// anything else falls through to RecoveryConsumer.ConsumeRecovery.
//
// Both flows collapse to ErrInvalidCode on mismatch — the response
// renders a single uniform error message ("código inválido") so a
// hostile prober cannot distinguish "wrong TOTP" from "wrong recovery"
// from "code in wrong format".
//
// On success the handler:
//  1. Calls Sessions.MarkVerified to flip the session bit.
//  2. Redirects 303 to the validated `?return=` (or FallbackOK).
//
// CSRF protection is supplied by the upstream RequireCSRF middleware
// at router wire-time; this handler does not re-check the token.
type VerifyHandler struct {
	cfg  VerifyHandlerConfig
	tmpl *template.Template
}

//go:embed templates/verify.html
var verifyTemplates embed.FS

// NewVerifyHandler validates inputs and parses the embedded template
// eagerly. Misconfiguration panics at wire time.
func NewVerifyHandler(cfg VerifyHandlerConfig) *VerifyHandler {
	if cfg.Verifier == nil {
		panic("mastermfa: NewVerifyHandler: Verifier is nil")
	}
	if cfg.Consumer == nil {
		panic("mastermfa: NewVerifyHandler: Consumer is nil")
	}
	if cfg.Sessions == nil {
		panic("mastermfa: NewVerifyHandler: Sessions is nil")
	}
	if cfg.FallbackOK == "" {
		cfg.FallbackOK = "/m/"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	tmpl, err := template.ParseFS(verifyTemplates, "templates/verify.html")
	if err != nil {
		panic("mastermfa: parse verify template: " + err.Error())
	}
	return &VerifyHandler{cfg: cfg, tmpl: tmpl}
}

// ServeHTTP implements http.Handler.
func (h *VerifyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.renderForm(w, r, "")
	case http.MethodPost:
		h.handlePost(w, r)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *VerifyHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	master, ok := MasterFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("code"))
	if code == "" {
		h.renderForm(w, r, "código inválido")
		return
	}

	// Shape-based dispatch. Six-digit numeric → TOTP. Otherwise →
	// recovery (the consumer normalises and refuses non-base32 shapes).
	if isSixDigit(code) {
		err := h.cfg.Verifier.Verify(r.Context(), master.ID, code)
		if errors.Is(err, mfa.ErrInvalidCode) {
			h.renderForm(w, r, "código inválido")
			return
		}
		if err != nil {
			h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: verify failed",
				slog.String("user_id", master.ID.String()),
				slog.String("error", err.Error()),
			)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	} else {
		reqCtx := mfa.RequestContext{
			IP:        clientIP(r),
			UserAgent: r.Header.Get("User-Agent"),
			Route:     r.URL.Path,
		}
		err := h.cfg.Consumer.ConsumeRecovery(r.Context(), master.ID, code, reqCtx)
		if errors.Is(err, mfa.ErrInvalidCode) {
			h.renderForm(w, r, "código inválido")
			return
		}
		if err != nil {
			h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: consume recovery failed",
				slog.String("user_id", master.ID.String()),
				slog.String("error", err.Error()),
			)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	// SIN-62377 (FAIL-4): when a Rotator is wired, swap the pre-MFA
	// session id for a fresh post-MFA id (CSPRNG-minted) and stamp
	// mfa_verified_at on the new row in a single tx so a passive
	// observer who saw the pre-MFA cookie cannot ride it past MFA.
	// The rotator also re-issues the __Host-sess-master cookie. When
	// no Rotator is wired (older test wireups), fall through to the
	// legacy MarkVerified path so existing behaviour is preserved.
	var markErr error
	if h.cfg.Rotator != nil {
		markErr = h.cfg.Rotator.RotateAndMarkVerified(w, r)
	} else {
		markErr = h.cfg.Sessions.MarkVerified(w, r)
	}
	if markErr != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: session mark verified failed",
			slog.String("user_id", master.ID.String()),
			slog.String("rotated", boolStr(h.cfg.Rotator != nil)),
			slog.String("error", markErr.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if h.cfg.Rotator != nil {
		// Audit trail: master_session row swap is auto-logged by the
		// migration-0002 master_ops_audit_trigger (insert + delete on
		// master_session); this slog line carries the human-friendly
		// label so dashboards can dedupe rotation-driven row churn.
		h.cfg.Logger.InfoContext(r.Context(), "mastermfa: session rotated on 2fa success",
			slog.String("user_id", master.ID.String()),
			slog.String("event", "master_session_rotated_2fa"),
		)
	}

	target := ResolveReturn(r.URL.Query().Get("return"), h.cfg.FallbackOK)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// renderForm writes the verify page with an optional error message.
// Cache headers prevent caching so a Back-button after a failure
// hits the server again rather than re-submitting a stale form.
func (h *VerifyHandler) renderForm(w http.ResponseWriter, r *http.Request, errMsg string) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg != "" {
		// Re-render with 401 so a CLI / API client gets a useful status
		// even though the HTML body still works for a browser.
		w.WriteHeader(http.StatusUnauthorized)
	}
	data := verifyViewData{
		ErrorMessage: errMsg,
		ReturnPath:   ResolveReturn(r.URL.Query().Get("return"), ""),
	}
	if err := h.tmpl.ExecuteTemplate(w, "verify.html", data); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: verify template render failed",
			slog.String("error", err.Error()),
		)
	}
}

type verifyViewData struct {
	ErrorMessage string
	ReturnPath   string
}

// isSixDigit reports whether s is exactly six ASCII decimal digits.
// Cheaper than running totp's own malformed-input rejection here —
// the shape is the dispatch key, not a security check.
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

// boolStr turns a bool into a slog-friendly "true"/"false" string. The
// SIN-62377 verify-success log line uses it so dashboards can split
// rotated vs. legacy MarkVerified paths during the rollout window.
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
