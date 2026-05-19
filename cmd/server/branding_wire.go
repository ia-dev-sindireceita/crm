package main

// SIN-63079 wiring — branding.PaletteExtractor adapter for Fase 5
// white-label. The factory returns a singleton boring-tech
// (cenkalti/dominantcolor) implementation per ADR 0060. cmd/server holds
// no handler that calls Extract yet; the logo-upload UI handler in
// SIN-63084 consumes this constructor when it lands. Keeping the wire
// stub here today avoids reintroducing it on the next ticket and gives
// SIN-63084 a stable injection point.
//
// The extractor is stateless and goroutine-safe — the median-cut (k-means
// with fixed seed) lib has no per-request state and the slot-selection
// code holds nothing across calls — so a single process-wide instance is
// safe to share across handlers.

import (
	"log/slog"

	mediancutadapter "github.com/pericles-luz/crm/internal/adapter/branding/mediancut"
	"github.com/pericles-luz/crm/internal/branding"
)

// newPaletteExtractor returns the production palette extractor wired
// with the application logger. The logger may be nil (constructor
// defensively no-ops nil), in which case the adapter discards structured
// records.
func newPaletteExtractor(logger *slog.Logger) branding.PaletteExtractor {
	return mediancutadapter.New(mediancutadapter.WithLogger(logger))
}
