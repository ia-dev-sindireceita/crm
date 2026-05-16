// Package catalog is the per-tenant product catalogue domain
// (SIN-62902 / Fase 3 W2B).
//
// The package owns Product (a billable item) and ProductArgument (a
// per-scope selling pitch attached to a product), the cascade resolver
// that picks the most-specific argument for a given scope, and the
// persistence ports that adapters must implement.
//
// Domain code MUST stay free of database/sql, pgx, net/http, and
// vendor SDK imports. Storage lives behind ProductRepository and
// ArgumentRepository; the Postgres adapter in
// internal/adapter/db/postgres/catalog is the only blessed
// implementation.
package catalog

import "errors"

// ErrInvalidProduct is returned by NewProduct when the caller passes
// arguments that violate the domain invariants (empty name, negative
// price, uuid.Nil tenant id). Constructors return this sentinel so
// callers can match with errors.Is without importing the package's
// private validation rules.
var ErrInvalidProduct = errors.New("catalog: invalid product")

// ErrInvalidArgument is returned by NewProductArgument when the caller
// passes arguments that violate the domain invariants (empty argument
// text, blank scope id, unknown scope type, uuid.Nil tenant or product
// id).
var ErrInvalidArgument = errors.New("catalog: invalid product argument")

// ErrInvalidScope is returned when a Scope value is malformed (unknown
// type or blank id). The resolver rejects malformed scopes up-front so
// callers can't accidentally widen visibility by passing zero-value
// scope structs.
var ErrInvalidScope = errors.New("catalog: invalid scope")

// ErrNotFound is returned by repositories when the requested row does
// not exist. Adapters MUST translate "no rows" to this sentinel so
// callers can match with errors.Is without importing pgx.
var ErrNotFound = errors.New("catalog: not found")

// ErrZeroTenant is returned when uuid.Nil is passed where a tenant id
// is required.
var ErrZeroTenant = errors.New("catalog: tenant id must not be uuid.Nil")

// ErrDuplicateArgument is returned by ArgumentRepository.SaveArgument
// when the (tenant_id, product_id, scope_type, scope_id) tuple
// already has a row. The adapter MUST translate the
// product_argument_product_scope_uniq violation (23505) into this
// sentinel so callers see a domain-level error.
var ErrDuplicateArgument = errors.New("catalog: argument already exists for this scope")
