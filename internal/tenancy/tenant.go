// Package tenancy is the domain layer for tenant identity. It defines the
// Tenant aggregate, the Resolver port that maps an HTTP host to a tenant,
// and the helpers that ferry the resolved tenant through request contexts.
//
// This package MUST stay free of database, HTTP, or other infrastructure
// imports. Concrete adapters live in internal/adapter (the postgres
// resolver) and internal/adapter/httpapi/middleware (the TenantScope
// middleware). The split exists because tenant resolution is the FIRST of
// three defense layers (middleware → WithTenant → RLS): keeping the port
// pure makes it trivial to mock in tests for the upper layers.
package tenancy

import "github.com/google/uuid"

// Tenant is the aggregate the rest of the codebase refers to once a
// request has been associated with a customer. id is the uuid persisted
// on every tenanted row; host is the customer-facing hostname that
// resolved to this tenant (subdomain or custom domain).
type Tenant struct {
	ID   uuid.UUID
	Name string
	Host string
}
