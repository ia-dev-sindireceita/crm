// Package middleware hosts the chi/net-http middlewares that wrap the
// CRM HTTP routes. TenantScope is the FIRST link in the defense chain
// (middleware → WithTenant → RLS) so a 404 here means no tenanted code
// downstream ever sees an unrecognised host.
package middleware

import (
	"errors"
	"net/http"

	"github.com/pericles-luz/crm/internal/tenancy"
)

// genericNotFoundBody is intentionally fixed and uninformative. Leaking
// the difference between "we serve this domain but not this subdomain"
// and "we have no record of this host" lets attackers enumerate
// tenants. 404 with a generic body for both cases denies that signal.
const genericNotFoundBody = "Not Found\n"

// TenantScope returns a middleware that resolves r.Host into a Tenant
// (using the supplied resolver) and stores it on the request context
// for downstream handlers.
//
// Flow:
//  1. Read r.Host.
//  2. Call resolver.ResolveByHost.
//  3. On success, attach the tenant via tenancy.WithContext.
//  4. On tenancy.ErrTenantNotFound, write a generic 404.
//  5. On any other error (DB blip, timeout), write a 500 — that case is
//     not a security signal, so we don't bother hiding it.
//
// Passing a nil resolver panics at request time rather than wiring time;
// the deny-by-default behaviour means a misconfigured router fails
// loudly on the first request.
func TenantScope(resolver tenancy.Resolver) func(http.Handler) http.Handler {
	if resolver == nil {
		panic("middleware: TenantScope resolver is nil")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, err := resolver.ResolveByHost(r.Context(), r.Host)
			if err != nil {
				if errors.Is(err, tenancy.ErrTenantNotFound) {
					writeGenericNotFound(w)
					return
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			ctx := tenancy.WithContext(r.Context(), tenant)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeGenericNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(genericNotFoundBody))
}
