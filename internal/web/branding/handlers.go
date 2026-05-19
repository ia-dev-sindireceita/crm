package branding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// MaxLogoBytes is the per-upload byte ceiling enforced by
// http.MaxBytesReader on POST /branding/logo. 2 MiB matches the spec
// in SIN-63084 (and the ADR-0080 tenant-logo cap). Requests larger than
// this surface as HTTP 413.
const MaxLogoBytes = 2 << 20

// allowedContentTypes is the closed allowlist for the uploaded logo's
// declared and sniffed content type. SVG is intentionally excluded
// today (ADR-0060) because the mediancut adapter only decodes raster
// formats; defence-in-depth here keeps a future SVG decoder behind an
// explicit additions.
var allowedContentTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
}

// hexColorRE is the strict #RRGGBB regex used for every palette
// override / save input. Three-letter and uppercase forms are
// accepted at parse time via ToLower before matching.
var hexColorRE = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// paletteSlots lists every form field name the page round-trips.
// The order matters for deterministic template rendering.
var paletteSlots = []string{
	"primary",
	"secondary",
	"accent",
	"foreground",
	"background",
	"text_on_primary",
}

// CSRFTokenFn returns the per-request CSRF token the handler embeds
// into every form + hx-headers attr. An empty token aborts the
// request with 500 because the middleware contract guarantees a
// session for every authenticated route.
type CSRFTokenFn func(*http.Request) string

// CacheInvalidator is the optional port the save / revert flows call
// to drop the per-tenant theme middleware cache so the next request
// sees the new palette without waiting for the TTL. cmd/server wires
// it to (*middleware.ThemeMiddleware).Invalidate; nil keeps the
// handler functional and skips the invalidation step.
type CacheInvalidator interface {
	Invalidate(tenantID uuid.UUID) bool
}

// Deps bundles the handler's collaborators. Required ports are
// nil-checked at construction so a misconfigured wire fails boot.
type Deps struct {
	Extractor  branding.PaletteExtractor
	Store      branding.PaletteStore
	Writer     branding.PaletteWriter
	CSRFToken  CSRFTokenFn
	ThemeCache CacheInvalidator
	Logger     *slog.Logger
}

// Handler serves the SIN-63084 HTMX surface.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Nil required ports are rejected so the
// boot path surfaces misconfiguration immediately.
func New(deps Deps) (*Handler, error) {
	if deps.Extractor == nil {
		return nil, errors.New("web/branding: Extractor is required")
	}
	if deps.Store == nil {
		return nil, errors.New("web/branding: Store is required")
	}
	if deps.Writer == nil {
		return nil, errors.New("web/branding: Writer is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/branding: CSRFToken is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts every endpoint on mux. Go 1.22 method+pattern syntax
// scopes each route to a single verb so non-matching combinations 405
// at the mux boundary.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /branding", h.page)
	mux.HandleFunc("POST /branding/logo", h.uploadLogo)
	mux.HandleFunc("POST /branding/palette/override", h.override)
	mux.HandleFunc("POST /branding/palette/save", h.save)
	mux.HandleFunc("POST /branding/palette/revert", h.revert)
}

// page renders the full /branding page. The current palette comes
// from PaletteStore; an absent row falls back to DefaultPalette.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	token, ok := h.requireCSRFToken(w, r)
	if !ok {
		return
	}
	pal := h.loadPalette(r.Context(), tenant.ID)
	h.writeHTML(w, http.StatusOK, pageTmpl, pageData{
		TenantName: tenant.Name,
		CSRFMeta:   csrf.MetaTag(token),
		HXHeaders:  csrf.HXHeadersAttr(token),
		Preview:    previewFromPalette(pal),
	})
}

// uploadLogo handles POST /branding/logo. The multipart body is
// capped at MaxLogoBytes, the declared content-type and the
// magic-byte sniff are both checked against the allowlist, and the
// configured PaletteExtractor derives the palette.
func (h *Handler) uploadLogo(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireCSRFToken(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxLogoBytes)
	if err := r.ParseMultipartForm(MaxLogoBytes); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			h.writeInline(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("Arquivo maior que %d MB. Reduza o logo e tente novamente.", MaxLogoBytes>>20))
			return
		}
		h.writeInline(w, http.StatusBadRequest, "Falha ao ler o formulário de upload.")
		return
	}
	file, header, err := r.FormFile("logo")
	if err != nil {
		h.writeInline(w, http.StatusBadRequest, "Selecione um arquivo de logo antes de enviar.")
		return
	}
	defer file.Close()
	if header.Size > MaxLogoBytes {
		h.writeInline(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Arquivo maior que %d MB. Reduza o logo e tente novamente.", MaxLogoBytes>>20))
		return
	}
	declared := strings.ToLower(header.Header.Get("Content-Type"))
	if _, ok := allowedContentTypes[declared]; !ok {
		h.writeInline(w, http.StatusUnsupportedMediaType,
			"Formato não suportado. Envie um PNG ou JPEG.")
		return
	}
	buf, err := io.ReadAll(file)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "read upload", err)
		return
	}
	sniffed := http.DetectContentType(buf)
	if _, ok := allowedContentTypes[sniffed]; !ok {
		h.writeInline(w, http.StatusUnsupportedMediaType,
			"Bytes do arquivo não correspondem a um PNG ou JPEG válido.")
		return
	}
	pal, err := h.deps.Extractor.Extract(r.Context(), bytes.NewReader(buf), branding.Hint{
		ContentType: declared,
		MaxBytes:    MaxLogoBytes,
	})
	if err != nil {
		switch {
		case errors.Is(err, branding.ErrTooLarge):
			h.writeInline(w, http.StatusRequestEntityTooLarge,
				"Logo excede os limites do extrator.")
			return
		case errors.Is(err, branding.ErrUnsupportedFormat),
			errors.Is(err, branding.ErrInvalidImage):
			h.writeInline(w, http.StatusUnsupportedMediaType,
				"Não foi possível decodificar o logo enviado.")
			return
		default:
			h.deps.Logger.Warn("web/branding: extractor failed; using default palette",
				"tenant_id", tenant.ID, "err", err)
			pal = branding.DefaultPalette
		}
	}
	pal, _ = branding.EnsureWCAGAA(pal)
	h.writeHTML(w, http.StatusOK, previewTmpl, previewFromPalette(pal))
}

