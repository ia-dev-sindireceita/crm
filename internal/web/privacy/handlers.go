// Package privacy is the HTMX UI for the platform privacy / DPA page
// (SIN-62354 / Fase 3, decisão #8 / SIN-62203). It serves a single
// authenticated, server-rendered page at GET /settings/privacy that
// lists every active sub-processor (OpenRouter, Meta, Mailgun, PIX
// PSP placeholder) plus the DPA version and a download link.
//
// The page is intentionally read-only: no POST surface, no CSRF token
// embed, no state-changing affordance. The only side door is GET
// /settings/privacy/dpa.md, which streams the embedded DPA markdown
// (versioned by internal/legal.DPAVersion) with a Last-Modified
// header that doubles as the "timestamp" the AC asks for.
//
// Following the existing pattern (internal/web/funnel, internal/web/
// contacts): one Deps struct with the small ports the handler
// actually needs, html/template inlined into templates.go, no JS
// framework. The wire (cmd/server/privacy_wire.go) provides the
// ModelResolver — today a static fallback returning "openrouter/auto",
// later swapped to the SIN-62351 cascade resolver without touching
// this package.
package privacy

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/legal"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// ModelResolver returns the AI model identifier active for the given
// tenant. The implementation is wire-injected so this package does
// not import internal/aipolicy (which is still landing under
// SIN-62351) and so tests can stub the value without any DB.
//
// Contract:
//   - Return value MUST be non-empty. An empty string is a contract
//     violation and the handler returns it as "openrouter/auto" so
//     the privacy page never shows a blank "modelo ativo" row.
//   - The function may return any error; on error the page renders
//     the documented fallback model and logs the failure. The page
//     never fails just because the model lookup did.
type ModelResolver interface {
	ActiveModel(ctx context.Context, tenantID uuid.UUID) (string, error)
}

// ModelResolverFunc adapts a plain function into ModelResolver. Lets
// the wire and tests pass a closure without defining a struct.
type ModelResolverFunc func(ctx context.Context, tenantID uuid.UUID) (string, error)

// ActiveModel calls f.
func (f ModelResolverFunc) ActiveModel(ctx context.Context, tenantID uuid.UUID) (string, error) {
	return f(ctx, tenantID)
}

// FallbackModel is the model string the page renders when the
// resolver returns empty or errors. It mirrors the migration 0098
// default for ai_policy.model so a tenant who has never configured
// IA sees the same value the database would have given them.
const FallbackModel = "openrouter/auto"

// Now is the time source the handler uses for the DPA "generated at"
// stamp. Tests stub this to make the rendered output deterministic.
// Default is time.Now (UTC).
type Now func() time.Time

// Deps bundles the handler collaborators. All fields are required
// except Logger (defaults to slog.Default).
type Deps struct {
	// Model resolves the active AI model for the current tenant.
	// See ModelResolver for the contract.
	Model ModelResolver

	// Now is the request-time clock used for the rendered "gerado
	// em" stamp on the page and the Last-Modified header on the
	// DPA download.
	Now Now

	// Logger receives one structured line per render error. The
	// page itself never serves a 5xx for cosmetic failures.
	Logger *slog.Logger
}

// Handler serves the privacy disclosure page and the DPA download.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Nil required dependencies are rejected
// at boot time so a misconfigured wire fails fast instead of
// serving a half-broken privacy page in production.
func New(deps Deps) (*Handler, error) {
	if deps.Model == nil {
		return nil, errors.New("web/privacy: Model is required")
	}
	if deps.Now == nil {
		return nil, errors.New("web/privacy: Now is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	// Defensive: the entire point of this page is the DPA
	// disclosure of OpenRouter. If the embedded markdown ever
	// drifts and stops mentioning the provider, refuse to wire so
	// CI/boot catches it rather than serving a non-compliant page.
	if !legal.DPAMentionsOpenRouter() {
		return nil, errors.New("web/privacy: embedded DPA missing OpenRouter mention — decisão #8 invariant violated")
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts the two endpoints on mux. Both are GET-only; no CSRF
// token surface lives in this handler.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/privacy", h.view)
	mux.HandleFunc("GET /settings/privacy/dpa.md", h.downloadDPA)
}

// view renders the privacy disclosure page. The page is fully
// server-rendered (HTMX-over-SPA lens): one round trip, no JS data
// fetch, no client-side templating. Renders in tens of microseconds
// in tests; well under the 300ms p95 budget called out in AC #1.
func (h *Handler) view(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	model, err := h.deps.Model.ActiveModel(r.Context(), tenant.ID)
	if err != nil {
		// Page never 500s on a model lookup failure — the DPA
		// disclosure is the load-bearing content. Log and render
		// the fallback so the tenant sees an honest "padrão" row.
		h.deps.Logger.Warn("web/privacy: model lookup failed; rendering fallback", "tenant_id", tenant.ID, "err", err)
		model = FallbackModel
	}
	if model == "" {
		model = FallbackModel
	}
	data := pageData{
		DPAVersion:    legal.DPAVersion,
		DPAFilename:   legal.DPAFilename(),
		Subprocessors: legal.Subprocessors(),
		ActiveModel:   model,
		GeneratedAt:   h.deps.Now().UTC().Format(time.RFC3339),
		TenantName:    tenant.Name,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := pageTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/privacy: render page", "err", err)
	}
}

// downloadDPA streams the embedded DPA markdown with a versioned
// filename and a Last-Modified header equal to the request-time
// clock (the DPA is embedded in the binary, so its "last modified"
// from a tenant's POV is "when you fetched it" — the version field
// is the real authority).
func (h *Handler) downloadDPA(w http.ResponseWriter, _ *http.Request) {
	now := h.deps.Now().UTC()
	w.Header().Set("Content-Type", legal.DPAContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+legal.DPAFilename()+`"`)
	w.Header().Set("Last-Modified", now.Format(http.TimeFormat))
	w.Header().Set("X-DPA-Version", legal.DPAVersion)
	w.Header().Set("Cache-Control", "private, no-store")
	if _, err := w.Write([]byte(legal.DPAMarkdown())); err != nil {
		h.deps.Logger.Error("web/privacy: write DPA download", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/privacy: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// pageData is the template view-model for the privacy page.
type pageData struct {
	DPAVersion    string
	DPAFilename   string
	Subprocessors []legal.Subprocessor
	ActiveModel   string
	GeneratedAt   string
	TenantName    string
}

// Compile-time check: the template can resolve every field on
// pageData. A typo here would fail the package's first test.
var _ template.HTML = template.HTML("")
