package tenancy

import (
	"errors"
	"net"
	"strings"
)

// ErrEmptyHost is returned by ParseHost when the input is the empty
// string. Routers normally fill this in for us, but defending in depth
// is cheap.
var ErrEmptyHost = errors.New("tenancy: host is empty")

// ErrNoTenantSubdomain is returned by ParseHost when the host has no
// leading subdomain (e.g. `crm.example.com`). The middleware treats it
// the same as a not-found tenant — both produce a 404.
//
// Custom-domain hosts (e.g. `acme.com.br`) do not trigger this error:
// ParseHost returns subdomain="" and root=host, leaving it to the
// Resolver to decide whether the host is registered as a custom domain
// (decisão #6/#17 do plan; self-service em Fase 5).
var ErrNoTenantSubdomain = errors.New("tenancy: host has no tenant subdomain")

// platformRootSuffixes is the set of suffixes the platform itself owns.
// A host whose root matches one of these MUST carry a tenant subdomain
// (e.g. acme.crm.local). Other hosts are treated as candidate
// custom-domain lookups; the resolver decides.
//
// Entries are matched against the trailing labels of the host, so
// "crm.local" matches both "crm.local" and "anything.crm.local". Update
// this list rather than ParseHost when a new platform domain is added
// (e.g. crm.example.org for staging).
var platformRootSuffixes = []string{
	"crm.local",
	"crm.example.com",
}

// ParseHost splits an HTTP Host header into (subdomain, root) for tenant
// resolution. Port suffixes (":443", ":8080") are stripped.
//
// Behaviour:
//   - "acme.crm.local"          → ("acme",   "crm.local",        nil)
//   - "globex.crm.example.com"  → ("globex", "crm.example.com",  nil)
//   - "crm.local"               → ("",       "crm.local",        ErrNoTenantSubdomain)
//   - "acme.com.br"             → ("",       "acme.com.br",      nil) — custom domain candidate
//   - ""                        → ("",       "",                  ErrEmptyHost)
//
// Custom-domain hosts return a nil error so the resolver gets a chance
// to look them up directly in the tenant table; a 404 happens later
// when the lookup misses.
func ParseHost(host string) (subdomain, root string, err error) {
	if host == "" {
		return "", "", ErrEmptyHost
	}
	// SplitHostPort fails on bare hostnames; fall back to the input on
	// error so we accept both "acme.crm.local" and "acme.crm.local:8080".
	if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")

	for _, suffix := range platformRootSuffixes {
		if host == suffix {
			return "", suffix, ErrNoTenantSubdomain
		}
		if strings.HasSuffix(host, "."+suffix) {
			return strings.TrimSuffix(host, "."+suffix), suffix, nil
		}
	}
	// Not a platform host: treat the whole thing as a custom-domain
	// candidate. The resolver will translate a miss into ErrTenantNotFound.
	return "", host, nil
}