// override applies a single-slot hex change. The form round-trips the
// current palette as hidden inputs; the override field names the slot
// to mutate and the new hex value. WCAG AA is enforced server-side
// (AC #4): a foreground change that drops contrast against the new
// background is rejected with an inline error.
func (h *Handler) override(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.requireCSRFToken(w, r); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.writeInline(w, http.StatusBadRequest, "Falha ao ler o formulário.")
		return
	}
	if slot := strings.TrimSpace(r.Form.Get("slot")); !slotIsValid(slot) {
		h.writeInline(w, http.StatusBadRequest, "Slot inválido.")
		return
	}
	updated, perr := parsePalette(r)
	if perr != nil {
		h.writeInline(w, http.StatusUnprocessableEntity, perr.Error())
		return
	}
	if msg, ok := validatePaletteContrast(updated); !ok {
		h.writeInline(w, http.StatusUnprocessableEntity, msg)
		return
	}
	h.writeHTML(w, http.StatusOK, previewTmpl, previewFromPalette(updated))
}

// save persists the in-flight palette via the PaletteWriter and emits
// an OOB swap so the runtime theme middleware's cached style on the
// open page refreshes instantly (the cache is also invalidated server-
// side so the next request reads the new row).
func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireCSRFToken(w, r); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.writeInline(w, http.StatusBadRequest, "Falha ao ler o formulário.")
		return
	}
	pal, perr := parsePalette(r)
	if perr != nil {
		h.writeInline(w, http.StatusUnprocessableEntity, perr.Error())
		return
	}
	if msg, ok := validatePaletteContrast(pal); !ok {
		h.writeInline(w, http.StatusUnprocessableEntity, msg)
		return
	}
	pal.Source = branding.PaletteSourceManual
	if err := h.deps.Writer.SetForTenant(r.Context(), tenant.ID, pal); err != nil {
		h.fail(w, http.StatusInternalServerError, "persist palette", err)
		return
	}
	if h.deps.ThemeCache != nil {
		h.deps.ThemeCache.Invalidate(tenant.ID)
	}
	h.writeHTML(w, http.StatusOK, saveTmpl, saveData{
		Preview:    previewFromPalette(pal),
		ThemeStyle: branding.ThemeStyleFromPalette(pal),
		Message:    "Paleta salva com sucesso.",
	})
}

// revert drops any persisted overrides and re-emits the default
// palette. The OOB swap restores the default theme style on the
// open page; the next request reads ErrPaletteNotFound and falls
// back to DefaultPalette through the theme middleware.
func (h *Handler) revert(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireCSRFToken(w, r); !ok {
		return
	}
	if err := h.deps.Writer.DeleteForTenant(r.Context(), tenant.ID); err != nil {
		h.fail(w, http.StatusInternalServerError, "delete palette", err)
		return
	}
	if h.deps.ThemeCache != nil {
		h.deps.ThemeCache.Invalidate(tenant.ID)
	}
	pal := defaultedPalette()
	h.writeHTML(w, http.StatusOK, saveTmpl, saveData{
		Preview:    previewFromPalette(pal),
		ThemeStyle: branding.DefaultThemeStyle,
		Message:    "Paleta revertida para o padrão.",
	})
}

// loadPalette returns the stored palette or DefaultPalette when no
// row exists. Store errors other than ErrPaletteNotFound surface the
// default but log the failure so an operator can act on transient DB
// issues. The defaulted return carries Source=PaletteSourceUnknown so
// the page label distinguishes "no palette configured yet" from a
// real fallback emitted by the extractor.
func (h *Handler) loadPalette(ctx context.Context, tenantID uuid.UUID) branding.Palette {
	got, err := h.deps.Store.GetByTenantID(ctx, tenantID)
	switch {
	case err == nil:
		return got
	case errors.Is(err, branding.ErrPaletteNotFound):
		return defaultedPalette()
	default:
		h.deps.Logger.Warn("web/branding: store load failed; using default",
			"tenant_id", tenantID, "err", err)
		return defaultedPalette()
	}
}

// defaultedPalette returns DefaultPalette but with Source flipped to
// PaletteSourceUnknown so the UI displays "Padrão neutro" instead of
// the WCAG-fallback label. The colour values are unchanged.
func defaultedPalette() branding.Palette {
	p := branding.DefaultPalette
	p.Source = branding.PaletteSourceUnknown
	return p
}

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (*tenancy.Tenant, bool) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return nil, false
	}
	return tenant, true
}

func (h *Handler) requireCSRFToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing",
			errors.New("empty csrf token"))
		return "", false
	}
	return token, true
}

func (h *Handler) writeHTML(w http.ResponseWriter, status int, t htmlTemplate, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := t.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/branding: render", "err", err)
	}
}

func (h *Handler) writeInline(w http.ResponseWriter, status int, msg string) {
	h.writeHTML(w, status, errorTmpl, errorData{Message: msg})
}

func (h *Handler) fail(w http.ResponseWriter, status int, op string, err error) {
	h.deps.Logger.Error("web/branding: "+op, "err", err)
	http.Error(w, http.StatusText(status), status)
}

type htmlTemplate interface {
	Execute(w io.Writer, data any) error
}
