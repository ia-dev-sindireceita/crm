// Package handler hosts the HTTP request handlers for the CRM httpapi
// adapter. Handlers are pure: every external dependency arrives via
// constructor parameters or an explicit ports interface; nothing reaches
// for a global resolver, db, or filesystem. This keeps each handler
// trivially substitutable in tests and forces wireup decisions to live in
// cmd/server.
package handler

import (
	"encoding/json"
	"net/http"
)

// healthResponse is the JSON shape returned by /health. commit_sha is the
// build-time identifier injected via -ldflags into internal/version; the
// staging smoke gate (cd-stg.yml) compares it against the GitHub workflow
// head SHA to detect a stale `docker compose pull` (the symptom that
// triggered SIN-63146).
//
// inbox_channel_provider is opt-in via WithInboxChannelProvider (SIN-63825
// / SIN-63793 W6). When set, it surfaces the resolved INBOX_CHANNEL_PROVIDER
// value so the staging smoke can refuse to proceed against a misconfigured
// deploy ("disabled" or unset) without SSH access to the boot log.
// `omitempty` keeps the legacy JSON shape unchanged for callers that do
// not wire the option.
// surfaces (SIN-64985) is an opt-in map of web-surface name → mounted
// boolean, set via WithSurfaces. Each entry mirrors a router.go
// `if deps.WebX != nil` mount gate, so an operator can `curl /health`
// and tell whether a surface silently failed to wire (a fail-soft
// `build*Handler` that returns nil makes the router skip the mount →
// 404 indistinguishable from a route that never existed). The map value
// is `bool` by type, never an error string: "mounted | not" is the
// information ceiling — exposing the wire failure reason would leak
// DSN / infra detail on an unauthenticated endpoint. Whether a surface
// is mounted is already inferable by an unauthenticated HTTP probe, so
// the boolean does not widen the attack surface. `omitempty` keeps the
// legacy JSON shape unchanged for callers that do not wire the option.
type healthResponse struct {
	Status               string          `json:"status"`
	CommitSHA            string          `json:"commit_sha"`
	InboxChannelProvider string          `json:"inbox_channel_provider,omitempty"`
	Surfaces             map[string]bool `json:"surfaces,omitempty"`
}

// HealthOption tunes the healthResponse rendered by Health. Options are
// applied in order on the configured response struct before the
// constructor seals the closure.
type HealthOption func(*healthResponse)

// WithInboxChannelProvider sets the inbox_channel_provider JSON field
// on /health. cmd/server passes the validated INBOX_CHANNEL_PROVIDER
// value (disabled / llmcustomer / real) so the staging smoke
// (scripts/ci/stg-smoke-inbox.sh) can pre-check the boot config without
// reading the container log. The empty string disables the option and
// the field is omitted from the response.
func WithInboxChannelProvider(name string) HealthOption {
	return func(resp *healthResponse) {
		resp.InboxChannelProvider = name
	}
}

// WithSurfaces sets the surfaces JSON field on /health (SIN-64985) — a
// map of web-surface name → mounted boolean. cmd/server passes the map
// derived from Deps.WebSurfaces() so an operator can diagnose a
// silently-nil surface (router skips the mount → bare 404) with a single
// `curl /health`, no container-log access. Only booleans cross the
// boundary; the wire failure reason is never exposed (see healthResponse
// doc). An empty or nil map omits the field and keeps the legacy shape.
// A populated map renders even when every value is false — that is the
// whole point of the diagnostic.
func WithSurfaces(surfaces map[string]bool) HealthOption {
	return func(resp *healthResponse) {
		if len(surfaces) == 0 {
			resp.Surfaces = nil
			return
		}
		// Defensive copy: the caller's map (Deps-derived, potentially
		// shared) must not be aliased into the sealed closure.
		cp := make(map[string]bool, len(surfaces))
		for k, v := range surfaces {
			cp[k] = v
		}
		resp.Surfaces = cp
	}
}

// Health returns the liveness response for the load balancer / k8s probe.
// Mounted OUTSIDE the tenant-scope and auth chains: it must answer 200
// even when the database is down or the host is unrecognised, otherwise
// the LB removes the pod and we lose visibility into the failure.
//
// The handler is a closure constructor — cmd/server injects the commit
// SHA at wireup time so the function stays pure (no os.Getenv, no
// version package import inside the handler body). Empty strings fall
// back to "unknown" so JSON consumers never see an empty field that they
// might mistake for "container is still starting".
//
// Optional HealthOption values (WithInboxChannelProvider, etc.) extend
// the JSON body without breaking pre-W6 callers that pass only the SHA.
func Health(commitSHA string, opts ...HealthOption) http.HandlerFunc {
	sha := commitSHA
	if sha == "" {
		sha = "unknown"
	}
	resp := healthResponse{Status: "ok", CommitSHA: sha}
	for _, opt := range opts {
		opt(&resp)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
