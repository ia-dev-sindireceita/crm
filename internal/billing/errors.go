// Package billing is the subscription and invoicing domain (SIN-62877 / Fase 2.5 C3).
//
// The package owns Plan (global catalogue), Subscription (one active per tenant),
// and Invoice (monthly billing rows) domain types, their state-machine transitions,
// and the persistence ports that adapters must implement.
//
// Domain code MUST stay free of database/sql, pgx, net/http, and PSP SDK imports.
// Storage lives behind PlanCatalog, SubscriptionRepository, and InvoiceRepository;
// the Postgres adapter in internal/adapter/db/postgres/billing is the only blessed
// implementation.
package billing

import "errors"

// ErrInvalidTransition is returned by state-machine methods (Cancel, MarkPaid,
// CancelByMaster) when the receiver is not in a compatible state, and also by
// constructors that receive logically inconsistent arguments (e.g. period end
// before start, nil plan ID).
var ErrInvalidTransition = errors.New("billing: invalid state transition")

// ErrInvoiceAlreadyExists is returned by InvoiceRepository when a non-cancelled
// invoice already exists for (tenant_id, period_start). Adapters MUST translate
// the unique-violation on the partial index invoice_tenant_period_active_idx
// (WHERE state <> 'cancelled_by_master') to this sentinel.
var ErrInvoiceAlreadyExists = errors.New("billing: invoice already exists for this period")

// ErrNotFound is returned by repositories when the requested row does not exist.
// Adapters MUST translate "no rows" to this sentinel so callers can match with
// errors.Is without importing pgx.
var ErrNotFound = errors.New("billing: not found")

// ErrZeroTenant is returned when uuid.Nil is passed where a tenant id is required.
var ErrZeroTenant = errors.New("billing: tenant id must not be uuid.Nil")

// ErrCancelReasonTooShort is returned by Invoice.CancelByMaster when the supplied
// reason is shorter than 10 characters. The 10-character floor matches the CHECK
// constraint in the invoice table (migration 0097) so a domain rejection always
// produces a more actionable message than a database constraint violation.
var ErrCancelReasonTooShort = errors.New("billing: cancel reason must be at least 10 characters")
