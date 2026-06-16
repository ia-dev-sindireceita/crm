package main

// SIN-64974 wiring — HTMX PIX-invoice surface (Fase 4, SIN-62963).
// Mounts the routes under /billing/invoices with the SIN-62956 /
// SIN-62957 postgres adapters backing the InvoiceLister + InvoiceGetter
// ports and the SIN-62880 dunning adapter backing the DunningStateReader
// port.
//
// Two pgxpools are required: the runtime pool (app_runtime, RLS gated)
// serves the tenant-scoped reads, and the master_ops pool
// (app_master_ops) backs the adapters' write/audit paths. When either
// pool URL is unset or unreachable the wire returns a nil handler and
// the router leaves the /billing/invoices routes unmounted — the same
// fail-soft pattern as buildWebCatalogHandler.
//
// The PIXChargeLister port (the QR / copia-e-cola block on the detail
// page) is satisfied by the documented "no-charge" placeholder until
// the PIX postgres read adapter (LatestForInvoice) lands in
// SIN-62958 / C7; see internal/web/billing/invoices/doc.go. The list
// page — the surface the staging probe (SIN-64964) found 404'ing — has
// no PIX dependency, so the placeholder does not gate the fix.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgbilling "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	pgdunning "github.com/pericles-luz/crm/internal/adapter/db/postgres/dunning"
	billingpix "github.com/pericles-luz/crm/internal/billing/pix"
	webinvoices "github.com/pericles-luz/crm/internal/web/billing/invoices"
)

// buildWebBillingInvoicesHandler returns the PIX-invoice mux + a cleanup
// closure that releases the two pgxpools the wire opened. A nil handler
// signals "skip mounting on the public listener" so callers can defer
// the cleanup unconditionally.
func buildWebBillingInvoicesHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	masterDSN := getenv(envMasterOpsDSN)
	if dsn == "" {
		log.Printf("crm: web/billing/invoices disabled — DATABASE_URL unset")
		return nil, noop
	}
	if masterDSN == "" {
		log.Printf("crm: web/billing/invoices disabled — %s unset", envMasterOpsDSN)
		return nil, noop
	}
	runtime, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/billing/invoices disabled — pg runtime connect: %v", err)
		return nil, noop
	}
	master, err := pgpool.New(ctx, masterDSN)
	if err != nil {
		runtime.Close()
		log.Printf("crm: web/billing/invoices disabled — pg master_ops connect: %v", err)
		return nil, noop
	}
	billingStore, err := pgbilling.New(runtime, master)
	if err != nil {
		runtime.Close()
		master.Close()
		log.Printf("crm: web/billing/invoices disabled — billing store: %v", err)
		return nil, noop
	}
	dunningStore, err := pgdunning.New(runtime, master)
	if err != nil {
		runtime.Close()
		master.Close()
		log.Printf("crm: web/billing/invoices disabled — dunning store: %v", err)
		return nil, noop
	}
	handler, err := assembleWebBillingInvoicesHandler(
		billingStore, billingStore, noChargeLister{}, dunningStore, slog.Default(),
	)
	if err != nil {
		runtime.Close()
		master.Close()
		log.Printf("crm: web/billing/invoices disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/billing/invoices HTMX routes mounted (billing + dunning adapters wired; PIX charge = no-charge placeholder)")
	return handler, func() {
		runtime.Close()
		master.Close()
	}
}

// assembleWebBillingInvoicesHandler is the pure assembly seam. Tests
// call it directly with stub deps so the wire is exercised without
// booting the whole server. It mirrors webinvoices.New's required-dep
// checks at the composition root so a nil adapter fails fast with a
// wire-scoped error rather than a nil-pointer panic at request time.
func assembleWebBillingInvoicesHandler(
	invoices webinvoices.InvoiceLister,
	invoice webinvoices.InvoiceGetter,
	charges webinvoices.PIXChargeLister,
	dunning webinvoices.DunningStateReader,
	logger *slog.Logger,
) (http.Handler, error) {
	if invoices == nil {
		return nil, errors.New("billing_invoices_wire: invoices is nil")
	}
	if invoice == nil {
		return nil, errors.New("billing_invoices_wire: invoice is nil")
	}
	if charges == nil {
		return nil, errors.New("billing_invoices_wire: charges is nil")
	}
	if dunning == nil {
		return nil, errors.New("billing_invoices_wire: dunning is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := webinvoices.New(webinvoices.Deps{
		Invoices:  invoices,
		Invoice:   invoice,
		Charges:   charges,
		Dunning:   dunning,
		CSRFToken: csrfTokenFromSessionContext,
		UserID:    userIDFromSessionContext,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("billing_invoices_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// noChargeLister is the documented "no-charge" placeholder for the
// PIXChargeLister port. The PIX postgres read adapter (LatestForInvoice)
// lands in SIN-62958 / C7; until then every lookup resolves to
// pix.ErrNotFound and the detail page renders the "cobrança em
// processamento" placeholder (see internal/web/billing/invoices/doc.go).
type noChargeLister struct{}

func (noChargeLister) LatestForInvoice(_ context.Context, _, _ uuid.UUID) (*billingpix.PIXCharge, error) {
	return nil, billingpix.ErrNotFound
}

// Compile-time guards: the pgx adapters satisfy the web ports the wire
// consumes, and the placeholder satisfies the charge-read port.
var (
	_ webinvoices.InvoiceLister      = (*pgbilling.Store)(nil)
	_ webinvoices.InvoiceGetter      = (*pgbilling.Store)(nil)
	_ webinvoices.DunningStateReader = (*pgdunning.Store)(nil)
	_ webinvoices.PIXChargeLister    = noChargeLister{}
)
