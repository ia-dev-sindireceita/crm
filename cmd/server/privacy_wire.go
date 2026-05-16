package main

// SIN-62354 wiring — HTMX privacy / DPA page (Fase 3, decisão #8 /
// SIN-62203).
//
// buildWebPrivacyHandler assembles the read-only privacy disclosure
// page. The wire is dependency-light by design: today the active-AI-
// model lookup falls back to "openrouter/auto" (the migration 0098
// default), because the internal/aipolicy cascade resolver lives in
// SIN-62351 which is still landing. When that ships, only this file
// changes: swap StaticModelResolver for an aipolicy-backed adapter
// and the privacy handler renders the real per-tenant value with no
// other surface area changes.
//
// Unlike the IAM/funnel wires, this one does NOT touch the database
// today, so it always returns a non-nil handler — there is no
// fail-soft branch keyed on DATABASE_URL. Privacy disclosure is a
// release-blocking LGPD requirement; we never let the page disappear
// because an unrelated dependency is misconfigured.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	webprivacy "github.com/pericles-luz/crm/internal/web/privacy"
)

// buildWebPrivacyHandler returns the privacy mux. Cleanup is a noop
// today because no resources are owned by the wire; the signature
// matches the other web-* wires so the registration site in main.go
// stays uniform.
func buildWebPrivacyHandler(_ context.Context, _ func(string) string) (http.Handler, func()) {
	resolver := staticModelResolver{model: webprivacy.FallbackModel}
	handler, err := assembleWebPrivacyHandler(resolver, time.Now, slog.Default())
	if err != nil {
		// The only way New returns an error here is the
		// DPAMentionsOpenRouter invariant failing — i.e. someone
		// shipped a broken DPA. Refuse to serve the privacy page
		// in that case so CI / smoke catches it loudly.
		log.Printf("crm: web/privacy handler disabled — %v", err)
		return nil, func() {}
	}
	log.Printf("crm: web/privacy HTMX routes mounted on public listener")
	return handler, func() {}
}

// assembleWebPrivacyHandler is the pure assembly seam. Tests call it
// directly with stub deps so the wire is exercised without booting
// the whole server.
func assembleWebPrivacyHandler(
	resolver webprivacy.ModelResolver,
	now webprivacy.Now,
	logger *slog.Logger,
) (http.Handler, error) {
	if resolver == nil {
		return nil, errors.New("privacy_wire: resolver is nil")
	}
	if now == nil {
		return nil, errors.New("privacy_wire: now is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := webprivacy.New(webprivacy.Deps{
		Model:  resolver,
		Now:    now,
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("privacy_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// staticModelResolver returns a single hard-coded model string. Used
// at boot until the SIN-62351 aipolicy cascade resolver lands. The
// value matches migration 0098's ai_policy.model default so the page
// renders the same string a fresh tenant would see if they queried
// the DB directly.
type staticModelResolver struct {
	model string
}

func (s staticModelResolver) ActiveModel(_ context.Context, _ uuid.UUID) (string, error) {
	return s.model, nil
}
