package main

// SIN-63084 wiring — HTMX branding admin UI for Fase 5.
//
// The handler stitches together the SIN-63079 palette-extractor adapter
// (newPaletteExtractor — see branding_wire.go) and an in-memory
// PaletteStore/PaletteWriter. The Postgres-backed adapter against the
// tenant_palette table (SIN-63075) lands separately; until then a
// per-process singleton memstore keeps the save / revert / read flows
// end-to-end functional. The same singleton is intended to back the
// SIN-63085 theme middleware once that wire is enabled in main, so
// reads from the middleware and writes from this handler share state.
//
// Returns (nil, no-op) only when the constructor fails — the in-memory
// store has no external dependencies, so the handler is always wired
// when the boot reaches this point.

import (
	"log"
	"log/slog"
	"net/http"

	memstoreadapter "github.com/pericles-luz/crm/internal/adapter/branding/memstore"
	webbranding "github.com/pericles-luz/crm/internal/web/branding"
)

// buildWebBrandingHandler assembles the HTMX branding mux. The cleanup
// closure is a no-op today (no pgxpool or redis client opened); the
// signature stays consistent with the rest of the web/* wires so the
// main.go boot path can defer it unconditionally.
func buildWebBrandingHandler(logger *slog.Logger) (http.Handler, func()) {
	noop := func() {}
	if logger == nil {
		logger = slog.Default()
	}
	store := memstoreadapter.New()
	handler, err := webbranding.New(webbranding.Deps{
		Extractor: newPaletteExtractor(logger),
		Store:     store,
		Writer:    store,
		CSRFToken: csrfTokenFromSessionContext,
		Logger:    logger,
	})
	if err != nil {
		log.Printf("crm: web/branding handler disabled — %v", err)
		return nil, noop
	}
	mux := http.NewServeMux()
	handler.Routes(mux)
	log.Printf("crm: web/branding HTMX routes mounted on public listener")
	return mux, noop
}
