package main

// SIN-63942 / UX-F5 wiring — HTMX gerente wallet UI. Mounts the four
// /wallet* routes (dashboard, topup catalogue, paginated ledger, CSV
// export) on top of the SIN-63954 walletui adapter (DashboardReader +
// LedgerReader + TopupCatalogReader) and the shared CSRF / session
// helpers.
//
// Returns (nil, no-op) when DATABASE_URL is unset so cmd/server keeps
// booting cleanly in health-only / smoke modes — the same fail-soft
// pattern the funnel / catalog / campaigns / inbox wires use.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgwalletui "github.com/pericles-luz/crm/internal/adapter/db/postgres/walletui"
	webwalletui "github.com/pericles-luz/crm/internal/web/walletui"
)

// buildWalletUIHandler returns the wallet HTMX mux + a cleanup closure
// that releases the pgxpool. cmd/server's main wires the mux into
// httpapi.Deps.WebWallet so the chi router wraps it with RequireAuth +
// RequireAction(iam.ActionTenantWalletViewLedger) before dispatch.
func buildWalletUIHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: web/walletui disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/walletui disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgwalletui.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/walletui disabled — store: %v", err)
		return nil, noop
	}
	handler, err := assembleWalletUIHandler(store, store, store, time.Now, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/walletui disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/walletui HTMX routes mounted on public listener")
	return handler, func() { pool.Close() }
}

// assembleWalletUIHandler is the pure assembly seam. Tests call it
// with in-process fakes that satisfy the three walletui ports so the
// wire exercises composition without booting Postgres.
func assembleWalletUIHandler(
	dashboard webwalletui.DashboardReader,
	ledger webwalletui.LedgerReader,
	topup webwalletui.TopupCatalogReader,
	now func() time.Time,
	logger *slog.Logger,
) (http.Handler, error) {
	if dashboard == nil {
		return nil, errors.New("walletui_wire: dashboard reader is nil")
	}
	if ledger == nil {
		return nil, errors.New("walletui_wire: ledger reader is nil")
	}
	if topup == nil {
		return nil, errors.New("walletui_wire: topup reader is nil")
	}
	if now == nil {
		return nil, errors.New("walletui_wire: now is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := webwalletui.New(webwalletui.Deps{
		Dashboard: dashboard,
		Ledger:    ledger,
		Topup:     topup,
		CSRFToken: csrfTokenFromSessionContext,
		UserLabel: walletUserLabelFromSession,
		Now:       func() time.Time { return now().UTC() },
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("walletui_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// walletUserLabelFromSession returns the user-menu display label. The
// wallet handler defaults to "Conta" via shell when this returns
// empty, which is the right behaviour pre-SIN-63985 — the project does
// not yet expose a display name on the session payload. Wiring the
// closure here keeps the upgrade seam in one place.
func walletUserLabelFromSession(*http.Request) string { return "" }

// Compile-time guards: the pg adapter satisfies every walletui port
// the wire layer hands the handler.
var (
	_ webwalletui.DashboardReader    = (*pgwalletui.Store)(nil)
	_ webwalletui.LedgerReader       = (*pgwalletui.Store)(nil)
	_ webwalletui.TopupCatalogReader = (*pgwalletui.Store)(nil)
)
