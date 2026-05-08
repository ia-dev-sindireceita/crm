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
	ConsumeRecovery(ctx context.Context, userID uuid.UUID, submitted string) error
}

// VerifyHandlerConfig is the constructor input.
type VerifyHandlerConfig struct {
	Verifier   Verifier
	Consumer   RecoveryConsumer
	Sessions   MasterSessionMFA
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
		err := h.cfg.Consumer.ConsumeRecovery(r.Context(), master.ID, code)
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

	if err := h.cfg.Sessions.MarkVerified(w, r); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: session mark verified failed",
			slog.String("user_id", master.ID.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
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
