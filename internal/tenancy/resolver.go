package tenancy

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Resolver maps an incoming HTTP host (acme.crm.local, acme.com.br, …) to
// the Tenant that should scope the request. Implementations live behind
// the port so tests and the middleware never depend on the database.
//
// Implementations MUST return ErrTenantNotFound — and only that error —
// when the host is not registered. Anything else (timeouts, transport
// errors, etc.) is treated as a server error by the middleware.
type Resolver interface {
	ResolveByHost(ctx context.Context, host string) (*Tenant, error)
}

// ByIDResolver looks up a Tenant by its uuid. Used by the master
// impersonation middleware (SIN-62219) to swap the request's tenant
// scope to a target id supplied via the X-Impersonate-Tenant header.
//
// The port is separate from Resolver so that callers which only need
// host→tenant lookups (TenantScope, login flow) do not have to satisfy
// the by-id contract. Implementations MUST return ErrTenantNotFound for
// an unknown id and a wrapped infra error for everything else.
type ByIDResolver interface {
	ResolveByID(ctx context.Context, id uuid.UUID) (*Tenant, error)
}

// ErrTenantNotFound signals that no tenant matches the supplied host.
// Callers MUST translate this into a generic 404 response — leaking the
// fact that some subdomains DO resolve enables tenant enumeration.
var ErrTenantNotFound = errors.New("tenancy: tenant not found")
