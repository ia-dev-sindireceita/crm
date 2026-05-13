package tenancy

import (
	"context"
	"errors"
)

// ErrNoTenantInContext signals that the request never went through the
// TenantScope middleware, or that a handler is being invoked outside an
// HTTP request. Treat it as a programmer error: every tenanted route
// MUST mount the middleware first.
var ErrNoTenantInContext = errors.New("tenancy: no tenant in context")

// tenantCtxKey is the unexported context-key type required by the
// context package's contract; using an unexported type prevents
// accidental key collisions with other packages.
type tenantCtxKey struct{}

// WithContext returns a derived context that carries tenant. The
// middleware uses this; handlers reach for FromContext to read it back.
func WithContext(ctx context.Context, tenant *Tenant) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenant)
}

// FromContext returns the Tenant attached by TenantScope. ErrNoTenantInContext
// indicates the middleware was skipped — handlers should not paper over
// this with a default tenant.
func FromContext(ctx context.Context) (*Tenant, error) {
	t, ok := ctx.Value(tenantCtxKey{}).(*Tenant)
	if !ok || t == nil {
		return nil, ErrNoTenantInContext
	}
	return t, nil
}
