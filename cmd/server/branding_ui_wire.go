package main

// SIN-63084 + SIN-63085 + SIN-63101 wiring — HTMX branding admin UI
// plus the per-tenant theme middleware that paints every downstream
// page with the persisted palette.
//
// The handler stitches together the SIN-63079 palette-extractor adapter
// (newPaletteExtractor — see branding_wire.go) and an in-memory
// PaletteStore/PaletteWriter. The Postgres-backed adapter against the
// tenant_palette table (SIN-63075) lands separately; until then a
// per-process singleton memstore keeps the save / revert / read flows
// end-to-end functional. SIN-63101 wires that same singleton through
// the SIN-63085 ThemeMiddleware so reads from the middleware and writes
// from the admin handler share state — without this, every authenticated
// page falls back to DefaultThemeStyle and AC #1 / AC #4 stay broken.
//
// Returns a brandingStack with nil fields and a no-op cleanup when the
// constructor fails — the in-memory store has no external dependencies,
// so the handler is always wired when the boot reaches this point.

import (
	"log"
	"log/slog"
	"net/http"
	"time"

	memstoreadapter "github.com/pericles-luz/crm/internal/adapter/branding/memstore"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	webbranding "github.com/pericles-luz/crm/internal/web/branding"
)

// brandingStack bundles the branding-admin HTTP handler with the
// per-tenant theme middleware that consumes the same PaletteStore. Both
// fields share the singleton memstore so a SIN-63084 palette save is
// immediately visible to the next theme-middleware lookup (AC #4 of
// SIN-63085 / SIN-63101).
//
// Theme is non-nil whenever Handler is non-nil — the two are built off
// the same store in the same scope. Cleanup is a no-op today (the
// in-memory adapter has no external client) but the slot stays for
// orthogonality with the other web/* wires.
type brandingStack struct {
	Handler http.Handler
	Theme   *middleware.ThemeMiddleware
	Cleanup func()
}

// buildBrandingStack assembles the SIN-63084 admin handler and the
// SIN-63085 theme middleware over a shared in-memory PaletteStore. The
// metrics argument is the obs.Metrics implementation of
// middleware.ThemeMetrics — pass nil when no metrics are wired (the
// middleware records no-op observations rather than panicking).
//
// The returned Handler is the *http.ServeMux that routes every
// /branding* surface, ready to be threaded into iamHandlerOpts.
func buildBrandingStack(logger *slog.Logger, metrics middleware.ThemeMetrics) brandingStack {
	noop := brandingStack{Cleanup: func() {}}
	if logger == nil {
		logger = slog.Default()
	}
	store := memstoreadapter.New()
	themeMW := middleware.NewTheme(middleware.ThemeConfig{
		Store:   store,
		TTL:     middleware.DefaultThemeCacheTTL,
		Now:     time.Now,
		Metrics: metrics,
	})
	handler, err := webbranding.New(webbranding.Deps{
		Extractor:  newPaletteExtractor(logger),
		Store:      store,
		Writer:     store,
		CSRFToken:  csrfTokenFromSessionContext,
		ThemeCache: themeMW,
		Logger:     logger,
	})
	if err != nil {
		log.Printf("crm: web/branding handler disabled — %v", err)
		return noop
	}
	mux := http.NewServeMux()
	handler.Routes(mux)
	log.Printf("crm: web/branding HTMX routes mounted on public listener")
	log.Printf("crm: theme middleware wired (cache TTL %s)", middleware.DefaultThemeCacheTTL)
	return brandingStack{
		Handler: mux,
		Theme:   themeMW,
		Cleanup: func() {},
	}
}

