package mastermfa

import (
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// Regenerator is the slice of mfa.Service.RegenerateRecovery the
// regenerate handler depends on.
type Regenerator interface {
	RegenerateRecovery(ctx context.Context, userID uuid.UUID) ([]string, error)
}

// RegenerateHandlerConfig is the constructor input.
type RegenerateHandlerConfig struct {
	Regenerator Regenerator
	Logger      *slog.Logger
}

// RegenerateHandler renders POST /m/2fa/recovery/regenerate. The route
// is gated by RequireMasterMFA upstream so the master is already
// authenticated, enrolled, and verified-in-this-session by the time we
// run. The handler mints a fresh 10-code set, persists hashes,
// invalidates the prior set, fires audit + Slack alert, and renders
// the new codes ONCE in the response.
//
// Like the enrol handler, this is POST-only because the operation is
// non-idempotent (every call mints a new set) — an idempotent GET
// would invalidate a freshly-minted set on browser reload.
type RegenerateHandler struct {
	cfg  RegenerateHandlerConfig
	tmpl *template.Template
}

//go:embed templates/regenerate_result.html
var regenerateTemplates embed.FS

// NewRegenerateHandler validates inputs and parses the embedded
// template eagerly. nil deps panic at wire time.
func NewRegenerateHandler(cfg RegenerateHandlerConfig) *RegenerateHandler {
	if cfg.Regenerator == nil {
		panic("mastermfa: NewRegenerateHandler: Regenerator is nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	tmpl, err := template.ParseFS(regenerateTemplates, "templates/regenerate_result.html")
	if err != nil {
		panic("mastermfa: parse regenerate template: " + err.Error())
	}
	return &RegenerateHandler{cfg: cfg, tmpl: tmpl}
}

// ServeHTTP implements http.Handler.
func (h *RegenerateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	master, ok := MasterFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	codes, err := h.cfg.Regenerator.RegenerateRecovery(r.Context(), master.ID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: regenerate failed",
			slog.String("user_id", master.ID.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, "could not regenerate, please retry", http.StatusInternalServerError)
		return
	}

	// Cache headers: same as enrol — the response carries plaintext
	// recovery codes that MUST NOT live in any browser/proxy cache.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := regenerateViewData{
		RecoveryCodes: formatRecoveryCodes(codes),
	}
	if err := h.tmpl.ExecuteTemplate(w, "regenerate_result.html", data); err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "mastermfa: regenerate render failed",
			slog.String("error", err.Error()),
		)
	}
}

type regenerateViewData struct {
	RecoveryCodes []string
}
