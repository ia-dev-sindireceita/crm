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
type healthResponse struct {
	Status    string `json:"status"`
	CommitSHA string `json:"commit_sha"`
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
func Health(commitSHA string) http.HandlerFunc {
	sha := commitSHA
	if sha == "" {
		sha = "unknown"
	}
	resp := healthResponse{Status: "ok", CommitSHA: sha}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
